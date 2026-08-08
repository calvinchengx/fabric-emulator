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
TENANT = "11111111-1111-1111-1111-111111111111"

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


def entra_token(scope=None, audience=None):
    if audience:
        t = post_json(f"{ENTRA}/admin/api/tokens",
                      {"clientId": "cccccccc-0000-0000-0000-000000000002", "audience": audience})
        return t.get("access_token") or t["token"]
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": "cccccccc-0000-0000-0000-000000000002",
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

print("ADLS-SDK E2E: PASS")
