"""Store the source system's API key in Key Vault and bind it to Fabric as a
Key credential resolved from that vault — which makes Fabric resolve it for real
with a vault-audience token, so the secret never appears in this payload.

Two connections, not one, and that is Fabric's design rather than ceremony: a
KeyVaultSecretReference names a CONNECTION to the vault (`connectionId`), never a
vault URL. The vault connection holds the credential that reaches the vault; the
data connection holds only a pointer into it."""
from urllib.parse import urlparse

import source_system as src
from common import (
                    FABRIC,
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

# The vault's ACCOUNT NAME, which is what a Fabric connection carries — the URL
# is Fabric's to compose. Derived here rather than added to common.py: the
# resolver's job is to say which target we are on, not to model connectors.
KV_ACCOUNT = urlparse(KV).hostname.split(".")[0]

r = S.put(f"{KV}/secrets/contoso-pos-api-key?api-version=7.4",
          headers={"Authorization": "Bearer " + vt}, json={"value": src.API_KEY})
assert r.status_code in (200, 201), f"put secret: {r.status_code} {r.text}"
log(f"secret stored: {r.json()['id']}")

st = load()

# 1. The vault itself, as a connection. `accountName` is the vault's name;
#    Fabric composes https://{accountName}.vault.azure.net from it, and the
#    emulator resolves it to whichever vault the stack is running.
r = S.post(f"{FABRIC}/v1/connections", headers=fabric_headers(), json={
    "displayName": "contoso-kv",
    "connectivityType": "ShareableCloud",
    "connectionDetails": {
        "type": "AzureKeyVault",
        "creationMethod": "AzureKeyVault.Actions",
        "parameters": [{"dataType": "Text", "name": "accountName",
                        "value": KV_ACCOUNT}]},
    "credentialDetails": {"credentials": {
        "credentialType": "WorkspaceIdentity",
        "workspaceId": st["workspace"]}}})
assert r.status_code == 201, f"vault connection: {r.status_code} {r.text}"
vault_conn = r.json()["id"]

# 2. The data connection, whose key is a POINTER into that vault.
r = S.post(f"{FABRIC}/v1/connections", headers=fabric_headers(), json={
    "displayName": "contoso-pos",
    "connectivityType": "ShareableCloud",
    "connectionDetails": {
        "type": "WebForPipeline",
        "creationMethod": "WebForPipeline.Contents",
        "parameters": [{"dataType": "Text", "name": "baseUrl",
                        "value": "https://pos.contoso.example/v2/export"}]},
    "credentialDetails": {"credentials": {
        "credentialType": "Key",
        "keyReference": {"connectionId": vault_conn,
                         "secretName": "contoso-pos-api-key"}}}})
assert r.status_code == 201, f"key-reference connection: {r.status_code} {r.text}"
conn_id = r.json()["id"]

# Read it back: metadata only — the secret must never come back over the wire.
listed = S.get(f"{FABRIC}/v1/connections", headers=fabric_headers())
listed.raise_for_status()
assert src.API_KEY not in listed.text, "connection listing leaked the secret value"
row = next(c for c in listed.json()["value"] if c["id"] == conn_id)
assert row["credentialDetails"]["credentialType"] == "Key", row
# The read shape is {type, path} — creationMethod and parameters are request-only.
assert "parameters" not in row["connectionDetails"], row

save(connection=conn_id, vault_connection=vault_conn)
log(f"key-reference connection {conn_id} via vault connection {vault_conn}")
