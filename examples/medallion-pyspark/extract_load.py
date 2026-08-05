"""Fetch the API key from Key Vault (as notebookutils.credentials.getSecret
does), pull the vendor export, and land it verbatim in OneLake."""
import datetime

import source_system as src
from common import FABRIC, KV, STORAGE_AUD, VAULT_AUD, S, ensure_app, load, log, save, token

vt = token(VAULT_AUD)
r = S.get(f"{KV}/secrets/contoso-pos-api-key?api-version=7.4",
          headers={"Authorization": "Bearer " + vt})
r.raise_for_status()
api_key = r.json()["value"]

# The vendor refuses a wrong key — prove the gate is real, not decorative.
try:
    src.export("wrong-key")
    raise AssertionError("source system accepted a wrong API key")
except PermissionError:
    pass

ensure_app(STORAGE_AUD, "Azure Storage")
st_tok = token(STORAGE_AUD)
st = load()
today = datetime.date.today().isoformat()

for name, blob in src.export(api_key).items():
    path = f"Files/landing/contoso_pos/{today}/{name}"
    r = S.put(f"{FABRIC}/onelake/{st['workspace']}/{st['lakehouse']}/{path}",
              data=blob, headers={"Authorization": "Bearer " + st_tok,
                                  "x-ms-blob-type": "BlockBlob"})
    assert r.status_code in (200, 201), f"land {path}: {r.status_code} {r.text}"
    log(f"landed {path} ({len(blob)} bytes)")

save(landing_date=today)
