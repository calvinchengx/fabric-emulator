"""Real-Time Intelligence: REAL KQL execution behind the Eventhouse surface.

Nothing here is emulated arithmetic. The witness drives the emulator's Fabric
control plane to create an eventhouse, reads `properties.queryServiceUri` off
it (the documented discovery step — fabric-docs
real-time-intelligence/eventhouse-deploy-with-fabric-api.md), and then speaks
the *Kusto* REST protocol to that URI:

    POST {queryServiceUri}/v1/rest/mgmt   {"db": "...", "csl": ".create-merge table ..."}
    POST {queryServiceUri}/v1/rest/query  {"db": "...", "csl": "Readings | count"}

Microsoft's own KQL engine container (kustainer) computes every one of those.
The emulator terminates the contract — Entra bearer on the Kusto audience,
workspace RBAC, and one isolated engine database per Fabric KQL Database —
and relays the KQL itself.

Two independent clients are asserted:
  * raw REST, the wire contract itself;
  * `azure-kusto-data`, Microsoft's real Kusto SDK, which builds its own
    endpoints from the cluster URI and parses the v2 frame stream.
"""
import base64
import json
import os
import time
import urllib.parse
import urllib.request

import requests
from azure.kusto.data import ClientRequestProperties, KustoClient, KustoConnectionStringBuilder

FABRIC = os.environ.get("FABRIC", "http://fabric-emulator")
ENTRA = os.environ.get("ENTRA", "http://entra-emulator:8443")
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
KUSTO_AUDIENCE = "https://kusto.fabric.microsoft.com"


def entra_token(scope):
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": CLIENT_ID,
        "client_secret": "daemon-app-secret",
        "scope": scope,
    }).encode()
    req = urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form)
    with urllib.request.urlopen(req) as r:
        return json.loads(r.read())["access_token"]


def forged_token(audience):
    """entra-emulator's admin mint, for an audience whose resource principal
    the seeded directory does not carry (the same escape hatch e2e/delta-rs
    uses for the Storage audience). Still a real signed JWT over real JWKS —
    only the resource registration is skipped."""
    body = json.dumps({"clientId": CLIENT_ID, "audience": audience}).encode()
    req = urllib.request.Request(f"{ENTRA}/admin/api/tokens", data=body,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req) as r:
        payload = json.loads(r.read())
    return payload.get("access_token") or payload["token"]


fabric_token = entra_token("https://api.fabric.microsoft.com/.default")
kusto_token = forged_token(KUSTO_AUDIENCE)
FABRIC_HEADERS = {"Authorization": f"Bearer {fabric_token}", "Content-Type": "application/json"}
KUSTO_HEADERS = {"Authorization": f"Bearer {kusto_token}", "Content-Type": "application/json",
                 "Accept": "application/json"}


def fabric_post(path, body):
    r = requests.post(f"{FABRIC}{path}", headers=FABRIC_HEADERS, json=body, timeout=60)
    r.raise_for_status()
    return r.json()


def fabric_get(path):
    r = requests.get(f"{FABRIC}{path}", headers=FABRIC_HEADERS, timeout=60)
    r.raise_for_status()
    return r.json()


def kusto(uri, kind, db, csl, expect=200):
    r = requests.post(f"{uri}/v1/rest/{kind}", headers=KUSTO_HEADERS,
                      json={"db": db, "csl": csl}, timeout=300)
    assert r.status_code == expect, f"{kind} {csl!r} -> {r.status_code} {r.text[:800]}"
    return r.json() if r.content else {}


def primary_rows(payload):
    """Rows of the first v1 table — the shape Microsoft's own docs read."""
    return payload["Tables"][0]["Rows"]


# ---------------------------------------------------------------- control plane
ws = fabric_post("/v1/workspaces", {"displayName": "rti-ws"})
eh = fabric_post(f"/v1/workspaces/{ws['id']}/eventhouses", {"displayName": "telemetry"})
print(f"workspace {ws['id']} / eventhouse {eh['id']}")

eh = fabric_get(f"/v1/workspaces/{ws['id']}/eventhouses/{eh['id']}")
props = eh["properties"]
query_uri = props["queryServiceUri"]
db_ids = props["databasesItemIds"]
assert query_uri, eh
assert db_ids, "an eventhouse must publish its databases, like real Fabric"

# Fabric creates a default KQL database with the eventhouse's own name.
default_db = fabric_get(f"/v1/workspaces/{ws['id']}/kqlDatabases/{db_ids[0]}")
assert default_db["displayName"] == "telemetry", default_db
assert default_db["properties"]["parentEventhouseItemId"] == eh["id"], default_db
print(f"queryServiceUri: {query_uri}  db: {default_db['displayName']}")

# A second, explicitly created database — the creationPayload path.
second = fabric_post(f"/v1/workspaces/{ws['id']}/kqlDatabases", {
    "displayName": "sensors",
    "creationPayload": {"databaseType": "ReadWrite", "parentEventhouseItemId": eh["id"]},
})
assert second["properties"]["parentEventhouseItemId"] == eh["id"], second

DB = "telemetry"

# ------------------------------------------------------------------- create table
kusto(query_uri, "mgmt", DB,
      ".create-merge table Readings (DeviceId:string, Temp:real, At:datetime)")
tables = kusto(query_uri, "mgmt", DB, ".show tables")
names = [row[0] for row in primary_rows(tables)]
assert "Readings" in names, names
print(f"created table on the engine: {names}")

# ---------------------------------------------------------------------- ingest
# Ingest-from-query (`.set-or-append`) is the documented direct-ingestion path
# the engine handles without a data-management service.
kusto(query_uri, "mgmt", DB, """
.set-or-append Readings <|
    datatable (DeviceId:string, Temp:real, At:datetime)
    [
        "dev-1", 21.5, datetime(2026-07-31T00:00:00Z),
        "dev-1", 23.5, datetime(2026-07-31T00:01:00Z),
        "dev-2", 30.0, datetime(2026-07-31T00:02:00Z)
    ]
""")
# …and the inline push command, the other documented direct-ingestion form.
kusto(query_uri, "mgmt", DB, """
.ingest inline into table Readings <|
dev-2,32.0,2026-07-31T00:03:00Z
""")
print("ingested 4 rows")

# ---------------------------------------------------------------------- query
count = primary_rows(kusto(query_uri, "query", DB, "Readings | count"))[0][0]
assert count == 4, f"count = {count}, want 4"

rows = primary_rows(kusto(query_uri, "query", DB,
                          "Readings | summarize Avg=avg(Temp), N=count() by DeviceId | order by DeviceId asc"))
got = {row[0]: (round(float(row[1]), 3), int(row[2])) for row in rows}
assert got == {"dev-1": (22.5, 2), "dev-2": (31.0, 2)}, got
print(f"REAL KQL aggregation: {got}")

# Time filtering and projection — the engine's own datetime semantics.
rows = primary_rows(kusto(query_uri, "query", DB,
                          "Readings | where At >= datetime(2026-07-31T00:01:00Z) "
                          "| project DeviceId, Temp | order by Temp asc"))
assert [[r[0], round(float(r[1]), 3)] for r in rows] == [
    ["dev-1", 23.5], ["dev-2", 30.0], ["dev-2", 32.0]], rows
print("REAL KQL filter/projection: OK")

# ------------------------------------------------------ per-database isolation
# The second Fabric database is a separate engine database: same table name,
# different contents, no crosstalk.
kusto(query_uri, "mgmt", "sensors", ".create-merge table Readings (DeviceId:string, Temp:real)")
kusto(query_uri, "mgmt", "sensors",
      '.set-or-append Readings <| datatable (DeviceId:string, Temp:real) ["dev-9", 99.0]')
assert primary_rows(kusto(query_uri, "query", "sensors", "Readings | count"))[0][0] == 1
assert primary_rows(kusto(query_uri, "query", DB, "Readings | count"))[0][0] == 4
print("per-KQL-database isolation: OK")

# The engine's internal database name must never leak back to a client:
# current_database() reports the database the engine actually ran in, and the
# relay maps it home to the Fabric display name.
current = primary_rows(kusto(query_uri, "query", DB, "print DB=current_database()"))[0][0]
assert current == DB, f"current_database() = {current!r}, want the Fabric name {DB!r}"
print("engine database naming stays internal: OK")

# ------------------------------------------- KQL keywords as column names
# A schema declaration cannot name a column with a BARE KQL keyword: the engine
# answers SYN0002. This matters beyond hand-written KQL because the emulator's
# eventstream -> eventhouse drain builds `.create-merge table T (name:type, …)`
# from whatever fields the events carry (internal/api/eventstream.go,
# kustoIngestTable), and `kind` is an ordinary field name in event data. The
# emitter therefore quotes every column — ['kind'], Kusto's documented remedy —
# and the Go tests police that against a FAKE engine whose keyword list only a
# real one can settle. This is where it gets settled.
#
# The candidates are every keyword Microsoft's identifier-naming rules put in
# reach; REFUSED_BARE is what this engine ACTUALLY refuses, and the two are not
# the same — the rules say a keyword used as an identifier must be quoted, and
# kustainer accepts 27 of these 36 bare anyway (`where` and `summarize` among
# them). Only the nine below are real, which is why the set is asserted rather
# than the list: a keyword drifting in either direction is a fault. Too few and
# a command the engine refuses passes the Go tests, which is the bug this probe
# was written for; too many and the fake refuses KQL kustainer runs, which is
# worse. internal/api/kql_test.go's kqlKeywords must equal REFUSED_BARE.
KQL_KEYWORD_CANDIDATES = [
    "and", "between", "by", "contains", "datatable", "distinct", "endswith",
    "extend", "false", "from", "has", "in", "join", "kind", "let", "limit",
    "not", "null", "on", "or", "order", "parse", "print", "project", "range",
    "set", "sort", "startswith", "summarize", "take", "then", "top", "true",
    "union", "where", "with",
]
REFUSED_BARE = ["and", "between", "false", "kind", "or", "order", "project", "true", "union"]

refused = {}
for kw in KQL_KEYWORD_CANDIDATES:
    bare = requests.post(f"{query_uri}/v1/rest/mgmt", headers=KUSTO_HEADERS,
                         json={"db": DB, "csl": f".create table KwBare_{kw} ({kw}:string)"},
                         timeout=60)
    if bare.status_code != 200:
        refused[kw] = bare.status_code
        # A refusal has to be the SYNTAX error this is about. Anything else
        # (a 5xx, an auth fault) would put a keyword on the list for a reason
        # that has nothing to do with the grammar.
        assert bare.status_code == 400, (kw, bare.status_code, bare.text[:400])
        assert "SYN0002" in bare.text, (kw, bare.text[:800])
assert sorted(refused) == sorted(REFUSED_BARE), (
    "what kustainer refuses as a BARE column name has moved. "
    f"newly refused (add to kqlKeywords): {sorted(set(refused) - set(REFUSED_BARE))}; "
    f"no longer refused (drop from kqlKeywords): {sorted(set(REFUSED_BARE) - set(refused))}")
print(f"{len(refused)} of {len(KQL_KEYWORD_CANDIDATES)} keywords refused bare, "
      f"all SYN0002: {sorted(refused)}")

# …and every candidate is legal QUOTED, refused bare or not — which is why the
# emitter can quote unconditionally instead of carrying the list above.
quoted = ", ".join(f"['{kw}']:string" for kw in KQL_KEYWORD_CANDIDATES)
kusto(query_uri, "mgmt", DB, f".create-merge table KwQuoted ({quoted})")
kusto(query_uri, "mgmt", DB,
      ".ingest inline into table KwQuoted <|\n" + ",".join(["x"] * len(KQL_KEYWORD_CANDIDATES)))
addressable = primary_rows(kusto(query_uri, "query", DB,
                                 "KwQuoted | where ['kind'] == 'x' and ['project'] == 'x' | count"))[0][0]
assert addressable == 1, f"quoted keyword columns are not addressable: {addressable}"
print(f"all {len(KQL_KEYWORD_CANDIDATES)} quoted: table created, row ingested, column queried back")

# The emitter's own output, verbatim: the `.create-merge table` + `.ingest
# inline` pair kustoIngestTable produces for an event carrying `kind` and `n`.
kusto(query_uri, "mgmt", DB,
      ".create-merge table StreamEvents (['kind']:string, ['n']:real, ['At']:string)")
kusto(query_uri, "mgmt", DB,
      ".ingest inline into table StreamEvents <|\nclick,1,2026-07-31T00:00:00Z\nview,2,2026-07-31T00:01:00Z")
drained = primary_rows(kusto(query_uri, "query", DB,
                             "StreamEvents | summarize N=count(), Total=sum(['n'])"))[0]
assert [int(drained[0]), round(float(drained[1]), 3)] == [2, 3.0], drained
print(f"eventstream drain's emitted KQL runs on the real engine: N={drained[0]} Total={drained[1]}")

# …and its unquoted twin does not, which is the whole reason for the quoting.
kusto(query_uri, "mgmt", DB,
      ".create-merge table StreamEventsBare (kind:string, n:real, At:string)", expect=400)
print("the unquoted twin is refused: OK")

# ------------------------------------------- KQL keywords as the TABLE name
# The same defect on the other name in those commands. The drain takes its
# table name from the destination bind, which validates it with the same
# character class — so `{"table": "kind"}` binds, and emits a command the
# engine refuses. Quoting is the same remedy, but the doc that licenses it
# ("entity names") is doing more work here: the column case is now witnessed
# above, while a table name is a different position in the grammar, and this
# is the one place that can settle whether ['name'] is accepted there.
#
# Not through kusto(..., expect=400): this is the assertion nobody has seen an
# answer to, so it records the engine's own status and body in the CI log
# rather than asserting a status and discarding the message.
bare_table = requests.post(f"{query_uri}/v1/rest/mgmt", headers=KUSTO_HEADERS,
                           json={"db": DB, "csl": ".create-merge table kind (a:string)"},
                           timeout=60)
assert bare_table.status_code >= 400, (
    f"the engine ACCEPTED a bare keyword TABLE name ({bare_table.status_code}) — "
    "there is no defect to fix, and the table name should be left bare")
print(f"bare `.create-merge table kind`: refused {bare_table.status_code} "
      f"{bare_table.text[:200]}")

kusto(query_uri, "mgmt", DB, ".create-merge table ['kind'] (a:string)")
kusto(query_uri, "mgmt", DB, ".ingest inline into table ['kind'] <|\nx")
assert "kind" in [row[0] for row in primary_rows(kusto(query_uri, "mgmt", DB, ".show tables"))]
assert primary_rows(kusto(query_uri, "query", DB, "['kind'] | count"))[0][0] == 1
print("quoted ['kind'] accepted as the table name in create-merge and ingest inline")

# THE ASSERTION A BLANKET CHANGE RESTS ON. Quoting the table name touches every
# drain, not just the one that names a keyword, so the quoted form must be the
# SAME ENTITY as the bare one — here, the Readings this file created bare and
# filled with four rows at the top. Acceptance alone would not rule out the
# quoted name resolving elsewhere, which would strand a drain's rows in a table
# its owner cannot see; a quoted COLUMN that named a new column would show up
# the same way, as a schema wider than three.
kusto(query_uri, "mgmt", DB,
      ".create-merge table ['Readings'] (['DeviceId']:string, ['Temp']:real, ['At']:datetime)")
assert primary_rows(kusto(query_uri, "query", DB, "Readings | getschema | count"))[0][0] == 3
assert primary_rows(kusto(query_uri, "query", DB, "Readings | count"))[0][0] == 4
print("fully quoted create-merge merges into Readings unchanged: 3 columns, 4 rows")

# ------------------------------------------------------------------ auth + RBAC
anon = requests.post(f"{query_uri}/v1/rest/query", json={"db": DB, "csl": "Readings | count"}, timeout=60)
assert anon.status_code == 401, anon.status_code
control_plane_token = requests.post(
    f"{query_uri}/v1/rest/query",
    headers={"Authorization": f"Bearer {fabric_token}", "Content-Type": "application/json"},
    json={"db": DB, "csl": "Readings | count"}, timeout=60)
assert control_plane_token.status_code == 401, control_plane_token.status_code
print("Kusto audience is enforced: OK")

missing = requests.post(f"{query_uri}/v1/rest/query", headers=KUSTO_HEADERS,
                        json={"db": "no-such-db", "csl": "Readings | count"}, timeout=60)
assert missing.status_code == 404, missing.status_code
print("unknown database: honest 404")

# -------------------------------------------- Microsoft's own Kusto SDK client
# azure-kusto-data builds /v2/rest/query and /v1/rest/mgmt from the cluster URI
# itself and parses the frame stream — an independent witness over the same
# surface, the way a real Fabric user's code reaches an eventhouse.
kcsb = KustoConnectionStringBuilder.with_aad_application_token_authentication(query_uri, kusto_token)
with KustoClient(kcsb) as client:
    crp = ClientRequestProperties()
    result = client.execute(DB, "Readings | summarize N=count(), Peak=max(Temp)", crp)
    row = result.primary_results[0][0]
    assert int(row["N"]) == 4, row
    assert round(float(row["Peak"]), 3) == 32.0, row
    print(f"azure-kusto-data (real SDK): N={row['N']} Peak={row['Peak']}")

    mgmt = client.execute_mgmt(DB, ".show tables")
    sdk_tables = [r["TableName"] for r in mgmt.primary_results[0]]
    assert "Readings" in sdk_tables, sdk_tables
    print(f"azure-kusto-data mgmt: {sdk_tables}")

# ------------------------------------ the AzureDataExplorerCommand ACTIVITY
# The pipeline activity that runs a KQL CONTROL COMMAND against an eventhouse,
# witnessed here rather than by Go tests alone because everything it needs is
# already standing: kustainer is the engine, fabric already has FABRIC_KQL_URL,
# and azure-kusto-data is in this image to confirm the effect independently.
#
# Judged by the ENGINE, not the activity's own status. A command activity that
# reported Succeeded having sent nothing would satisfy any check that read only
# the pipeline, and the emulator's own note for this row is that the engine's
# internal database name must not leak back into the output.


def run_pipeline(name, activities, want="Completed"):
    """Create → define → run → wait, returning the activity runs."""
    pl = fabric_post(f"/v1/workspaces/{ws['id']}/items",
                     {"displayName": name, "type": "DataPipeline"})
    payload = base64.b64encode(
        json.dumps({"properties": {"activities": activities}}).encode()).decode()
    # Raw POSTs, NOT fabric_post: it calls r.json() unconditionally, and both of
    # these answer 202 with an EMPTY body (createJobInstance writes
    # StatusAccepted and nothing else), so parsing would raise JSONDecodeError.
    # This suite cannot run on Apple silicon — kustainer needs a real x86-64
    # kernel — so bugs like it are found by reading, or by burning a CI cycle.
    def _accepted(path, body):
        r = requests.post(f"{FABRIC}{path}", headers=FABRIC_HEADERS, json=body, timeout=60)
        assert r.status_code in (200, 201, 202), f"{path} -> {r.status_code} {r.text[:300]}"
        return r

    _accepted(f"/v1/workspaces/{ws['id']}/items/{pl['id']}/updateDefinition",
              {"definition": {"parts": [{"path": "pipeline-content.json",
                                         "payload": payload,
                                         "payloadType": "InlineBase64"}]}})
    _accepted(f"/v1/workspaces/{ws['id']}/items/{pl['id']}/jobs/instances?jobType=Pipeline", {})
    inst = None
    for _ in range(120):
        got = fabric_get(
            f"/v1/workspaces/{ws['id']}/items/{pl['id']}/jobs/instances").get("value") or []
        inst = next((r for r in got if r.get("status") in ("Completed", "Failed")), None)
        if inst:
            break
        time.sleep(0.5)
    assert inst, f"{name}: never reached a terminal state"
    detail = requests.post(
        f"{FABRIC}/v1/workspaces/{ws['id']}/items/{pl['id']}/jobs/instances/"
        f"{inst['id']}/queryactivityruns", headers=FABRIC_HEADERS, json={}, timeout=60)
    runs = (detail.json() or {}).get("value") or []
    assert inst["status"] == want, f"{name}: job {inst['status']}, want {want}; runs={runs}"
    return runs


cmd_runs = run_pipeline("adx-cmd", [
    {"name": "Cmd", "type": "AzureDataExplorerCommand", "typeProperties": {
        # Column names mirror the `.create-merge table Readings` above, which is
        # PROVEN against this engine. The first version used
        # `(ts:datetime, kind:string)` — copied from the Go test, which runs
        # against a FAKE engine that does not parse KQL — and real kustainer
        # rejected it: `SYN0002 ... [line:position=1:43]`, position 43 being
        # `kind`, a KQL keyword. The emulator was right throughout: it relayed
        # the command and reported the engine's 400 faithfully.
        "command": ".create table PipelineEvents (DeviceId:string, At:datetime)",
        "database": {"itemId": db_ids[0]}}}])   # db_ids[0] IS the item id
cmd_out = next((r.get("output") or {} for r in cmd_runs if r.get("activityName") == "Cmd"), {})

# The output must carry the Fabric DISPLAY name, never the engine's internal
# per-item database name. That mapping is the emulator's job, and a leak here
# would hand a user a name that does not exist in their workspace.
assert cmd_out.get("database") == default_db["displayName"], cmd_out
assert cmd_out.get("tables") is not None, f"the engine's result was dropped: {cmd_out}"
print(f"ADX command activity: output names {cmd_out['database']!r}, engine result carried")

# A COMMAND activity must refuse a QUERY. Without this the row would be
# satisfied by an activity that happily runs anything it is handed.
run_pipeline("adx-query", [
    {"name": "Cmd", "type": "AzureDataExplorerCommand", "typeProperties": {
        "command": "PipelineEvents | count",
        "database": {"itemId": db_ids[0]}}}], want="Failed")
print("ADX command activity: a query is refused, as the contract says")

with KustoClient(kcsb) as client:
    # THE ASSERTION THAT MATTERS: Microsoft's own SDK, talking to the engine
    # directly, sees the table the PIPELINE created. Nothing about the activity
    # reporting Succeeded implies this.
    after = [r["TableName"] for r in client.execute_mgmt(DB, ".show tables").primary_results[0]]
    assert "PipelineEvents" in after, (
        f"the activity reported success but the engine has no PipelineEvents: {after}")
    print(f"azure-kusto-data confirms the activity's command landed: {after}")

print("RTI e2e: PASS — real KQL execution behind the Fabric Eventhouse contract")
