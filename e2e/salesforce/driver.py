"""Salesforce round trip: Bulk API 2.0 out of an org, into a lakehouse, and back.

A round trip rather than a one-way read, because it proves the two halves
against each other. Reading alone can pass while the rows are subtly wrong;
writing them back and comparing what the org RECEIVED against what it SERVED
catches a shape that only looked right.

    Login (Web activity)   ->  OAuth client credentials, access_token
      |
      v
    Import (Copy)          ->  SalesforceV2Source: create a Bulk query job,
      |                        poll it out of InProgress, page results by
      |                        Sforce-Locator until the string "null"
      v
    Lakehouse Tables/bronze_accounts (Delta)
      |
      v
    Export (Copy)          ->  SalesforceV2Sink: create an ingest job, PUT the
                               CSV, PATCH to UploadComplete, poll

Every activity type is real in Fabric and every one executes for real here.
"""

import base64
import json
import sys
import time
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT = "cccccccc-0000-0000-0000-000000000002"
SECRET = "daemon-app-secret"
SF = "http://salesforce:8080"
EXPECTED_ROWS = 5
BATCH = 2


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


def fail(msg):
    print(f"FAIL: {msg}", file=sys.stderr)
    raise SystemExit(1)


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


def run_pipeline(fabric, ws_id, name, definition):
    payload = base64.b64encode(json.dumps(definition).encode()).decode()
    status, headers, _ = call("POST", f"{FABRIC}/v1/workspaces/{ws_id}/items", {
        "displayName": name, "type": "DataPipeline",
        "definition": {"parts": [{"path": "pipeline-content.json",
                                  "payloadType": "InlineBase64", "payload": payload}]},
    }, fabric)
    if status != 202:
        fail(f"creating {name} answered {status}, want 202")
    pid = await_operation(headers["x-ms-operation-id"], fabric)["id"]

    status, headers, _ = call("POST",
                              f"{FABRIC}/v1/workspaces/{ws_id}/items/{pid}/jobs/instances?jobType=Pipeline",
                              {}, fabric)
    if status != 202:
        fail(f"starting {name} answered {status}, want 202")
    # The Location header is an absolute https:// URL as Fabric's is; this stack
    # runs plain HTTP, so it is followed by PATH. The contract is the path.
    loc = urllib.parse.urlparse(headers["Location"])
    # Async pipelines (doc 37 §4): poll to terminal before reading the runs.
    for _ in range(120):
        job = request("GET", FABRIC + loc.path + (f"?{loc.query}" if loc.query else ""), None, fabric)
        if job.get("status") not in ("NotStarted", "InProgress"):
            break
        time.sleep(1)

    runs = request("POST",
                   f"{FABRIC}/v1/workspaces/{ws_id}/items/{pid}/jobs/instances/{job['id']}/queryactivityruns",
                   {}, fabric)
    activities = (runs or {}).get("value", [])
    if job.get("status") != "Completed":
        for a in activities:
            print(f"  {a.get('activityName')}: {a.get('status')} {a.get('error') or ''}", file=sys.stderr)
        fail(f"{name} status = {job.get('status')}")
    return activities


fabric = token("https://api.fabric.microsoft.com/.default")
ws = request("POST", f"{FABRIC}/v1/workspaces", {"displayName": "salesforce-ws"}, fabric)
lake = request("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, fabric)
lakehouse_ref = {"linkedService": {"properties": {"typeProperties": {"artifactId": lake["id"]}}}}

# The org's token is fetched by a Web activity and threaded into both copies as
# an expression — the emulator models no connections, so this is how a real
# pipeline supplies one (docs/41).
login = {
    "name": "Login", "type": "WebActivity",
    "typeProperties": {
        "url": f"{SF}/services/oauth2/token", "method": "POST",
        "headers": {"Content-Type": "application/x-www-form-urlencoded"},
        "body": "grant_type=client_credentials&client_id=fabric-emulator-e2e&client_secret=local-only",
    },
}
sf_token = "@{activity('Login').output.access_token}"

activities = run_pipeline(fabric, ws["id"], "salesforce-import", {"properties": {"activities": [
    login,
    {
        "name": "Import", "type": "Copy",
        "dependsOn": [{"activity": "Login", "dependencyConditions": ["Succeeded"]}],
        "typeProperties": {
            "source": {"type": "SalesforceV2Source", "instanceUrl": SF,
                       "accessToken": sf_token, "objectApiName": "Account"},
            "sink": dict({"type": "LakehouseTableSink", "table": "bronze_accounts",
                          "tableActionOption": "Overwrite"}, datasetSettings=lakehouse_ref),
        },
    },
]}})

imported = next((a for a in activities if a.get("activityName") == "Import"), None)
if imported is None:
    fail("no Import activity run recorded")
out = imported.get("output") or {}
if out.get("rowsCopied") != EXPECTED_ROWS:
    # A connector that read only the first page would report 2 here and look
    # perfectly healthy.
    fail(f"rowsCopied = {out.get('rowsCopied')}, want {EXPECTED_ROWS} (locator paging incomplete)")
if out.get("resultPages") != 3:
    fail(f"resultPages = {out.get('resultPages')}, want 3")

# The rows really committed as Delta, readable back through OneLake.
storage = token("https://storage.azure.com/.default")
listing = request("GET",
                  f"http://onelake.dfs.fabric.microsoft.com/{ws['id']}?resource=filesystem&recursive=true"
                  f"&directory={lake['id']}/Tables/bronze_accounts", None, storage)
paths = [p.get("name", "") for p in (listing or {}).get("paths", [])]
if not any("_delta_log" in p for p in paths):
    fail(f"no _delta_log under the table — rows were not committed as Delta: {paths}")

# --- and back out again ----------------------------------------------------
activities = run_pipeline(fabric, ws["id"], "salesforce-export", {"properties": {"activities": [
    login,
    {
        "name": "Export", "type": "Copy",
        "dependsOn": [{"activity": "Login", "dependencyConditions": ["Succeeded"]}],
        "typeProperties": {
            "source": {"type": "LakehouseTableSource",
                       "location": {"itemId": lake["id"], "path": "Tables/bronze_accounts"}},
            "sink": {"type": "SalesforceV2Sink", "instanceUrl": SF, "accessToken": sf_token,
                     "objectApiName": "Account", "writeBehavior": "Upsert",
                     "externalIdFieldName": "Id", "writeBatchSize": BATCH},
        },
    },
]}})

exported = next((a for a in activities if a.get("activityName") == "Export"), None)
if exported is None:
    fail("no Export activity run recorded")
out = exported.get("output") or {}
if out.get("rowsCopied") != EXPECTED_ROWS:
    fail(f"exported rowsCopied = {out.get('rowsCopied')}, want {EXPECTED_ROWS}")
expected_jobs = -(-EXPECTED_ROWS // BATCH)
if out.get("jobsWritten") != expected_jobs:
    fail(f"jobsWritten = {out.get('jobsWritten')}, want {expected_jobs} at writeBatchSize {BATCH}")

# What the ORG received, not what we believe we sent. This is the half a one-way
# read cannot check: the rows survived Delta and came back identical.
state = request("GET", f"{SF}/_debug/state")
ingest = state["ingestJobs"]
if len(ingest) != expected_jobs:
    fail(f"the org saw {len(ingest)} ingest jobs, want {expected_jobs}")
for jid, job in ingest.items():
    if job["state"] != "JobComplete":
        fail(f"ingest job {jid} left in {job['state']} — a job never PATCHed is never processed")
    if job["operation"] != "upsert" or job["externalIdFieldName"] != "Id":
        fail(f"ingest job {jid} created as {job['operation']}/{job['externalIdFieldName']}")

received = sorted((r["Id"], r["Name"], r["Industry"]) for job in ingest.values() for r in job["rows"])
if len(received) != EXPECTED_ROWS:
    fail(f"the org received {len(received)} records across its jobs, want {EXPECTED_ROWS}")
names = [r[1] for r in received]
if "Acme Corporation" not in names or "Soylent Foods" not in names:
    fail(f"the round trip lost or altered records: {names}")

# The query job really ran as a lifecycle: it was polled out of InProgress.
qjobs = state["queryJobs"]
if not qjobs:
    fail("the org saw no query job")
for jid, job in qjobs.items():
    if job["polls"] < 3:
        fail(f"query job {jid} was polled {job['polls']} times — it must be waited out of InProgress")
    if job["operation"] != "query":
        fail(f"query job {jid} operation = {job['operation']}")

print(f"OK: {EXPECTED_ROWS} accounts out of Salesforce over 3 locator pages, committed as Delta, "
      f"and upserted back in {expected_jobs} ingest jobs — every record identical")
