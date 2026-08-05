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
from contextlib import contextmanager

# Refresh this long before the token actually expires, so a statement that
# starts just under the wire still finishes with a valid bearer.
_SKEW_SECONDS = 300

# Two values with two types, kept apart rather than in one loosely-typed dict:
# `good_until` is compared against a monotonic clock, and a dict holding both a
# token and a deadline makes that comparison unverifiable.
_cached_token: str | None = None
_cached_good_until: float = 0.0


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
    global _cached_token, _cached_good_until
    env = os.environ if env is None else env
    static = env.get("AZURE_STORAGE_TOKEN")
    if static:
        return static
    url = env.get("ENTRA_TOKEN_URL")
    if not url:
        return None
    now = time.monotonic()
    if _cached_token and now < _cached_good_until:
        return _cached_token
    minted, lifetime = _mint(url, env)
    _cached_token = minted
    # Never cache for less than a minute even if the issuer returns a short
    # lifetime: minting on every statement would turn a REPL into a token flood.
    _cached_good_until = now + max(60.0, lifetime - _SKEW_SECONDS)
    return minted



@contextmanager
def cell_context(job, cell):
    """Export the running cell's identity for the duration of one statement.

    Nothing set FABRIC_JOB_ID / FABRIC_CELL_INDEX before this existed — they had
    only readers, so `_forge_attributed` below always returned None and
    `notebookutils.fs` tagged nothing. Both were dead paths, and a driven
    notebook's I/O was attributed to no cell at all.

    RESTORED, not cleared. The agent is long-lived and serves interleaved
    sessions; leaving one cell's identity set would attribute the NEXT
    statement's I/O to it. A wrong lineage edge is worse than a missing one —
    nothing about it looks wrong. Absent job/cell (an ordinary Livy statement)
    this sets nothing at all.
    """
    if not job or cell is None:
        yield
        return
    keys = ("FABRIC_JOB_ID", "FABRIC_CELL_INDEX")
    prev = {k: os.environ.get(k) for k in keys}
    os.environ["FABRIC_JOB_ID"] = str(job)
    os.environ["FABRIC_CELL_INDEX"] = str(cell)
    try:
        yield
    finally:
        for k in keys:
            # Bound to a local so the None check actually narrows: a dict
            # subscript re-reads, so `prev[k]` after `prev[k] is None` is still
            # `str | None` to a reader and to a checker alike.
            old = prev[k]
            if old is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = old


def _forge_attributed(env):
    """Mint a storage token carrying notebook attribution as extra claims.

    delta-rs takes credentials, not HTTP options: Rust object_store exposes no
    way to add a request header, so the cell identity cannot ride alongside the
    request the way notebookutils sends it. It can ride *inside* the bearer —
    entra's token forge merges `extraClaims`, and the emulator surfaces them on
    the validated principal, so its storage layer attributes the I/O to the cell
    that caused it.

    Returns None when no cell context is set, so ordinary statements keep using
    the cached client-credentials token.
    """
    job, cell = env.get("FABRIC_JOB_ID"), env.get("FABRIC_CELL_INDEX")
    forge = env.get("ENTRA_FORGE_URL")
    if not job or not cell or not forge:
        return None
    body = json.dumps({
        "clientId": env["ENTRA_CLIENT_ID"],
        "audience": env.get("ENTRA_AUDIENCE", "https://storage.azure.com"),
        "extraClaims": {"fabric_job_id": job, "fabric_cell_index": str(cell)},
    }).encode()
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    req = urllib.request.Request(forge, data=body, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30, context=ctx) as response:
            forged = json.loads(response.read())
    except Exception:  # noqa: BLE001 — attribution is best effort, never fatal
        return None
    return forged.get("access_token") or forged.get("token")


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
    bearer = _forge_attributed(env) or token(env)
    if bearer:
        opts["azure_storage_token"] = bearer
    return opts
