"""Fetch the API key from Key Vault as a Fabric notebook does, pull the vendor
export, and land it verbatim in OneLake.

`notebookutils.credentials.getSecret` is the BROKERED path, and it is the one a
notebook has: inside Fabric the workspace identity mints a vault-audience token
and the secret never appears in the notebook, a parameter, or a pipeline
definition. Calling Key Vault's REST API with a token minted here reaches the
same secret and teaches the wrong lesson, because that code cannot move into a
notebook unchanged. The shim makes this exact call work outside Fabric too
(python/notebookutils/credentials.py): entra-emulator under `emulator`,
DefaultAzureCredential under `real`.

The vault is passed as a URI, which is also the real signature — Fabric accepts
either a vault name or its URI, and only the URI can name the emulator's vault.
"""
import datetime

import notebookutils
import source_system as src
from common import (
    ONELAKE_BLOB,
    STORAGE_AUD,
    VAULT_AUD,
    S,
    ensure_app,
    load,
    log,
    require_vault,
    save,
    token,
)

ensure_app(VAULT_AUD, "Azure Key Vault")  # a real tenant already issues it
api_key = notebookutils.credentials.getSecret(require_vault(), "contoso-pos-api-key")

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
    r = S.put(f"{ONELAKE_BLOB}/{st['workspace']}/{st['lakehouse']}/{path}",
              data=blob, headers={"Authorization": "Bearer " + st_tok,
                                  "x-ms-blob-type": "BlockBlob"})
    assert r.status_code in (200, 201), f"land {path}: {r.status_code} {r.text}"
    log(f"landed {path} ({len(blob)} bytes)")

save(landing_date=today)
