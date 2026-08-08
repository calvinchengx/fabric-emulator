"""Ingest BMC Helix incidents into a lakehouse table, the way a real Fabric user would.

This is the end-to-end claim the REST connector exists to support, and BMC Helix
is the sharpest case for it: **Helix has no Fabric connector at all** — not in
Fabric, not in ADF, not in Power Query — so the generic REST connector is the
only supported route. Its competitor ServiceNow has a first-party connector;
Helix does not.

The pipeline is Fabric-shaped throughout. Every activity type in it is real in
the product, and every one executes for real here:

    Login (Web activity)  ->  POST /api/jwt/login, raw JWT in the body
      |
      v
    Ingest (Copy)         ->  RestSource, AR-JWT header built from the login
                              output, limit/offset pagination, ARS records
                              unnested through translator.mappings
      |
      v
    Lakehouse Tables/bronze_incidents (Delta)

The AR-JWT step is the reason this shape matters. Helix's scheme is not one of
Fabric's built-in authentication types, so a real user fetches the token with a
Web activity and threads it into the copy as an expression — which is exactly
what runs below.
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
HELIX = "http://helix:8080"
PAGE = 2
EXPECTED_ROWS = 5


def call(method, url, body=None, token=None):
    """Returns (status, headers, parsed-body). Nearly every mutation here answers
    202 with the result behind an operation, so the headers are not optional."""
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
    """Poll an LRO to Succeeded and return its result. The emulator completes on
    the next poll by default, so this is a handful of iterations, not a wait."""
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


fabric = token("https://api.fabric.microsoft.com/.default")
ws = request("POST", f"{FABRIC}/v1/workspaces", {"displayName": "helix-ws"}, fabric)
lake = request("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, fabric)

lakehouse_ref = {"linkedService": {"properties": {"typeProperties": {"artifactId": lake["id"]}}}}
entry_url = f"{HELIX}/api/arsys/v1/entry/" + urllib.parse.quote("HPD:Help Desk")

pipeline_definition = {
    "properties": {
        "activities": [
            {
                # Helix answers with the raw JWT as the whole body, so the token
                # is `output.body` — there is no JSON envelope to reach into.
                "name": "Login",
                "type": "WebActivity",
                "typeProperties": {
                    "url": f"{HELIX}/api/jwt/login",
                    "method": "POST",
                    "body": "username=fabric-emulator&password=local-only",
                    "headers": {"Content-Type": "application/x-www-form-urlencoded"},
                },
            },
            {
                "name": "Ingest",
                "type": "Copy",
                "dependsOn": [{"activity": "Login", "dependencyConditions": ["Succeeded"]}],
                "typeProperties": {
                    "source": {
                        "type": "RestSource",
                        "url": f"{entry_url}?limit={PAGE}&offset={{offset}}",
                        # AR-JWT, not Bearer. The fake server rejects Bearer for
                        # the same reason the real one does.
                        "additionalHeaders": {
                            "Authorization": "AR-JWT @{activity('Login').output.body}"
                        },
                        "paginationRules": {
                            "QueryParameters.{offset}": f"RANGE:0::{PAGE}",
                            "EndCondition:$.entries": "Empty",
                        },
                    },
                    # ARS nests every field under `values`, so auto-flatten finds
                    # no scalar columns. Mappings are what make this shape work.
                    "translator": {
                        "type": "TabularTranslator",
                        "collectionReference": "$['entries']",
                        "mappings": [
                            {"source": {"path": "$['values']['Incident Number']"},
                             "sink": {"name": "incident_number"}},
                            {"source": {"path": "$['values']['Description']"},
                             "sink": {"name": "description"}},
                            {"source": {"path": "$['values']['Status']"},
                             "sink": {"name": "status"}},
                            {"source": {"path": "$['values']['Priority']"},
                             "sink": {"name": "priority"}},
                        ],
                    },
                    "sink": dict({"type": "LakehouseTableSink", "table": "bronze_incidents",
                                  "tableActionOption": "Overwrite"},
                                 datasetSettings=lakehouse_ref),
                },
            },
        ]
    }
}

payload = base64.b64encode(json.dumps(pipeline_definition).encode()).decode()
status, headers, _ = call("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items", {
    "displayName": "helix-ingest", "type": "DataPipeline",
    "definition": {"parts": [{"path": "pipeline-content.json",
                              "payloadType": "InlineBase64", "payload": payload}]},
}, fabric)
if status != 202:
    fail(f"creating the pipeline answered {status}, want 202")
pipeline_id = await_operation(headers["x-ms-operation-id"], fabric)["id"]

status, headers, _ = call("POST",
                          f"{FABRIC}/v1/workspaces/{ws['id']}/items/{pipeline_id}/jobs/instances?jobType=Pipeline",
                          {}, fabric)
if status != 202:
    fail(f"starting the pipeline answered {status}, want 202")
# The Location header is an ABSOLUTE https:// URL, as Fabric's is — but this
# stack runs the emulator over plain HTTP, so the header is followed by PATH
# rather than verbatim. Fabric's contract is the path; the scheme is this
# deployment's business.
location = urllib.parse.urlparse(headers["Location"])
# Async pipelines (doc 37 §4): the 202 returns while the pipeline runs, so
# poll to a terminal state the way a real client does — the activity-run
# detail exists only once the run finishes.
for _ in range(120):
    job = request("GET", FABRIC + location.path + (f"?{location.query}" if location.query else ""), None, fabric)
    if job.get("status") not in ("NotStarted", "InProgress"):
        break
    time.sleep(1)
status, job_id = job.get("status"), job.get("id")

runs = request("POST",
               f"{FABRIC}/v1/workspaces/{ws['id']}/items/{pipeline_id}/jobs/instances/{job_id}/queryactivityruns",
               {}, fabric)
activities = runs.get("value", []) if runs else []

if status != "Completed":
    for a in activities:
        print(f"  {a.get('activityName')}: {a.get('status')} {a.get('error') or ''}", file=sys.stderr)
    fail(f"pipeline status = {status}")

ingest = next((a for a in activities if a.get("activityName") == "Ingest"), None)
if ingest is None:
    fail("no Ingest activity run recorded")

out = ingest.get("output") or {}
if out.get("rowsCopied") != EXPECTED_ROWS:
    # A connector that read only the first page would report 2 here and look
    # perfectly healthy — which is the whole reason this asserts a count that
    # only full pagination can produce.
    fail(f"rowsCopied = {out.get('rowsCopied')}, want {EXPECTED_ROWS} (pagination did not complete)")

# The rows really landed as Delta, readable back through OneLake.
storage = token("https://storage.azure.com/.default")
listing = request("GET",
                  f"http://onelake.dfs.fabric.microsoft.com/{ws['id']}?resource=filesystem&recursive=true"
                  f"&directory={lake['id']}/Tables/bronze_incidents", None, storage)
paths = [p.get("name", "") for p in (listing or {}).get("paths", [])]
if not any("_delta_log" in p for p in paths):
    fail(f"no _delta_log under the table — the rows were not committed as Delta: {paths}")

# Lineage marks the source as OUTSIDE Fabric and carries the Helix URL, so the
# portal draws it as an external node instead of hunting for an item.
edge = out.get("lineage") or {}
if edge.get("sourceKind") != "connection":
    fail(f"lineage sourceKind = {edge.get('sourceKind')}, want connection")
if "arsys" not in str(edge.get("sourcePath")):
    fail(f"lineage sourcePath does not name the Helix entry point: {edge.get('sourcePath')}")

print(f"OK: {out['rowsCopied']} Helix incidents ingested through "
      f"{-(-EXPECTED_ROWS // PAGE) + 1} paged requests, AR-JWT authenticated, committed as Delta")
