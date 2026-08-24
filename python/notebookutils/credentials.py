"""notebookutils.credentials — tokens and Key Vault secrets for the notebook.

In real Fabric these resolve through the workspace's managed identity. Under
FABRIC_TARGET=emulator the notebook identity is the client-credentials app
from the runtime context (_config) and tokens come from entra-emulator;
under FABRIC_TARGET=real they come from DefaultAzureCredential (`az login`,
managed identity, or AZURE_* env vars) against real Entra. Either way
getSecret then reads Key Vault with a vault-audience token — the same
identity-brokered path Fabric uses.
"""
import sys as _sys
import urllib.parse

from . import _help
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
    """Real mode: mint through azure-identity, so `az login` just works.

    THE CONFIGURED TENANT MUST REACH THE az CLI PATH, and it did not.
    `DefaultAzureCredential` takes tenant hints for the browser, VS Code,
    shared-cache and workload-identity links and has NONE for the CLI — so a
    developer whose `az` default tenant differs from NOTEBOOKUTILS_TENANT got
    tokens for the wrong tenant from the credential source this shim documents as
    the default. `getSecret` then fails with a vault or licensing error that names
    nothing about tenants.
    """
    global _real_credential
    tenant = config().tenant
    if tenant == "organizations":
        tenant = None  # nothing configured; let the chain decide
    if _real_credential is None:
        try:
            from azure.identity import DefaultAzureCredential
        except ImportError as e:
            raise RuntimeError(
                "notebookutils: FABRIC_TARGET=real needs azure-identity "
                "(`uv add azure-identity`), then `az login` or AZURE_* "
                "credentials in the environment."
            ) from e
        _real_credential = DefaultAzureCredential(
            additionally_allowed_tenants=[tenant] if tenant else None)
    if tenant:
        return _real_credential.get_token(scope, tenant_id=tenant).token
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


def putSecret(akvName, secretName, secretValue):  # noqa: N802,N803 - documented spelling
    """Store `secretValue` at `secretName` in the Key Vault `akvName`.

    The write half of the pair. `getSecret` has been here since the shim
    started and this had not — which is the shape of gap contract 2 exists to
    find: a framework that manages its own secrets introspects for `putSecret`,
    sees nothing, and declines without ever calling anything.
    """
    token = getToken("keyvault")
    url = f"{_vault_url(akvName)}/secrets/{secretName}?api-version=7.4"
    return request("PUT", url, token=token, body={"value": secretValue}).get("value")


def isValidToken(token):  # noqa: N802 - documented spelling
    """Is `token` a JWT that has not expired?

    READ, NOT VERIFIED, and the difference matters enough to state. Fabric's
    own description is "valid and not expired", and its documented use is a
    caller deciding whether to re-mint before a long operation — a question
    about the clock, not about trust. Signature verification belongs to
    whatever accepts the token, and doing it here would need the issuer's JWKS
    and would still not make the answer authoritative.

    A token that cannot be parsed is not valid: unreadable is a "no", because
    the caller's next move on False (mint a fresh one) is the safe one.
    """
    import base64
    import json
    import time

    try:
        payload = token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        claims = json.loads(base64.urlsafe_b64decode(payload))
    except Exception:  # noqa: BLE001 - anything unparseable is not a valid token
        return False
    exp = claims.get("exp")
    if not isinstance(exp, (int, float)):
        # Real Entra always mints `exp`; its absence means this is not a token
        # this method can answer for. Same reasoning as internal/auth, which
        # refuses a token without one rather than treating it as eternal.
        return False
    return time.time() < float(exp)


def help(method_name=None):  # noqa: A001 - Fabric's own spelling, on every module
    """List this module's methods, or document one of them.

    Fabric's `fs` page opens by documenting `notebookutils.fs.help()` as the
    discovery mechanism, and the stubs carry it on every module. Shadows the
    builtin inside this module only, exactly as Microsoft's package does.
    """
    _help.emit(_sys.modules[__name__], method_name)
