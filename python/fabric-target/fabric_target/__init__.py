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

# entra-emulator's seeded dev defaults — emulator mode only, by construction.
SEED_TENANT = "11111111-1111-1111-1111-111111111111"
SEED_CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
SEED_CLIENT_SECRET = "daemon-app-secret"

AccessToken = namedtuple("AccessToken", ["token", "expires_on"])
Workspace = namedtuple("Workspace", ["id", "display_name"])


class TargetError(RuntimeError):
    """A target rule was violated (emulator-only feature under real, missing
    credentials, unguarded destructive call, ...)."""


def _env(name, default=None):
    v = os.environ.get(name)
    return v if v not in (None, "") else default


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


class Target:
    def __init__(self, name):
        if name not in ("emulator", "real"):
            raise TargetError(f"FABRIC_TARGET must be 'emulator' or 'real', got {name!r}")
        self.name = name
        self._credential = None
        self._workspaces = None

        if self.is_emulator:
            fabric = _env("FABRIC_EMULATOR_URL", "https://localhost:9443").rstrip("/")
            self.api_root = fabric + "/v1"
            self.entra_url = _env("ENTRA_EMULATOR_URL", "https://localhost:8443").rstrip("/")
            self.tenant = _env("FABRIC_TENANT", SEED_TENANT)
            self.onelake_url = fabric  # Host-routed / account-prefixed locally
            self.vault_url = _env("VAULT_EMULATOR_URL", "https://localhost:8444").rstrip("/")
            self.tls_verify = False
            self.workspace_scope = _env("FABRIC_WORKSPACE")  # optional locally
        else:
            self.api_root = "https://api.fabric.microsoft.com/v1"
            self.tenant = _env("AZURE_TENANT_ID", "organizations")
            self.entra_url = "https://login.microsoftonline.com"
            self.onelake_url = "https://onelake.dfs.fabric.microsoft.com"
            self.vault_url = _env("FABRIC_VAULT_URL")  # the user's real vault
            self.tls_verify = True
            # Real mode is always workspace-scoped: nothing may enumerate a tenant.
            self.workspace_scope = _env("FABRIC_WORKSPACE")
            if not self.workspace_scope:
                raise TargetError(
                    "FABRIC_TARGET=real requires FABRIC_WORKSPACE=<workspace display name> "
                    "— real mode never operates tenant-wide.")
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
                    _env("FABRIC_CLIENT_ID", SEED_CLIENT_ID),
                    _env("FABRIC_CLIENT_SECRET", SEED_CLIENT_SECRET))
            else:
                try:
                    from azure.identity import DefaultAzureCredential
                except ImportError as e:
                    raise TargetError(
                        "real mode needs azure-identity: pip install 'fabric-target[real]'"
                    ) from e
                self._credential = DefaultAzureCredential()
        return self._credential

    # -- guards ------------------------------------------------------------
    def emulator_only(self, feature):
        """Declare a spot that has no real-Fabric counterpart (clock control,
        fault injection, seeded principals). Raises under real."""
        if self.is_real:
            raise TargetError(f"{feature}: emulator-only — this does not exist on real Fabric")

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
                "session() needs requests: pip install 'fabric-target[sessions]'") from e

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
                resp = super().request(method, url, **kw)
                if resp.status_code == 429:  # real Fabric throttles; honor it once
                    time.sleep(min(float(resp.headers.get("Retry-After", "1")), 60))
                    resp = super().request(method, url, **kw)
                return resp

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
