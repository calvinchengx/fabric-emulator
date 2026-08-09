"""Ingest ServiceNow incidents into a lakehouse table, the way Microsoft documents it.

`e2e/rest-helix` proves the REST connector against a vendor Fabric has no
connector for. This proves it against **the vendor Microsoft's own connector
documentation uses to teach the feature**: Example 1 of connector-rest's
"Pagination rules examples" is
`api/now/table/incident?sysparm_limit=…&sysparm_offset=…` with the rule
`"QueryParameters.{offset}" : "RANGE:0:10000:1000"`. The suite runs that
example.

**Two runs, because ServiceNow documents two paging routes and a connector that
implements one would pass half the claim looking whole:**

    Offset    RestSource + QueryParameters.{offset} RANGE  -> Tables/bronze_incidents
    RFC 5988  RestSource + SupportRFC5988 (Link header)    -> Tables/bronze_incidents_rfc

Both must land all five rows. Page size is 2, so a reader that stops after one
page reports 2 and a reader one page short reports 4 — the count is the
assertion precisely because a truncating read otherwise looks healthy.

**What this exercises that Helix cannot.** Helix nests every field under
`values`, so its pipeline needs `translator.mappings` and never tests
auto-flattening. ServiceNow's records are flat under `$.result`, so this one
carries **no mappings at all** — the connector must infer the columns. The two
suites therefore cover opposite halves of row-shaping rather than duplicating
one.

**Scope, stated so the parity row cannot outrun it.** This proves the Table
API's documented *shape* is reachable through `RestSource` against a modelled
server. It does not prove Fabric's first-party ServiceNow connector type, which
the emulator does not implement: auth here is Basic through
`additionalHeaders`, because a native `authenticationType` is refused by name
(the emulator models no connections — see internal/api/restconnector.go). That
is a real Fabric pattern and the documented reason a user reaches for
RestSource over the built-in connector, which is Basic-auth only.
"""

import base64
import json
import sys
import time
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT = "00d88624-f0d7-46f6-a641-6232c2608928"
SECRET = "daemon-app-secret"
SNOW = "http://servicenow:8080"
TABLE_URL = f"{SNOW}/api/now/table/incident"
USER, PASSWORD = "fabric.emulator", "local-only"
BASIC = "Basic " + base64.b64encode(f"{USER}:{PASSWORD}".encode()).decode()
PAGE = 2
EXPECTED_ROWS = 5


def call(method, url, body=None, token=None):
    headers = {}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    with urllib.request.urlopen(urllib.request.Request(url, data=data, headers=headers, method=method)) as r:
        raw = r.read()
        return r.status, r.headers, (json.loads(raw) if raw else None)


def request(method, url, body=None, token=None):
    return call(method, url, body, token)[2]


def await_operation(op_id, token):
    for _ in range(120):
        op = request("GET", f"{FABRIC}/v1/operations/{op_id}", None, token)
        if (op or {}).get("status") == "Succeeded":
            return request("GET", f"{FABRIC}/v1/operations/{op_id}/result", None, token)
        if (op or {}).get("status") == "Failed":
            fail(f"operation {op_id} failed: {op}")
    fail(f"operation {op_id} never completed")


def token(scope):
    data = urllib.parse.urlencode({"grant_type": "client_credentials", "client_id": CLIENT,
                                   "client_secret": SECRET, "scope": scope}).encode()
    req = urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=data,
                                 headers={"Content-Type": "application/x-www-form-urlencoded"})
    with urllib.request.urlopen(req) as r:
        return json.load(r)["access_token"]


def fail(msg):
    print(f"FAIL: {msg}", file=sys.stderr)
    raise SystemExit(1)


# --- negative controls, before anything is asserted to work -------------------
#
# Without these, a passing ingest cannot distinguish "the connector sent
# credentials" from "the server lets anyone in" — the whole claim rests on the
# difference.
def expect_401(headers, what):
    req = urllib.request.Request(f"{TABLE_URL}?sysparm_limit=1", headers=headers)
    try:
        urllib.request.urlopen(req)
    except urllib.error.HTTPError as e:
        if e.code != 401:
            fail(f"{what}: got {e.code}, want 401")
        return
    fail(f"{what}: the request SUCCEEDED — the target is not enforcing auth, "
         f"so an ingest passing against it would prove nothing")


expect_401({}, "anonymous read")
expect_401({"Authorization": "Basic " + base64.b64encode(b"wrong:wrong").decode()}, "wrong credentials")
print("negative controls: anonymous and wrong-credential reads are refused", flush=True)

fabric = token("https://api.fabric.microsoft.com/.default")
ws = request("POST", f"{FABRIC}/v1/workspaces", {"displayName": "servicenow-ws"}, fabric)
lake = request("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, fabric)
lakehouse_ref = {"linkedService": {"properties": {"typeProperties": {"artifactId": lake["id"]}}}}


def copy_activity(name, table, source):
    return {
        "name": name,
        "type": "Copy",
        "typeProperties": {
            "source": source,
            # NO mappings. ServiceNow's records are flat under $.result, so the
            # connector must infer columns — the half Helix's nested shape can
            # never reach.
            "translator": {"type": "TabularTranslator", "collectionReference": "$['result']"},
            "sink": dict({"type": "LakehouseTableSink", "table": table,
                          "tableActionOption": "Overwrite"},
                         datasetSettings=lakehouse_ref),
        },
    }


offset_source = {
    "type": "RestSource",
    "url": f"{TABLE_URL}?sysparm_limit={PAGE}&sysparm_offset={{offset}}",
    "additionalHeaders": {"Authorization": BASIC},
    # Microsoft's Example 1, with an open-ended range: the emulator refuses a
    # rule whose end is unreachable rather than silently reading forever.
    "paginationRules": {
        "QueryParameters.{offset}": f"RANGE:0::{PAGE}",
        "EndCondition:$.result": "Empty",
    },
}

rfc_source = {
    "type": "RestSource",
    "url": f"{TABLE_URL}?sysparm_limit={PAGE}",
    "additionalHeaders": {"Authorization": BASIC},
    # The other documented route. The server emits first/next/last and drops
    # `next` on the final page, which is what terminates this read.
    "paginationRules": {"SupportRFC5988": "true"},
}

pipeline_definition = {"properties": {"activities": [
    copy_activity("IngestByOffset", "bronze_incidents", offset_source),
    copy_activity("IngestByLinkHeader", "bronze_incidents_rfc", rfc_source),
]}}

payload = base64.b64encode(json.dumps(pipeline_definition).encode()).decode()
status, headers, _ = call("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items", {
    "displayName": "servicenow-ingest", "type": "DataPipeline",
    "definition": {"parts": [{"path": "pipeline-content.json",
                              "payloadType": "InlineBase64", "payload": payload}]},
}, fabric)
if status != 202:
    fail(f"creating the pipeline answered {status}, want 202")
pipeline_id = await_operation(headers["x-ms-operation-id"], fabric)["id"]

status, headers, _ = call(
    "POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items/{pipeline_id}/jobs/instances?jobType=Pipeline",
    {}, fabric)
if status != 202:
    fail(f"starting the pipeline answered {status}, want 202")

# Async pipelines (doc 37 §4): the POST returns while the run continues.
location = urllib.parse.urlparse(headers["Location"])
for _ in range(120):
    job = request("GET", FABRIC + location.path + (f"?{location.query}" if location.query else ""), None, fabric)
    if job.get("status") not in ("NotStarted", "InProgress"):
        break
    time.sleep(1)
job_status, job_id = job.get("status"), job.get("id")

runs = request("POST",
               f"{FABRIC}/v1/workspaces/{ws['id']}/items/{pipeline_id}/jobs/instances/{job_id}/queryactivityruns",
               {}, fabric)
activities = (runs or {}).get("value", [])

if job_status != "Completed":
    for a in activities:
        print(f"  {a.get('activityName')}: {a.get('status')} {a.get('error') or ''}", file=sys.stderr)
    fail(f"pipeline status = {job_status}")

for name, table in (("IngestByOffset", "bronze_incidents"),
                    ("IngestByLinkHeader", "bronze_incidents_rfc")):
    act = next((a for a in activities if a.get("activityName") == name), None)
    if act is None:
        fail(f"no {name} activity run recorded")
    out = act.get("output") or {}
    if out.get("rowsCopied") != EXPECTED_ROWS:
        fail(f"{name}: rowsCopied = {out.get('rowsCopied')}, want {EXPECTED_ROWS} — "
             f"page size is {PAGE}, so {PAGE} means one page and "
             f"{EXPECTED_ROWS - 1} means one page short")
    print(f"{name}: {out.get('rowsCopied')} rows through {table}", flush=True)

# The rows really landed as Delta, read back through OneLake rather than
# trusting the activity's own report.
storage = token("https://storage.azure.com/.default")
for table in ("bronze_incidents", "bronze_incidents_rfc"):
    listing = request("GET",
                      f"http://onelake.dfs.fabric.microsoft.com/{ws['id']}?resource=filesystem&recursive=true"
                      f"&directory={lake['id']}/Tables/{table}", None, storage)
    paths = [p.get("name", "") for p in (listing or {}).get("paths", [])]
    if not any("_delta_log" in p for p in paths):
        fail(f"{table}: no _delta_log — the rows were not committed as Delta: {paths}")

print(f"PASS: ServiceNow Table API ingested through RestSource — "
      f"{EXPECTED_ROWS} rows by offset paging AND by RFC 5988, committed as Delta", flush=True)
