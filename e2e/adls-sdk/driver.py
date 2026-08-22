"""e2e: Microsoft's real Azure Blob SDK (`azure-storage-blob`) uploads and
downloads through fabric-emulator's OneLake Blob surface, authenticated by an
entra-emulator Storage-audience token.

Two independent oracles in one run:
  1. the real Azure SDK round-trips bytes through our surface (it points at
     the emulator via account_url + a static bearer TokenCredential; the
     account-prefixed /onelake/{container} path reaches the Blob dialect);
  2. pyarrow writes a Parquet file and reads it back after the SDK moved it —
     so the SDK, pyarrow's Parquet codec, and our storage all agree on the
     same bytes.

Driving the real SDK is what surfaced the x-ms-range gap (the Blob client
sends its range as x-ms-range, not Range, and requires 206 + Content-Range).
"""
import base64
import json
import os
import ssl
import time
import urllib.parse
import urllib.request
from io import BytesIO

import pyarrow as pa
import pyarrow.parquet as pq
from azure.core.credentials import AccessToken
from azure.storage.blob import BlobServiceClient

ENTRA = f"https://localhost:{os.environ.get('ENTRA_PORT', '18443')}"
FABRIC = f"https://127.0.0.1:{os.environ.get('FABRIC_PORT', '19443')}"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE  # self-signed harness cert


def post_json(url, body, token=None):
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, context=_CTX) as r:
        return json.loads(r.read() or b"{}")


def req_json(url, method, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, context=_CTX) as r:
        return json.loads(r.read() or b"{}")


def entra_token(scope=None, audience=None, client_id=None):
    if audience:
        t = post_json(f"{ENTRA}/admin/api/tokens",
                      {"clientId": client_id or "00d88624-f0d7-46f6-a641-6232c2608928",
                       "audience": audience})
        return t.get("access_token") or t["token"]
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
        "client_secret": "daemon-app-secret",
        "scope": scope,
    }).encode()
    with urllib.request.urlopen(urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form), context=_CTX) as r:
        return json.loads(r.read())["access_token"]


class StaticCredential:
    """A TokenCredential that hands the SDK our entra Storage token."""

    def __init__(self, token):
        self._token = token

    def get_token(self, *scopes, **kwargs):
        return AccessToken(self._token, int(time.time()) + 3600)


def viewer_storage_token(oid):
    """A Storage token whose principal is `oid`.

    The forge API needs a REGISTERED clientId, so the app stays the seeded one
    and `oid` is overridden instead -- which is the claim the emulator resolves
    a principal from (internal/auth: "oid claim (falls back to sub)").
    """
    t = post_json(f"{ENTRA}/admin/api/tokens", {
        "audience": "https://storage.azure.com",
        "extraClaims": {"oid": oid, "sub": oid},
    })
    return t.get("access_token") or t["token"]


fabric_token = entra_token(scope="https://api.fabric.microsoft.com/.default")
storage_token = entra_token(audience="https://storage.azure.com")

ws = post_json(f"{FABRIC}/v1/workspaces", {"displayName": "sdkws"}, fabric_token)
post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, fabric_token)
print(f"workspace: {ws['id']}")

svc = BlobServiceClient(account_url=f"{FABRIC}/onelake",
                        credential=StaticCredential(storage_token),
                        connection_verify=False)
container = ws["id"]  # the Blob container is the workspace

# 1. pyarrow writes a Parquet file; the SDK uploads it.
table = pa.table({"id": pa.array([10, 20, 30], pa.int64()), "city": ["nyc", "sfo", "sea"]})
buf = BytesIO()
pq.write_table(table, buf)
payload = buf.getvalue()
blob = "lake.Lakehouse/Files/data/cities.parquet"
svc.get_blob_client(container=container, blob=blob).upload_blob(payload, overwrite=True)
print(f"SDK uploaded {len(payload)} bytes of Parquet")

# 2. The SDK downloads it back; pyarrow parses it; rows must match.
downloaded = svc.get_blob_client(container=container, blob=blob).download_blob().readall()
assert downloaded == payload, "SDK round-trip changed the bytes"
got = pq.read_table(BytesIO(downloaded)).sort_by("id")
assert got.column("id").to_pylist() == [10, 20, 30], got
assert got.column("city").to_pylist() == ["nyc", "sfo", "sea"], got
print("SDK download + pyarrow parse: OK (3 rows, bytes identical)")

# 3. A ranged read via the SDK (exercises x-ms-range → 206 + Content-Range).
head = svc.get_blob_client(container=container, blob=blob).download_blob(offset=0, length=4).readall()
assert head == payload[:4], (head, payload[:4])
print("SDK ranged read (x-ms-range): OK")

# 4. List blobs via the SDK.
names = [b.name for b in svc.get_container_client(container).list_blobs()]
assert blob in names, names
print(f"SDK list_blobs sees it: {names}")

# 5. The same blob is visible through the DFS surface — one substrate.
req = urllib.request.Request(
    f"{FABRIC}/{container}?resource=filesystem&recursive=true",
    headers={"Authorization": "Bearer " + storage_token, "Host": "onelake.dfs.fabric.microsoft.com"})
with urllib.request.urlopen(req, context=_CTX) as r:
    dfs = [p["name"] for p in json.loads(r.read())["paths"]]
assert any(n.endswith("cities.parquet") for n in dfs), dfs
print(f"DFS surface sees the SDK-written blob: {len(dfs)} paths")

# --------------------------------------- an SDK write FIRES an event trigger
#
# The parity row for Reflex event triggers was witnessed only by Go tests, on
# the reading that the trigger BINDING is an emulator-native surface (Fabric
# publishes no REST for it) so nothing external could witness it. That confuses
# the definition with the effect. The binding is ours; the two things that
# matter are not:
#
#   * the WRITE is Microsoft's Azure Blob SDK, already used above, and
#   * the EFFECT is a real item job, readable over public Fabric REST.
#
# So the assertion is end to end through third-party surfaces: a file uploaded
# by the SDK causes a job the Fabric API reports as EventTriggered.
lake = post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/items",
                 {"displayName": "trig-lake", "type": "Lakehouse"}, fabric_token)
reflex = post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/items",
                   {"displayName": "watcher", "type": "Reflex"}, fabric_token)
pipe = post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/items",
                 {"displayName": "on-landing", "type": "DataPipeline"}, fabric_token)
wait_def = base64.b64encode(json.dumps({"properties": {"activities": [
    {"name": "Pause", "type": "Wait", "typeProperties": {"waitTimeInSeconds": 0}}]}}).encode()).decode()
post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/items/{pipe['id']}/updateDefinition",
          {"definition": {"parts": [{"path": "pipeline-content.json",
                                     "payload": wait_def,
                                     "payloadType": "InlineBase64"}]}}, fabric_token)
post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/reflexes/{reflex['id']}/triggers", {
    "displayName": "on-landing", "eventType": "Microsoft.Fabric.OneLake.FileCreated",
    "source": {"itemId": lake["id"], "pathPrefix": "Files/landing"},
    "action": {"itemId": pipe["id"], "jobType": "Pipeline"}}, fabric_token)


def pipeline_runs():
    req = urllib.request.Request(
        f"{FABRIC}/v1/workspaces/{ws['id']}/items/{pipe['id']}/jobs/instances",
        headers={"Authorization": "Bearer " + fabric_token})
    with urllib.request.urlopen(req, context=_CTX) as r:
        return json.loads(r.read()).get("value") or []


def sdk_put(rel, body):
    svc.get_blob_client(container=container,
                        blob=f"trig-lake.Lakehouse/{rel}").upload_blob(body, overwrite=True)


# The NEGATIVE half first: a write outside the watched prefix must start
# nothing. Without it, a trigger that fires on every write would pass.
sdk_put("Files/other/ignored.csv", b"x")
time.sleep(2)
assert not pipeline_runs(), f"a write outside the prefix started a run: {pipeline_runs()}"
print("event trigger: an SDK write outside the prefix starts nothing")

sdk_put("Files/landing/orders.csv", b"id\n1\n")
runs = []
for _ in range(60):
    runs = pipeline_runs()
    if runs:
        break
    time.sleep(0.5)
assert len(runs) == 1, f"watched write started {len(runs)} runs, want 1: {runs}"
assert runs[0].get("invokeType") == "EventTriggered", runs[0]
for _ in range(60):
    got = pipeline_runs()
    if got and got[0].get("status") in ("Completed", "Failed"):
        break
    time.sleep(0.5)
assert got[0]["status"] == "Completed", got[0]
print(f"event trigger: the SDK's write ran the pipeline, invokeType={runs[0]['invokeType']}")

# --- OneLake security: a role widens a Viewer, and only to what it names.
#
# The SDK is the point. Our own tests prove the emulator refuses; this proves an
# UNMODIFIED Microsoft SDK sees the refusal as a 403 on the wire, which is what
# a caller actually builds against. The denial is the assertion that carries
# weight — a granted read alone would pass against a surface enforcing nothing.
from azure.core.exceptions import HttpResponseError  # noqa: E402

VIEWER = "onelake-sec-viewer"
post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/roleAssignments",
          {"principal": {"id": VIEWER, "type": "User"}, "role": "Viewer"}, fabric_token)

granted_blob = "lake.Lakehouse/Tables/dbo/Customers/part-0.parquet"
denied_blob = "lake.Lakehouse/Tables/dbo/Orders/part-0.parquet"
for b in (granted_blob, denied_blob):
    svc.get_blob_client(container=container, blob=b).upload_blob(payload, overwrite=True)

viewer_svc = BlobServiceClient(
    account_url=f"{FABRIC}/onelake",
    credential=StaticCredential(viewer_storage_token(VIEWER)),
    connection_verify=False)


def viewer_status(blob):
    try:
        viewer_svc.get_blob_client(container=container, blob=blob).download_blob().readall()
        return 200
    except HttpResponseError as e:
        return e.status_code


# Before any role: a Viewer has no ReadAll, so both are refused. This is the
# state every item is in until someone authors a role.
assert viewer_status(granted_blob) == 403, "a Viewer read OneLake with no role granting it"
print("viewer without a role: 403 from the SDK")

lake_id = [i for i in req_json(f"{FABRIC}/v1/workspaces/{ws['id']}/items", "GET", None, fabric_token)["value"]
           if i["displayName"] == "lake"][0]["id"]
req_json(f"{FABRIC}/v1/workspaces/{ws['id']}/items/{lake_id}/dataAccessRoles", "PUT", {"value": [{
    "name": "readers",
    "decisionRules": [{"effect": "Permit", "permission": [
        {"attributeName": "Path", "attributeValueIncludedIn": ["Tables/dbo/Customers"]},
        {"attributeName": "Action", "attributeValueIncludedIn": ["Read"]}]}],
    "members": {"microsoftEntraMembers": [{"objectId": VIEWER}]}}]}, fabric_token)

assert viewer_status(granted_blob) == 200, "the granted path was still refused"
# THE CONTROL: the sibling table was never granted, and must stay refused.
assert viewer_status(denied_blob) == 403, "a grant on one table reached another"
print("OneLake security: the SDK reads the granted table and is refused the sibling")

print("ADLS-SDK E2E: PASS")
