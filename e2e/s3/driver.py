"""External-store shortcut: a REAL S3 server behind OneLake.

Nothing here is a stub or an anonymous HTTP fetch. SeaweedFS runs with an
identity config, so every request it serves must carry a valid AWS SigV4
signature — that is the whole point of the suite. The witness:

  1. writes an object with **boto3**, Amazon's own SDK, so the bytes are put
     there by a real S3 client and not by us;
  2. proves the bucket really is protected, by showing an unsigned GET is
     refused (otherwise step 4 would prove nothing);
  3. creates a Fabric Connection carrying the Access Key ID / Secret Access
     Key pair, and an `AmazonS3` shortcut pointing at the bucket — the same
     two fields Fabric's own shortcut dialog collects;
  4. reads the object back **through the OneLake ADLS surface**, which forces
     the emulator to sign the upstream request itself.

If the emulator's SigV4 implementation were wrong, step 4 would 403 while
steps 1-3 still passed.
"""
import io
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request

import boto3
import requests
from botocore.config import Config

FABRIC = os.environ.get("FABRIC", "http://fabric-emulator")
ENTRA = os.environ.get("ENTRA", "http://entra-emulator:8443")
S3_ENDPOINT = os.environ.get("S3_ENDPOINT", "http://seaweedfs:8333")
ACCESS_KEY = os.environ["S3_ACCESS_KEY"]
SECRET_KEY = os.environ["S3_SECRET_KEY"]
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"

BUCKET = "lake-exports"
KEY = "curated/readings.csv"
PAYLOAD = b"device,temp\ndev-1,21.5\ndev-2,30.0\n"


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
    """entra-emulator's admin mint for the Storage audience — the same escape
    hatch e2e/adls-sdk and e2e/delta-rs use. Still a real signed JWT over real
    JWKS; only the resource registration is skipped."""
    body = json.dumps({"clientId": CLIENT_ID, "audience": audience}).encode()
    req = urllib.request.Request(f"{ENTRA}/admin/api/tokens", data=body,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req) as r:
        payload = json.loads(r.read())
    return payload.get("access_token") or payload["token"]


token = entra_token("https://api.fabric.microsoft.com/.default")
# OneLake is a separate surface: Storage-audience token, and it is routed by
# Host header, so the DFS host has to be sent explicitly.
storage_token = forged_token("https://storage.azure.com")
ONELAKE_HEADERS = {"Authorization": f"Bearer {storage_token}",
                   "Host": "onelake.dfs.fabric.microsoft.com"}
HEADERS = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}


def fabric_post(path, body):
    r = requests.post(f"{FABRIC}{path}", headers=HEADERS, json=body, timeout=60)
    r.raise_for_status()
    return r.json() if r.content else {}


# SeaweedFS takes a few seconds to bind its S3 port. Any HTTP answer — including
# the 403 an anonymous request correctly gets — means it is serving.
def wait_for_s3(timeout=180):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            urllib.request.urlopen(f"{S3_ENDPOINT}/", timeout=5)
            return
        except urllib.error.HTTPError:
            return  # refused, but answering — that is what we want
        except Exception as e:
            last = e
            time.sleep(2)
    raise SystemExit(f"S3 endpoint {S3_ENDPOINT} never came up: {last}")


wait_for_s3()

# ------------------------------------------------- 1. write with a real SDK
s3 = boto3.client(
    "s3",
    endpoint_url=S3_ENDPOINT,
    aws_access_key_id=ACCESS_KEY,
    aws_secret_access_key=SECRET_KEY,
    region_name="us-east-1",
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
)
s3.create_bucket(Bucket=BUCKET)
s3.upload_fileobj(io.BytesIO(PAYLOAD), BUCKET, KEY)
roundtrip = s3.get_object(Bucket=BUCKET, Key=KEY)["Body"].read()
assert roundtrip == PAYLOAD, roundtrip
print(f"boto3 wrote and read back s3://{BUCKET}/{KEY} ({len(PAYLOAD)} bytes)")

# --------------------------------------- 2. prove the bucket is NOT public
# Without this the read-through below would not demonstrate signing at all.
try:
    with urllib.request.urlopen(f"{S3_ENDPOINT}/{BUCKET}/{KEY}", timeout=30) as r:
        raise SystemExit(f"unsigned GET succeeded ({r.status}) — the bucket is public, "
                         "so this suite would not be testing SigV4 at all")
except urllib.error.HTTPError as e:
    if e.code not in (401, 403):
        raise SystemExit(f"unsigned GET returned {e.code}, want 401/403")
    print(f"unsigned GET correctly refused: HTTP {e.code}")

# ------------------------------- 3. Fabric connection + AmazonS3 shortcut
ws = fabric_post("/v1/workspaces", {"displayName": "s3-shortcuts"})
lakehouse = fabric_post(f"/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lh"})

# Fabric's S3 connector uses authentication kind "Access Key" with an Access
# Key Id and a Secret Access Key (fabric-docs connector-amazon-s3.md). The REST
# reference's CredentialType enum has no AccessKey member, and Basic is the
# only documented type carrying two secrets — so the pair travels as
# username/password. Only documented enum values are used here.
conn = fabric_post("/v1/connections", {
    "displayName": "seaweedfs-s3",
    "connectivityType": "ShareableCloud",
    "credentialDetails": {"credentials": {
        "credentialType": "Basic",
        "username": ACCESS_KEY,
        "password": SECRET_KEY,
    }},
})

shortcut = fabric_post(
    f"/v1/workspaces/{ws['id']}/items/{lakehouse['id']}/shortcuts",
    {
        "path": "Files",
        "name": "s3exports",
        "target": {"amazonS3": {
            "location": f"{S3_ENDPOINT}/{BUCKET}",
            "subpath": "curated",
            "connectionId": conn["id"],
        }},
    },
)
print(f"created AmazonS3 shortcut {shortcut.get('name', 's3exports')}")

# ------------------------- 4. read it back THROUGH OneLake (emulator signs)
onelake = requests.get(
    f"{FABRIC}/{ws['id']}/{lakehouse['id']}/Files/s3exports/readings.csv",
    headers=ONELAKE_HEADERS, timeout=60,
)
if onelake.status_code != 200:
    raise SystemExit(f"OneLake read-through failed: HTTP {onelake.status_code} {onelake.text[:400]}")
if onelake.content != PAYLOAD:
    raise SystemExit(f"read-through content mismatch: {onelake.content!r}")
print(f"OneLake read-through returned the real S3 object ({len(onelake.content)} bytes)")

# A wrong secret must fail, or the signature is not actually being checked.
bad = fabric_post("/v1/connections", {
    "displayName": "seaweedfs-s3-wrong",
    "connectivityType": "ShareableCloud",
    "credentialDetails": {"credentials": {
        "credentialType": "Basic",
        "username": ACCESS_KEY,
        "password": "wrong-secret-entirely",
    }},
})
fabric_post(
    f"/v1/workspaces/{ws['id']}/items/{lakehouse['id']}/shortcuts",
    {"path": "Files", "name": "s3bad",
     "target": {"amazonS3": {"location": f"{S3_ENDPOINT}/{BUCKET}",
                             "subpath": "curated", "connectionId": bad["id"]}}},
)
denied = requests.get(
    f"{FABRIC}/{ws['id']}/{lakehouse['id']}/Files/s3bad/readings.csv",
    headers=ONELAKE_HEADERS, timeout=60,
)
if denied.status_code == 200:
    raise SystemExit("a wrong secret still read the object — the signature is not being verified")
print(f"wrong secret correctly refused through the shortcut: HTTP {denied.status_code}")

print("S3 e2e: PASS — real S3 server, real AWS SDK, real SigV4 through OneLake")
