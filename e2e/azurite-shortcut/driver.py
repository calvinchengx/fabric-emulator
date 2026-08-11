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
from datetime import UTC, datetime, timedelta

import requests
from azure.storage.blob import BlobSasPermissions, BlobServiceClient, generate_blob_sas

FABRIC = os.environ.get("FABRIC", "http://fabric-emulator")
ENTRA = os.environ.get("ENTRA", "http://entra-emulator:8443")
AZURITE = os.environ.get("AZURITE", "http://azurite:10000")
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"

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
    expiry=datetime.now(UTC) + timedelta(hours=1),
)
ws = fabric_post("/v1/workspaces", {"displayName": "adls-shortcuts"})
lakehouse = fabric_post(f"/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lh"})

# SharedAccessSignature is a documented CredentialType; `token` is its
# documented field (core/connections/create-connection).
conn = fabric_post("/v1/connections", {
    "displayName": "azurite-adls",
    "connectivityType": "ShareableCloud",
    "connectionDetails": {"type": "AzureDataLakeStorage",
                          "creationMethod": "AzureDataLakeStorage",
                          "parameters": [{"dataType": "Text", "name": "server", "value": "external-store"}, {"dataType": "Text", "name": "path", "value": "/"}]},
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
    "connectionDetails": {"type": "AzureDataLakeStorage",
                          "creationMethod": "AzureDataLakeStorage",
                          "parameters": [{"dataType": "Text", "name": "server", "value": "external-store"}, {"dataType": "Text", "name": "path", "value": "/"}]},
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
    expiry=datetime.now(UTC) + timedelta(hours=1),
)
write_conn = fabric_post("/v1/connections", {
    "displayName": "azurite-adls-rw",
    "connectivityType": "ShareableCloud",
    "connectionDetails": {"type": "AzureDataLakeStorage",
                          "creationMethod": "AzureDataLakeStorage",
                          "parameters": [{"dataType": "Text", "name": "server", "value": "external-store"}, {"dataType": "Text", "name": "path", "value": "/"}]},
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

print("ADLS Gen2 half: PASS — real storage emulator, real SDK, real SAS through OneLake")

# ================================================================ Dataverse
# A Dataverse TARGET TYPE in this same shortcut machinery — not a Dataverse
# emulator. What Microsoft documents is the target's four fields
# (connectionId, deltaLakeFolder, environmentDomain, tableName) and the rule
# that "Dataverse shortcuts are read-only. They don't support write operations
# regardless of the user's permissions." What Microsoft does NOT document is
# the byte layout of the Dataverse Managed Lake, so the composition
# environmentDomain/deltaLakeFolder/tableName is OURS, and the parity row says
# so. Azurite stands in for the endpoint exactly as it does for ADLS above.
#
# Two honest divergences, stated here so the witness is not read as more than
# it is:
#   * Dataverse's delegated authorization is organizational account (OAuth2)
#     or service principal. This presents a SAS, because that is what Azurite
#     verifies. What is witnessed is that the shortcut's credential is really
#     presented and really checked — not that OAuth2 delegation works.
#   * The shortcut is created under **Files**, not Tables. Tables is a managed
#     folder with its own write guards, and a refusal there would not prove
#     the read-only rule — it would prove the guard. Under Files the only
#     thing that can refuse the write is the target type.
DV_CONTAINER = "dvlake"
DV_BLOB = "deltalake/account/part-0000.parquet"
DV_PAYLOAD = b"accountid,name\n1,Contoso Ltd\n"

svc_dv = BlobServiceClient.from_connection_string(conn_str)
try:
    svc_dv.create_container(DV_CONTAINER)
except Exception:
    pass
svc_dv.get_blob_client(DV_CONTAINER, DV_BLOB).upload_blob(DV_PAYLOAD, overwrite=True)

dv_sas = generate_blob_sas(
    account_name=ACCOUNT, container_name=DV_CONTAINER, blob_name=DV_BLOB,
    account_key=ACCOUNT_KEY,
    # read+write deliberately: if the emulator ever DID forward a write, the
    # credential would not be what stopped it. The refusal has to come from
    # the target type, so the credential must not be the limiting factor.
    permission=BlobSasPermissions(read=True, write=True, create=True, delete=True),
    expiry=datetime.now(UTC) + timedelta(hours=1),
)
dv_conn = fabric_post("/v1/connections", {
    "displayName": "dataverse-env",
    "connectivityType": "ShareableCloud",
    "connectionDetails": {"type": "AzureDataLakeStorage",
                          "creationMethod": "AzureDataLakeStorage",
                          "parameters": [{"dataType": "Text", "name": "server", "value": "external-store"}, {"dataType": "Text", "name": "path", "value": "/"}]},
    "credentialDetails": {"credentials": {
        "credentialType": "SharedAccessSignature", "token": dv_sas,
    }},
})
created_dv = fabric_post(
    f"/v1/workspaces/{ws['id']}/items/{lakehouse['id']}/shortcuts",
    {"path": "Files", "name": "dvaccount",
     "target": {"dataverse": {
         "environmentDomain": f"{AZURITE}/{ACCOUNT}",
         "deltaLakeFolder": f"{DV_CONTAINER}/deltalake",
         "tableName": "account",
         "connectionId": dv_conn["id"],
     }}},
)
# The response must echo the four documented fields, not the storage-shaped
# location/subpath the other external targets use.
dv_target = created_dv["target"]
if dv_target.get("type") != "Dataverse" or "dataverse" not in dv_target:
    raise SystemExit(f"create response is not a Dataverse target: {created_dv}")
echoed = dv_target["dataverse"]
if echoed.get("tableName") != "account" or echoed.get("deltaLakeFolder") != f"{DV_CONTAINER}/deltalake":
    raise SystemExit(f"the documented fields did not round-trip: {echoed}")
print("created Dataverse shortcut dvaccount, four documented fields echoed back")

# ---- read through: the emulator composes domain + folder + table + remainder
dv_read = requests.get(
    f"{FABRIC}/{ws['id']}/{lakehouse['id']}/Files/dvaccount/part-0000.parquet",
    headers=ONELAKE_HEADERS, timeout=60,
)
if dv_read.status_code != 200:
    raise SystemExit(f"Dataverse read-through failed: HTTP {dv_read.status_code} {dv_read.text[:400]}")
if dv_read.content != DV_PAYLOAD:
    raise SystemExit(f"Dataverse read-through content mismatch: {dv_read.content!r}")
print(f"Dataverse read-through returned the real blob ({len(dv_read.content)} bytes)")

# ---- THE DOCUMENTED RULE: writes are refused, and nothing lands at the target
dv_base = f"{FABRIC}/{ws['id']}/{lakehouse['id']}/Files/dvaccount/written.parquet"
requests.put(dv_base + "?resource=file", headers=ONELAKE_HEADERS, timeout=60)
requests.patch(dv_base + "?action=append&position=0", headers=ONELAKE_HEADERS,
               data=b"nope", timeout=60)
dv_flush = requests.patch(dv_base + "?action=flush&position=4", headers=ONELAKE_HEADERS, timeout=60)
if dv_flush.status_code == 200:
    raise SystemExit("flush through a Dataverse shortcut succeeded — Fabric documents "
                     "them read-only regardless of the user's permissions")
print(f"write through the Dataverse shortcut correctly refused: HTTP {dv_flush.status_code}")

# The emulator's refusal is only half the claim; the other half is that the
# storage account never received it. The SDK is the oracle, as it is above.
from azure.core.exceptions import ResourceNotFoundError as _NotFound

try:
    svc_dv.get_blob_client(DV_CONTAINER, "deltalake/account/written.parquet").download_blob().readall()
    raise SystemExit("the refused write reached the storage account anyway")
except _NotFound:
    print("the refused write never reached the target (verified by SDK)")

# ---- delete is the same rule, and it is a separate code path
dv_del = requests.delete(
    f"{FABRIC}/{ws['id']}/{lakehouse['id']}/Files/dvaccount/part-0000.parquet",
    headers=ONELAKE_HEADERS, timeout=60,
)
if dv_del.status_code == 200:
    raise SystemExit("delete through a Dataverse shortcut succeeded")
still_there = svc_dv.get_blob_client(DV_CONTAINER, DV_BLOB).download_blob().readall()
if still_there != DV_PAYLOAD:
    raise SystemExit("the refused delete removed data at the target")
print(f"delete through the Dataverse shortcut correctly refused: HTTP {dv_del.status_code}, "
      "and the target still holds the blob")

# ---- negative control on auth: a tampered SAS must not read
dv_bad = fabric_post("/v1/connections", {
    "displayName": "dataverse-env-bad",
    "connectivityType": "ShareableCloud",
    "connectionDetails": {"type": "AzureDataLakeStorage",
                          "creationMethod": "AzureDataLakeStorage",
                          "parameters": [{"dataType": "Text", "name": "server", "value": "external-store"}, {"dataType": "Text", "name": "path", "value": "/"}]},
    "credentialDetails": {"credentials": {
        "credentialType": "SharedAccessSignature", "token": dv_sas.replace("sig=", "sig=00"),
    }},
})
fabric_post(
    f"/v1/workspaces/{ws['id']}/items/{lakehouse['id']}/shortcuts",
    {"path": "Files", "name": "dvbad",
     "target": {"dataverse": {
         "environmentDomain": f"{AZURITE}/{ACCOUNT}",
         "deltaLakeFolder": f"{DV_CONTAINER}/deltalake",
         "tableName": "account",
         "connectionId": dv_bad["id"],
     }}},
)
dv_denied = requests.get(
    f"{FABRIC}/{ws['id']}/{lakehouse['id']}/Files/dvbad/part-0000.parquet",
    headers=ONELAKE_HEADERS, timeout=60,
)
if dv_denied.status_code == 200:
    raise SystemExit("a tampered SAS still read through the Dataverse shortcut")
print(f"tampered credential refused through the Dataverse shortcut: HTTP {dv_denied.status_code}")

print("Dataverse shortcut e2e: PASS — read-through composed from the documented "
      "fields, writes and deletes refused, credential really checked")
