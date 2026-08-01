"""A1 — bring the second source in: Contoso Web.

Its API key gets its own Key Vault secret and its own AKV-reference connection,
because two source systems do not share a credential. Then the export lands
verbatim under its own path, exactly as the POS export does in 02.

Nothing is reshaped here. Landing is the record of what the vendor sent, and
the web store sends nested JSON.
"""
import datetime

import web_store as web
from common import (FABRIC, KV, KV_INTERNAL, S, STORAGE_AUD, VAULT_AUD, ensure_app,
                    fabric_headers, load, log, save, token)

ensure_app(VAULT_AUD, "Azure Key Vault")
vt = token(VAULT_AUD)

r = S.put(f"{KV}/secrets/contoso-web-api-key?api-version=7.4",
          headers={"Authorization": "Bearer " + vt}, json={"value": web.API_KEY})
assert r.status_code in (200, 201), f"put secret: {r.status_code} {r.text}"

st = load()
r = S.post(f"{FABRIC}/v1/connections", headers=fabric_headers(), json={
    "displayName": "contoso-web",
    "connectivityType": "ShareableCloud",
    "connectionDetails": {"type": "RestApi", "path": "https://web.contoso.example/api/v1/export"},
    "credentialDetails": {"credentials": {
        "credentialType": "AzureKeyVaultReference",
        "workspaceId": st["workspace"],
        "vaultUri": KV_INTERNAL,
        "secretName": "contoso-web-api-key"}}})
assert r.status_code == 201, f"AKV connection: {r.status_code} {r.text}"
web_conn = r.json()["id"]

# Two sources, two secrets — check the second one does not leak either. The
# mechanism is proven in 01; what is new is that a listing now spans both
# connections and must stay clean across all of them.
listed = S.get(f"{FABRIC}/v1/connections", headers=fabric_headers())
listed.raise_for_status()
assert web.API_KEY not in listed.text, "connection listing leaked the web secret"
log(f"AKV-reference connection {web_conn} for contoso-web")

# Fetch the key back the way a notebook would, and prove the vendor's gate is
# real rather than decorative.
r = S.get(f"{KV}/secrets/contoso-web-api-key?api-version=7.4",
          headers={"Authorization": "Bearer " + vt})
r.raise_for_status()
api_key = r.json()["value"]
try:
    web.export("wrong-key")
    raise AssertionError("web store accepted a wrong API key")
except PermissionError:
    pass

ensure_app(STORAGE_AUD, "Azure Storage")
st_tok = token(STORAGE_AUD)
today = datetime.date.today().isoformat()

for name, blob in web.export(api_key).items():
    path = f"Files/landing/contoso_web/{today}/{name}"
    r = S.put(f"{FABRIC}/onelake/{st['workspace']}/{st['lakehouse']}/{path}",
              data=blob, headers={"Authorization": "Bearer " + st_tok,
                                  "x-ms-blob-type": "BlockBlob"})
    assert r.status_code in (200, 201), f"land {path}: {r.status_code} {r.text}"
    log(f"landed {path} ({len(blob)} bytes)")

save(web_connection=web_conn, web_landing_date=today)
