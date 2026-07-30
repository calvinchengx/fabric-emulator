"""notebookutils.credentials — tokens and Key Vault secrets for the notebook.

In real Fabric these resolve through the workspace's managed identity. Under
FABRIC_TARGET=emulator the notebook identity is the client-credentials app
from the runtime context (_config) and tokens come from entra-emulator;
under FABRIC_TARGET=real they come from DefaultAzureCredential (`az login`,
managed identity, or AZURE_* env vars) against real Entra. Either way
getSecret then reads Key Vault with a vault-audience token — the same
identity-brokered path Fabric uses.
"""
import urllib.parse

from ._config import config
from ._http import request

# Fabric's friendly audience aliases → the resource the STS mints for.
_AUDIENCE = {
    "storage": "https://storage.azure.com",
    "keyvault": "https://vault.azure.net",
    "vault": "https://vault.azure.net",
    "pbi": "https://api.fabric.microsoft.com",
    "fabric": "https://api.fabric.microsoft.com",
}


def _scope(audience):
    """Resolve a friendly name or resource URL to a `.default` scope."""
    resource = _AUDIENCE.get(audience, audience)
    if resource.endswith("/.default"):
        return resource
    return resource.rstrip("/") + "/.default"


# DefaultAzureCredential is constructed once and cached: it probes its chain
# (env SP -> managed identity -> az login) on first use, which is slow.
_real_credential = None


def _real_token(scope):
    """Real mode: mint through azure-identity, so `az login` just works."""
    global _real_credential
    if _real_credential is None:
        try:
            from azure.identity import DefaultAzureCredential
        except ImportError as e:
            raise RuntimeError(
                "notebookutils: FABRIC_TARGET=real needs azure-identity "
                "(`uv add azure-identity`), then `az login` or AZURE_* "
                "credentials in the environment."
            ) from e
        _real_credential = DefaultAzureCredential()
    return _real_credential.get_token(scope).token


def getToken(audience):
    """Return a bearer token for `audience` (a Fabric alias like "storage"/
    "keyvault" or a full resource URL), minted for the notebook identity."""
    cfg = config()
    if cfg.is_real:
        return _real_token(_scope(audience))
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": cfg.client_id,
        "client_secret": cfg.client_secret,
        "scope": _scope(audience),
    }).encode()
    resp = request(
        "POST", f"{cfg.entra_url}/{cfg.tenant}/oauth2/v2.0/token",
        body=form, headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    return resp["access_token"]


def _vault_url(akvName):
    # A configured override wins (the emulator's default vault); then a full
    # URL passed as the vault; then the standard vault DNS suffix.
    override = config().vault_url
    if override:
        return override.rstrip("/")
    if akvName.startswith("http://") or akvName.startswith("https://"):
        return akvName.rstrip("/")
    return f"https://{akvName}.vault.azure.net"


def getSecret(akvName, secret, linkedService=None):
    """Read `secret` from the Key Vault `akvName` (name or full URL), brokered
    through a vault-audience token — mirrors Fabric's Key Vault integration."""
    token = getToken("keyvault")
    url = f"{_vault_url(akvName)}/secrets/{secret}?api-version=7.4"
    return request("GET", url, token=token)["value"]


def getSecretWithLS(linkedService, secret):
    """Compatibility alias for the linked-service overload."""
    return getSecret(linkedService, secret)
