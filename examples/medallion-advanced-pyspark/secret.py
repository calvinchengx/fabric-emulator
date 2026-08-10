"""Store the source system's API key in Key Vault and bind it to Fabric as an
AzureKeyVaultReference connection — which makes Fabric resolve it for real with
a vault-audience workspace-identity token."""
import source_system as src
from common import (
                    FABRIC,
                    KV_INTERNAL,
                    VAULT_AUD,
                    S,
                    ensure_app,
                    fabric_headers,
                    load,
                    log,
                    require_vault,
                    save,
                    token,
)

ensure_app(VAULT_AUD, "Azure Key Vault")
KV = require_vault()  # this step is the one that needs it
vt = token(VAULT_AUD)

r = S.put(f"{KV}/secrets/contoso-pos-api-key?api-version=7.4",
          headers={"Authorization": "Bearer " + vt}, json={"value": src.API_KEY})
assert r.status_code in (200, 201), f"put secret: {r.status_code} {r.text}"
log(f"secret stored: {r.json()['id']}")

st = load()
r = S.post(f"{FABRIC}/v1/connections", headers=fabric_headers(), json={
    "displayName": "contoso-pos",
    "connectivityType": "ShareableCloud",
    "connectionDetails": {"type": "RestApi", "path": "https://pos.contoso.example/v2/export"},
    "credentialDetails": {"credentials": {
        "credentialType": "AzureKeyVaultReference",
        "workspaceId": st["workspace"],
        "vaultUri": KV_INTERNAL,
        "secretName": "contoso-pos-api-key"}}})
assert r.status_code == 201, f"AKV connection: {r.status_code} {r.text}"
conn_id = r.json()["id"]

# Read it back: metadata only — the secret must never come back over the wire.
listed = S.get(f"{FABRIC}/v1/connections", headers=fabric_headers())
listed.raise_for_status()
assert src.API_KEY not in listed.text, "connection listing leaked the secret value"
row = next(c for c in listed.json()["value"] if c["id"] == conn_id)
assert row["credentialDetails"]["credentialType"] == "AzureKeyVaultReference", row

save(connection=conn_id)
log(f"AKV-reference connection {conn_id} (no secret in the read shape)")
