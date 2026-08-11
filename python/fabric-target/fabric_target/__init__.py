"""One toggle between fabric-emulator and real Microsoft Fabric.

    from fabric_target import target
    t = target()                      # reads FABRIC_TARGET (default: emulator)
    ws = t.workspace("analytics")     # name -> id, either target
    s = t.session()                   # authed requests.Session, 429/LRO aware
    s.post(f"/workspaces/{ws.id}/items", json={...})

Design: docs/21-real-fabric-toggle.md. User code holds *names* and receives
endpoints/credentials — it never branches on which target is active. The
resolver does, once, here.
"""
import base64
import json
import os
import shutil
import ssl
import subprocess
import time
import urllib.parse
import urllib.request
from collections import namedtuple

FABRIC_SCOPE = "https://api.fabric.microsoft.com/.default"

STORAGE_SCOPE = "https://storage.azure.com/.default"
VAULT_SCOPE = "https://vault.azure.net/.default"
# A refused connection is retried; see _send_with_connect_retry. Small, because
# the observed rate was ~1 in 25 and the point is to survive a blip, not to sit
# out an outage.
_CONNECT_ATTEMPTS = 4
_CONNECT_BACKOFF = 0.5

# entra-emulator's seeded dev defaults — emulator mode only, by construction.
SEED_TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
SEED_CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
SEED_CLIENT_SECRET = "daemon-app-secret"

AccessToken = namedtuple("AccessToken", ["token", "expires_on"])
Workspace = namedtuple("Workspace", ["id", "display_name"])


class TargetError(RuntimeError):
    """A target rule was violated (emulator-only feature under real, missing
    credentials, unguarded destructive call, ...)."""


def _env(name, default=None):
    v = os.environ.get(name)
    return v if v not in (None, "") else default


def _env_any(names, default=None):
    """First non-empty of `names`, in order.

    Emulator mode names its own knobs FABRIC_*/*_EMULATOR_URL, but a consumer
    driving BOTH targets from one compose file writes the Azure names, because
    real mode requires them. Accepting both is what lets such a consumer adopt
    this package without rewriting its environment contract — the FABRIC_* name
    still wins, so nothing that already sets it changes behaviour.
    """
    for n in names:
        v = _env(n)
        if v is not None:
            return v
    return default


class _EmulatorCredential:
    """TokenCredential-shaped client-credentials flow against entra-emulator.

    Structurally compatible with azure-core's TokenCredential protocol
    (get_token(*scopes) -> .token/.expires_on), so Azure SDK clients accept it
    without azure-identity installed. Stdlib-only; tolerates the family's
    self-signed TLS.
    """

    def __init__(self, entra_url, tenant, client_id, client_secret):
        self._url = f"{entra_url}/{tenant}/oauth2/v2.0/token"
        self._id = client_id
        self._secret = client_secret
        self._cache = {}

    def get_token(self, *scopes, **_):
        scope = scopes[0] if scopes else FABRIC_SCOPE
        tok = self._cache.get(scope)
        if tok and tok.expires_on - time.time() > 60:
            return tok
        body = urllib.parse.urlencode({
            "grant_type": "client_credentials",
            "client_id": self._id,
            "client_secret": self._secret,
            "scope": scope,
        }).encode()
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        req = urllib.request.Request(self._url, data=body, method="POST")
        with urllib.request.urlopen(req, context=ctx, timeout=10) as r:
            payload = json.load(r)
        claims = json.loads(base64.urlsafe_b64decode(
            payload["access_token"].split(".")[1] + "=="))
        tok = AccessToken(payload["access_token"], int(claims["exp"]))
        self._cache[scope] = tok
        return tok


def _az_logged_in():
    az = shutil.which("az")
    if not az:
        return False
    try:
        return subprocess.run([az, "account", "show", "-o", "none"],
                              capture_output=True, timeout=15).returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


class _TenantScoped:
    """A TokenCredential that mints for ONE tenant, whatever the CLI's default is.

    Thin on purpose: it forwards `tenant_id` and nothing else, so the underlying
    chain keeps deciding WHICH credential answers. A caller that passes its own
    tenant_id still wins, because a per-call intent is more specific than a
    configured default.
    """

    def __init__(self, inner, tenant):
        self._inner, self._tenant = inner, tenant

    def get_token(self, *scopes, **kw):
        kw.setdefault("tenant_id", self._tenant)
        return self._inner.get_token(*scopes, **kw)

    def get_token_info(self, *scopes, **kw):
        # Newer azure-core clients prefer this; forwarding keeps the wrapper from
        # silently downgrading a caller to the older shape.
        kw.setdefault("tenant_id", self._tenant)
        return self._inner.get_token_info(*scopes, **kw)


class Target:
    def __init__(self, name):
        if name not in ("emulator", "real"):
            raise TargetError(f"FABRIC_TARGET must be 'emulator' or 'real', got {name!r}")
        self.name = name
        self._credential = None
        self._workspaces = None

        if self.is_emulator:
            fabric = _env_any(("FABRIC_EMULATOR_URL", "FABRIC_URL"),
                              "https://localhost:9443").rstrip("/")
            self.api_root = fabric + "/v1"
            self.entra_url = _env_any(("ENTRA_EMULATOR_URL", "ENTRA_URL"),
                                      "https://localhost:8443").rstrip("/")
            self.tenant = _env_any(("FABRIC_TENANT", "AZURE_TENANT_ID"), SEED_TENANT)
            self.onelake_url = fabric  # Host-routed / account-prefixed locally
            self.vault_url = _env_any(("VAULT_EMULATOR_URL", "AZURE_KEY_VAULT_URL"),
                                      "https://localhost:8444").rstrip("/")
            # No capacity concept locally: compute is whatever sidecar is
            # attached, and there is nothing to bill or to assign a workspace to.
            self.capacity_id = None
            self.tls_verify = False
            self.workspace_scope = _env("FABRIC_WORKSPACE")  # optional locally
        else:
            self.api_root = "https://api.fabric.microsoft.com/v1"
            self.tenant = _env("AZURE_TENANT_ID", "organizations")
            self.entra_url = "https://login.microsoftonline.com"
            self.onelake_url = "https://onelake.dfs.fabric.microsoft.com"
            # The user's real vault. AZURE_KEY_VAULT_URL is the name the Azure
            # SDKs' own samples use, so accept it too rather than making a
            # consumer set the same URL twice.
            self.vault_url = _env_any(("FABRIC_VAULT_URL", "AZURE_KEY_VAULT_URL"))
            # The capacity a newly created workspace is assigned to. Only needed
            # when something creates a workspace; resolving an existing one by
            # name never touches it. A trial capacity is a valid value, with one
            # caveat that is Microsoft's, not ours: only the account that started
            # the trial can assign a workspace to it, so a service principal
            # cannot — use az login as that user.
            self.capacity_id = _env_any(("FABRIC_CAPACITY_ID", "AZURE_FABRIC_CAPACITY_ID"))
            self.tls_verify = True
            # Real mode is always workspace-scoped: nothing may enumerate a tenant.
            self.workspace_scope = _env("FABRIC_WORKSPACE")
            if not self.workspace_scope:
                raise TargetError(
                    "FABRIC_TARGET=real requires FABRIC_WORKSPACE=<workspace display name> "
                    "— real mode never operates tenant-wide.")
            # NEVER the seeds. Emulator mode accepts the AZURE_* names as
            # aliases, so a shell left over from a local run can carry the
            # seeded daemon into real mode — where it would authenticate
            # against nothing and fail far from its cause, or worse, be taken
            # for a real principal. Refuse by value, not by variable name.
            if _env("AZURE_CLIENT_SECRET") == SEED_CLIENT_SECRET \
                    or _env("AZURE_CLIENT_ID") == SEED_CLIENT_ID \
                    or _env("AZURE_TENANT_ID") == SEED_TENANT:
                raise TargetError(
                    "FABRIC_TARGET=real was given the emulator's SEEDED credential "
                    "— unset AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET, "
                    "or set your own. The seeds exist only locally.")
            # A real credential source, never seeds: env SP or a live az login.
            if not _env("AZURE_CLIENT_SECRET") and not _az_logged_in():
                raise TargetError(
                    "FABRIC_TARGET=real needs a credential source: run `az login`, "
                    "or set AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET.")

    # -- identity ---------------------------------------------------------
    @property
    def is_emulator(self):
        return self.name == "emulator"

    @property
    def is_real(self):
        return self.name == "real"

    @property
    def credential(self):
        """A TokenCredential for this target. Emulator: seeded client
        credentials against entra-emulator (stdlib). Real: DefaultAzureCredential
        — env SP vars win when set, otherwise the developer's az login."""
        if self._credential is None:
            if self.is_emulator:
                self._credential = _EmulatorCredential(
                    self.entra_url, self.tenant,
                    _env_any(("FABRIC_CLIENT_ID", "AZURE_CLIENT_ID"), SEED_CLIENT_ID),
                    _env_any(("FABRIC_CLIENT_SECRET", "AZURE_CLIENT_SECRET"),
                             SEED_CLIENT_SECRET))
            else:
                try:
                    from azure.identity import DefaultAzureCredential
                except ImportError as e:
                    raise TargetError(
                        "real mode needs azure-identity: uv add 'fabric-target[real]'"
                    ) from e
                # AZURE_TENANT_ID MUST REACH THE az CLI PATH.
                #
                # DefaultAzureCredential does not forward it there: it accepts
                # tenant hints for the browser, VS Code, shared-cache and
                # workload-identity links and has no azure_cli_tenant_id. So a
                # developer whose `az` default tenant is not the tenant they
                # configured got tokens for the WRONG tenant, from the credential
                # source docs/21 documents as the default — and Fabric answered
                # `UserNotLicensed`, which reads as a licensing problem rather
                # than a tenant one. Measured against a real trial: the capacity
                # was listable with `az account get-access-token --tenant <id>`
                # and invisible to this credential at the same moment.
                #
                # Baked in rather than passed per call, because callers hold the
                # credential and call get_token(scope) themselves — notebookutils
                # and the examples both do.
                inner = DefaultAzureCredential(
                    additionally_allowed_tenants=[self.tenant] if self.tenant else None)
                self._credential = (_TenantScoped(inner, self.tenant)
                                    if self.tenant and self.tenant != "organizations"
                                    else inner)
        return self._credential

    # -- guards ------------------------------------------------------------
    def emulator_only(self, feature):
        """Declare a spot that has no real-Fabric counterpart (clock control,
        fault injection, seeded principals). Raises under real."""
        if self.is_real:
            raise TargetError(f"{feature}: emulator-only — this does not exist on real Fabric")

    def _complete_workspace_create(self, method, url, kw):
        """Add `capacityId` to a real-Fabric workspace create, or refuse.

        WHY THE SESSION DOES THIS. On real Fabric a workspace with no capacity
        accepts the create and then rejects every Fabric item in it, so
        `POST /workspaces {"displayName": ...}` returns 201 and the next call
        fails with a message about the item rather than the capacity. The
        emulator has no capacity concept at all, which is why portable code does
        not name one — and why completing the request per target belongs here,
        next to the bearer token the session already adds for the same reason.

        Nothing is injected when the caller supplied a capacityId, when the
        target is the emulator, or when the URL is anything other than the
        workspaces COLLECTION (`/workspaces/{id}/assignToCapacity` is a
        different call and must pass through untouched).

        Refusing when no capacity is configured is the actionable half: the
        alternative is a 201 followed by a confusing failure one call later.
        """
        if not self.is_real or method.upper() != "POST":
            return
        if urllib.parse.urlsplit(url).path.rstrip("/") != urllib.parse.urlsplit(
                self.api_root + "/workspaces").path:
            return
        body = kw.get("json")
        if not isinstance(body, dict) or body.get("capacityId"):
            return
        if not self.capacity_id:
            raise TargetError(
                "creating a workspace on real Fabric needs a capacity: set "
                "FABRIC_CAPACITY_ID to a Fabric or trial capacity id (GET "
                "/v1/capacities lists them). Without one the workspace is created "
                "with no capacity and every lakehouse, warehouse and notebook in "
                "it is then rejected — a failure one call away from its cause. "
                "The emulator has no capacity, so nothing is needed there.")
        body["capacityId"] = self.capacity_id

    def _guard_destructive(self, method, url):
        if self.is_real and method.upper() == "DELETE" \
                and _env("FABRIC_TARGET_ALLOW_DESTRUCTIVE") not in ("1", "true", "yes"):
            raise TargetError(
                f"DELETE {url} against real Fabric blocked — set "
                "FABRIC_TARGET_ALLOW_DESTRUCTIVE=1 to allow destructive calls.")

    # -- control plane -----------------------------------------------------
    def session(self):
        """requests.Session bound to this target: api_root-relative URLs,
        bearer auth (auto-refresh), TLS mode, one Retry-After-honoring retry
        on 429, and the real-mode destructive gate."""
        try:
            import requests
        except ImportError as e:
            raise TargetError(
                "session() needs requests: uv add 'fabric-target[sessions]'") from e

        if not self.tls_verify:
            # Self-signed family certs are the norm locally; the warning would
            # fire on every call. Real mode keeps full verification (and any
            # warnings) intact.
            import urllib3
            urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

        t = self

        class _Session(requests.Session):
            def request(self, method, url, **kw):
                if url.startswith("/"):
                    url = t.api_root + url
                t._guard_destructive(method, url)
                kw.setdefault("verify", t.tls_verify)
                headers = kw.setdefault("headers", {})
                headers.setdefault(
                    "Authorization", "Bearer " + t.credential.get_token(FABRIC_SCOPE).token)
                t._complete_workspace_create(method, url, kw)
                resp = _send_with_connect_retry(super().request, method, url, **kw)
                if resp.status_code == 429:  # real Fabric throttles; honor it once
                    time.sleep(min(float(resp.headers.get("Retry-After", "1")), 60))
                    resp = _send_with_connect_retry(super().request, method, url, **kw)
                return resp

        def _send_with_connect_retry(send, method, url, **kw):
            """Retry a request whose CONNECTION was refused.

            MEASURED against real Fabric on 2026-08-11: 1 request in 25, polling
            at 0.3s, came back `[Errno 61] Connection refused`. This session
            already honoured `Retry-After` on a 429 — a response, which means the
            service answered — but had nothing for a connection that was never
            established, so a single refusal raised straight out of `requests`
            and killed the caller.

            Safe for ANY method, including POST: a refused connection carried no
            bytes, so the service cannot have acted on it. Everything else is
            left alone, because a failure after the request landed could mean the
            work was done and a retry would duplicate it.
            """
            for attempt in range(_CONNECT_ATTEMPTS):
                try:
                    return send(method, url, **kw)
                except requests.exceptions.ConnectionError:
                    if attempt == _CONNECT_ATTEMPTS - 1:
                        raise
                    time.sleep(_CONNECT_BACKOFF * (2 ** attempt))
            raise AssertionError("unreachable")  # pragma: no cover

        return _Session()

    def poll_lro(self, response, timeout=600):
        """Follow a 202's operation to a terminal state, honoring Retry-After.
        Instant against the emulator's default clock; real minutes on real."""
        if response.status_code != 202:
            return response
        op = response.headers.get("Location") \
            or self.api_root + "/operations/" + response.headers["x-ms-operation-id"]
        s = self.session()
        end = time.time() + timeout
        while time.time() < end:
            r = s.get(op)
            status = (r.json() or {}).get("status")
            if status in ("Succeeded", "Failed"):
                return r
            time.sleep(min(float(r.headers.get("Retry-After", "1")), 30))
        raise TargetError(f"operation did not reach a terminal state in {timeout}s: {op}")

    def workspace(self, name=None):
        """Resolve a workspace by display name (or id) on the active target.
        Names are the cross-target contract; GUIDs never match across targets."""
        name = name or self.workspace_scope
        if not name:
            raise TargetError("workspace(): pass a name or set FABRIC_WORKSPACE")
        r = self.session().get("/workspaces")
        r.raise_for_status()
        for w in r.json().get("value", []):
            if w.get("displayName") == name or w.get("id") == name:
                return Workspace(w["id"], w.get("displayName", name))
        raise TargetError(
            f"workspace {name!r} not found on target {self.name} "
            f"({len(r.json().get('value', []))} visible)")


def notebook_env(t=None):
    """The NOTEBOOKUTILS_* runtime context for a target, as (name, value) pairs.

    Real Fabric injects a notebook's runtime context — workspace, endpoints,
    identity — into the kernel. The shim cannot inject into an arbitrary kernel,
    so it reads that context from the environment (python/notebookutils/_config.py).
    Which means SOMETHING has to put it there, and if every caller writes its own
    set, the shim and the resolver can disagree about which entra a token comes
    from while both look right. This is the one definition: `python -m
    fabric_target env` prints it for a shell, `apply_notebook_env` applies it
    in-process, and both are this function.

    Real mode emits NO credential: `credentials.getToken` falls through to
    DefaultAzureCredential, which is what lets the same notebook run under
    `az login`, a managed identity, or inside real Fabric.
    """
    t = t or target()
    e = [("FABRIC_TARGET", t.name)]
    if t.is_emulator:
        e += [
            ("NOTEBOOKUTILS_FABRIC_URL", t.api_root.removesuffix("/v1")),
            ("NOTEBOOKUTILS_ONELAKE_URL", t.onelake_url),
            ("NOTEBOOKUTILS_ENTRA_URL", t.entra_url),
            ("NOTEBOOKUTILS_TENANT", t.tenant),
            ("NOTEBOOKUTILS_CLIENT_ID",
             _env_any(("FABRIC_CLIENT_ID", "AZURE_CLIENT_ID"), SEED_CLIENT_ID)),
            ("NOTEBOOKUTILS_CLIENT_SECRET",
             _env_any(("FABRIC_CLIENT_SECRET", "AZURE_CLIENT_SECRET"),
                      SEED_CLIENT_SECRET)),
            ("NOTEBOOKUTILS_VAULT_URL", t.vault_url),
            ("NOTEBOOKUTILS_INSECURE", "1"),
        ]
    else:
        e += [
            ("NOTEBOOKUTILS_FABRIC_URL", "https://api.fabric.microsoft.com"),
            ("NOTEBOOKUTILS_ONELAKE_URL", t.onelake_url),
            ("NOTEBOOKUTILS_ENTRA_URL", t.entra_url),
            ("NOTEBOOKUTILS_TENANT", t.tenant),
            ("NOTEBOOKUTILS_INSECURE", "0"),
            # az-login-friendly auth hints for env-driven tools:
            ("AZCOPY_AUTO_LOGIN_TYPE", "AZCLI"),
        ]
        if t.vault_url:
            e.append(("NOTEBOOKUTILS_VAULT_URL", t.vault_url))
    return e


def apply_notebook_env(t=None, override=False):
    """Put `notebook_env` into os.environ, so notebookutils in THIS process
    resolves against the same target the caller resolved.

    Without this, a script that uses both the control plane and
    `notebookutils.credentials.getSecret` gets its Fabric calls from the target
    and its notebook tokens from the shim's own defaults — two answers to one
    question, agreeing only by luck.

    An explicit NOTEBOOKUTILS_* already in the environment WINS by default:
    `eval "$(python -m fabric_target env ...)"` and a compose file are both
    deliberate, and silently overwriting them would make the toggle unpredictable
    for the case it exists to serve. Returns what it set.
    """
    applied = {}
    for k, v in notebook_env(t):
        if v is None:
            continue
        if override or not os.environ.get(k):
            os.environ[k] = v
            applied[k] = v
    if applied:
        _config_reset()
    return applied


def _config_reset():
    """Drop notebookutils' cached runtime profile, if the shim is importable.

    _config caches Config on first use; applying the environment after something
    already called getSecret would otherwise have no effect. Absent shim is fine
    — fabric-target does not depend on it.
    """
    try:
        from notebookutils._config import reset
    except ImportError:
        return
    reset()


_cached = None


def target(name=None, fresh=False):
    """The resolver. Reads FABRIC_TARGET unless a name is given."""
    global _cached
    if name is None and not fresh and _cached is not None:
        return _cached
    t = Target(name or _env("FABRIC_TARGET", "emulator"))
    if name is None:
        _cached = t
    return t
