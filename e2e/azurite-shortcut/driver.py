"""ADLS Gen2 shortcut reads, witnessed against Microsoft's own storage emulator.

What this proves, and what it deliberately does not:

  * Azurite implements Blob/Queue/Table and **not** ADLS Gen2 — Microsoft
    documents that limitation. So the endpoint here is the Blob one. That is
    still a real witness for the shortcut *read path*, because the emulator's
    read-through issues a plain authenticated GET, which is identical on the
    Blob and DFS endpoints of a real ADLS Gen2 account. DFS-specific behaviour
    (the append/flush write protocol, directory semantics) is NOT witnessed
    here and remains unwitnessed offline.
  * It replaces a hand-written Python stub that validated nothing.

The steps:
  1. Upload a blob with **azure-storage-blob**, Microsoft's own SDK, using
     shared-key auth — so the bytes are put there by a real client.
  2. Prove an unauthenticated GET is refused (403), or step 4 proves nothing.
  3. Create a Fabric Connection carrying a real **SAS token** and an
     `ADLSGen2` shortcut pointing at the container.
  4. Read the blob back through the OneLake ADLS surface, which forces the
     emulator to present the SAS upstream.
  5. Show a *revoked* (tampered) SAS is refused, so the signature is really
     being checked.
"""
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone

import requests
from azure.storage.blob import BlobServiceClient, BlobSasPermissions, generate_blob_sas

FABRIC = os.environ.get("FABRIC", "http://fabric-emulator")
ENTRA = os.environ.get("ENTRA", "http://entra-emulator:8443")
AZURITE = os.environ.get("AZURITE", "http://azurite:10000")
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"

# Azurite's well-known development account (documented by Microsoft).
ACCOUNT = "devstoreaccount1"
ACCOUNT_KEY = (
    "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)
CONTAINER = "curated"
WRITTEN_BLOB = "written-through-shortcut.csv"
WRITTEN_PAYLOAD = b"written,through\nshortcut,yes\n"
BLOB = "readings.csv"
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
    hatch e2e/adls-sdk and e2e/delta-rs use. A real signed JWT over real JWKS."""
    body = json.dumps({"clientId": CLIENT_ID, "audience": audience}).encode()
    req = urllib.request.Request(f"{ENTRA}/admin/api/tokens", data=body,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req) as r:
        payload = json.loads(r.read())
    return payload.get("access_token") or payload["token"]


token = entra_token("https://api.fabric.microsoft.com/.default")
HEADERS = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
# OneLake is Host-routed and takes a Storage-audience token.
ONELAKE_HEADERS = {"Authorization": f"Bearer {forged_token('https://storage.azure.com')}",
                   "Host": "onelake.dfs.fabric.microsoft.com"}


def fabric_post(path, body):
    r = requests.post(f"{FABRIC}{path}", headers=HEADERS, json=body, timeout=60)
    r.raise_for_status()
    return r.json() if r.content else {}


def wait_for_azurite(timeout=120):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            urllib.request.urlopen(f"{AZURITE}/{ACCOUNT}/{CONTAINER}", timeout=5)
            return
        except urllib.error.HTTPError:
            return  # answering (403 for an unauthenticated request) — that is up
        except Exception as e:
            last = e
            time.sleep(2)
    raise SystemExit(f"azurite never came up: {last}")


wait_for_azurite()

# ------------------------------------------- 1. upload with Microsoft's SDK
conn_str = (f"DefaultEndpointsProtocol=http;AccountName={ACCOUNT};"
            f"AccountKey={ACCOUNT_KEY};BlobEndpoint={AZURITE}/{ACCOUNT};")
svc = BlobServiceClient.from_connection_string(conn_str)
try:
    svc.create_container(CONTAINER)
except Exception:
    pass  # already exists
svc.get_blob_client(CONTAINER, BLOB).upload_blob(PAYLOAD, overwrite=True)
back = svc.get_blob_client(CONTAINER, BLOB).download_blob().readall()
assert back == PAYLOAD, back
print(f"azure-storage-blob wrote and read back {CONTAINER}/{BLOB} ({len(PAYLOAD)} bytes)")

# ------------------------------- 2. prove the container is NOT public
try:
    with urllib.request.urlopen(f"{AZURITE}/{ACCOUNT}/{CONTAINER}/{BLOB}", timeout=30) as r:
        raise SystemExit(f"unsigned GET succeeded ({r.status}) — the container is public, "
                         "so this suite would not be testing SAS at all")
except urllib.error.HTTPError as e:
    if e.code not in (401, 403, 404):
        raise SystemExit(f"unsigned GET returned {e.code}, want 401/403")
    print(f"unauthenticated GET correctly refused: HTTP {e.code}")

# -------------------------------------- 3. real SAS + ADLSGen2 shortcut
sas = generate_blob_sas(
    account_name=ACCOUNT, container_name=CONTAINER, blob_name=BLOB,
    account_key=ACCOUNT_KEY, permission=BlobSasPermissions(read=True),
    expiry=datetime.now(timezone.utc) + timedelta(hours=1),
)
ws = fabric_post("/v1/workspaces", {"displayName": "adls-shortcuts"})
lakehouse = fabric_post(f"/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lh"})

# SharedAccessSignature is a documented CredentialType; `token` is its
# documented field (core/connections/create-connection).
conn = fabric_post("/v1/connections", {
    "displayName": "azurite-adls",
    "connectivityType": "ShareableCloud",
    "credentialDetails": {"credentials": {
        "credentialType": "SharedAccessSignature",
        "token": sas,
    }},
})
fabric_post(
    f"/v1/workspaces/{ws['id']}/items/{lakehouse['id']}/shortcuts",
    {"path": "Files", "name": "adlsdata",
     "target": {"adlsGen2": {
         "location": f"{AZURITE}/{ACCOUNT}/{CONTAINER}",
         "subpath": "",
         "connectionId": conn["id"],
     }}},
)
print("created ADLSGen2 shortcut adlsdata")

# ------------------- 4. read back THROUGH OneLake (emulator presents the SAS)
onelake = requests.get(
    f"{FABRIC}/{ws['id']}/{lakehouse['id']}/Files/adlsdata/{BLOB}",
    headers=ONELAKE_HEADERS, timeout=60,
)
if onelake.status_code != 200:
    raise SystemExit(f"OneLake read-through failed: HTTP {onelake.status_code} {onelake.text[:400]}")
if onelake.content != PAYLOAD:
    raise SystemExit(f"read-through content mismatch: {onelake.content!r}")
print(f"OneLake read-through returned the real blob ({len(onelake.content)} bytes)")

# ------------------------- 5. a tampered SAS must be refused
tampered = sas.replace("sig=", "sig=00")
bad = fabric_post("/v1/connections", {
    "displayName": "azurite-adls-bad",
    "connectivityType": "ShareableCloud",
    "credentialDetails": {"credentials": {
        "credentialType": "SharedAccessSignature",
        "token": tampered,
    }},
})
fabric_post(
    f"/v1/workspaces/{ws['id']}/items/{lakehouse['id']}/shortcuts",
    {"path": "Files", "name": "adlsbad",
     "target": {"adlsGen2": {
         "location": f"{AZURITE}/{ACCOUNT}/{CONTAINER}",
         "subpath": "", "connectionId": bad["id"],
     }}},
)
denied = requests.get(
    f"{FABRIC}/{ws['id']}/{lakehouse['id']}/Files/adlsbad/{BLOB}",
    headers=ONELAKE_HEADERS, timeout=60,
)
if denied.status_code == 200:
    raise SystemExit("a tampered SAS still read the blob — the signature is not being verified")
print(f"tampered SAS correctly refused through the shortcut: HTTP {denied.status_code}")

# ------------- 6. WRITE through the shortcut lands in the storage account
# Fabric supports writes through an ADLS Gen2 shortcut (and documents deleting
# a file within a shortcut as deleting it at the target). The proof that
# matters is not the emulator's 200 — it is Azurite holding the bytes
# afterwards, read back with the SDK rather than through the emulator.
write_sas = generate_blob_sas(
    account_name=ACCOUNT, container_name=CONTAINER, blob_name=WRITTEN_BLOB,
    account_key=ACCOUNT_KEY,
    permission=BlobSasPermissions(read=True, write=True, create=True, delete=True),
    expiry=datetime.now(timezone.utc) + timedelta(hours=1),
)
write_conn = fabric_post("/v1/connections", {
    "displayName": "azurite-adls-rw",
    "connectivityType": "ShareableCloud",
    "credentialDetails": {"credentials": {
        "credentialType": "SharedAccessSignature", "token": write_sas,
    }},
})
fabric_post(
    f"/v1/workspaces/{ws['id']}/items/{lakehouse['id']}/shortcuts",
    {"path": "Files", "name": "adlswrite",
     "target": {"adlsGen2": {
         "location": f"{AZURITE}/{ACCOUNT}/{CONTAINER}",
         "subpath": "", "connectionId": write_conn["id"],
     }}},
)

base = f"{FABRIC}/{ws['id']}/{lakehouse['id']}/Files/adlswrite/{WRITTEN_BLOB}"
created = requests.put(base + "?resource=file", headers=ONELAKE_HEADERS, timeout=60)
if created.status_code not in (200, 201):
    raise SystemExit(f"create failed: HTTP {created.status_code} {created.text[:300]}")
appended = requests.patch(base + "?action=append&position=0", headers=ONELAKE_HEADERS,
                          data=WRITTEN_PAYLOAD, timeout=60)
if appended.status_code not in (200, 202):
    raise SystemExit(f"append failed: HTTP {appended.status_code} {appended.text[:300]}")
flushed = requests.patch(base + f"?action=flush&position={len(WRITTEN_PAYLOAD)}",
                         headers=ONELAKE_HEADERS, timeout=60)
if flushed.status_code != 200:
    raise SystemExit(f"flush failed: HTTP {flushed.status_code} {flushed.text[:300]}")

# The SDK is the oracle: read the blob straight from Azurite.
landed = svc.get_blob_client(CONTAINER, WRITTEN_BLOB).download_blob().readall()
if landed != WRITTEN_PAYLOAD:
    raise SystemExit(f"the storage account holds {landed!r}, not what was written")
print(f"write through the shortcut landed in Azurite ({len(landed)} bytes, verified by SDK)")

# ------------- 7. DELETE through the shortcut removes it at the target
deleted = requests.delete(base, headers=ONELAKE_HEADERS, timeout=60)
if deleted.status_code not in (200, 202):
    raise SystemExit(f"delete failed: HTTP {deleted.status_code} {deleted.text[:300]}")
from azure.core.exceptions import ResourceNotFoundError
try:
    svc.get_blob_client(CONTAINER, WRITTEN_BLOB).download_blob().readall()
    raise SystemExit("the blob still exists in Azurite — the delete never reached the target")
except ResourceNotFoundError:
    print("delete through the shortcut removed the blob at the target (verified by SDK)")

print("Azurite shortcut e2e: PASS — real storage emulator, real SDK, real SAS through OneLake")
