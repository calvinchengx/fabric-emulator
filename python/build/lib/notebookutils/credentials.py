"""notebookutils.credentials — tokens and Key Vault secrets for the notebook.

In real Fabric these resolve through the workspace's managed identity. Here the
notebook identity is the client-credentials app from the runtime context
(_config), and tokens come from the entra-emulator. getSecret then reads the
azure-keyvault-emulator with a vault-audience token — the same identity-brokered
path Fabric uses, just against the local family.
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


def getToken(audience):
    """Return a bearer token for `audience` (a Fabric alias like "storage"/
    "keyvault" or a full resource URL), minted for the notebook identity."""
    cfg = config()
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
