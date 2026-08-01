"""OneLake credentials for the agent's delta-rs path.

Sail and delta-rs take their storage credentials in opposite shapes, and that
difference is the whole reason this module exists.

**Sail** reads them from process env once, at startup: object_store's
`MicrosoftAzureBuilder::from_env` runs before the server binds, which is why
`docker/sail/launcher.py` mints the token and *then* execs `sail`. There is no
per-session credential channel.

**delta-rs** takes `storage_options` on every `DeltaTable(...)` call. So the
agent resolves credentials *per statement* rather than once at import — which
also means a refreshed token is picked up without restarting the agent, the one
thing the Sail side cannot do.

Why the agent mints its own token instead of borrowing the session's: a Spark
Connect client cannot read back the bearer the server holds — no such API
exists, in Sail or in Apache Spark's own Connect client. The agent therefore
performs its own client-credentials grant against the same issuer with the same
Storage audience, so delta-rs authenticates as the same principal Sail does.

Config comes from the same env vars the rest of the stack already uses
(`docker-compose.override.yml`), so there is one place to change a credential:

    AZURE_STORAGE_ACCOUNT_NAME  account name  -> azure_storage_account_name
    AZURE_STORAGE_ENDPOINT      endpoint URL  -> azure_endpoint
    AZURE_STORAGE_TOKEN         static bearer -> azure_storage_token
    AZURE_ALLOW_HTTP                          -> azure_allow_http
    AZURE_ALLOW_INVALID_CERTIFICATES          -> azure_allow_invalid_certificates
    ENTRA_TOKEN_URL / ENTRA_CLIENT_ID / ENTRA_CLIENT_SECRET / ENTRA_SCOPE
        mint a bearer when AZURE_STORAGE_TOKEN is not set

With none of these set, `options()` returns `{}` and delta-rs falls back to its
own env handling — which is what makes local-path tables keep working.
"""
import json
import os
import ssl
import time
import urllib.parse
import urllib.request

# Refresh this long before the token actually expires, so a statement that
# starts just under the wire still finishes with a valid bearer.
_SKEW_SECONDS = 300

_cached = {"token": None, "good_until": 0.0}


def _mint(url, env):
    """Client-credentials grant — the same one docker/sail/launcher.py makes."""
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": env["ENTRA_CLIENT_ID"],
        "client_secret": env["ENTRA_CLIENT_SECRET"],
        "scope": env.get("ENTRA_SCOPE", "https://storage.azure.com/.default"),
    }).encode()
    # entra-emulator serves self-signed TLS on the compose network. This is a
    # local dev tool and the issuer is named by our own config, not by anything
    # user-supplied, so skip verification for the mint — same call the Sail
    # launcher makes, and the emulator's own FABRIC_ENTRA_TLS_INSECURE.
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    with urllib.request.urlopen(urllib.request.Request(url, data=form),
                                timeout=30, context=ctx) as response:
        body = json.loads(response.read())
    return body["access_token"], float(body.get("expires_in", 3600))


def token(env=None):
    """The Storage-audience bearer, minted and cached, or None if unconfigured.

    A static AZURE_STORAGE_TOKEN wins: it is how the image is pointed at real
    Azure, or at a fixed test token, without an issuer to call.
    """
    env = os.environ if env is None else env
    static = env.get("AZURE_STORAGE_TOKEN")
    if static:
        return static
    url = env.get("ENTRA_TOKEN_URL")
    if not url:
        return None
    now = time.monotonic()
    if _cached["token"] and now < _cached["good_until"]:
        return _cached["token"]
    minted, lifetime = _mint(url, env)
    _cached["token"] = minted
    # Never cache for less than a minute even if the issuer returns a short
    # lifetime: minting on every statement would turn a REPL into a token flood.
    _cached["good_until"] = now + max(60.0, lifetime - _SKEW_SECONDS)
    return minted


def options(env=None):
    """Build delta-rs `storage_options` from the environment.

    Returns `{}` when no OneLake account is configured — the local-path case,
    where passing half-populated options would fail more confusingly than
    passing none.
    """
    env = os.environ if env is None else env
    account = env.get("AZURE_STORAGE_ACCOUNT_NAME")
    if not account:
        return {}
    opts = {"azure_storage_account_name": account}
    endpoint = env.get("AZURE_STORAGE_ENDPOINT")
    if endpoint:
        opts["azure_endpoint"] = endpoint
    if env.get("AZURE_ALLOW_HTTP"):
        opts["azure_allow_http"] = env["AZURE_ALLOW_HTTP"]
    if env.get("AZURE_ALLOW_INVALID_CERTIFICATES"):
        opts["azure_allow_invalid_certificates"] = env["AZURE_ALLOW_INVALID_CERTIFICATES"]
    bearer = token(env)
    if bearer:
        opts["azure_storage_token"] = bearer
    return opts
