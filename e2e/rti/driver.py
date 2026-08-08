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
import json
import os
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

print("RTI e2e: PASS — real KQL execution behind the Fabric Eventhouse contract")
