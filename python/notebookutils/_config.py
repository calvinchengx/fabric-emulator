"""Runtime wiring for the shim — emulator or real Fabric.

Real Fabric injects the notebook's runtime context (workspace, endpoints,
managed identity) into the kernel. The shim can't inject into an arbitrary
kernel, so it reads the same context from the environment — one place, so
every module agrees on where the family lives and who the notebook runs as.

    FABRIC_TARGET                "emulator" (default) or "real" — picks the
                                 defaults below and how tokens are minted
                                 (T2 of docs/21-real-fabric-toggle.md)

    NOTEBOOKUTILS_FABRIC_URL     control plane, e.g. http://127.0.0.1:19080
    NOTEBOOKUTILS_ONELAKE_URL    DFS base; defaults to the control plane in
                                 emulator mode (Host-routed) and to the real
                                 OneLake endpoint under FABRIC_TARGET=real
    NOTEBOOKUTILS_ONELAKE_HOST   DFS Host header, default onelake.dfs.fabric.microsoft.com
    NOTEBOOKUTILS_ENTRA_URL      STS base, e.g. https://localhost:18443
    NOTEBOOKUTILS_TENANT         tenant id used in the token path
    NOTEBOOKUTILS_CLIENT_ID      the notebook's identity (client-credentials)
    NOTEBOOKUTILS_CLIENT_SECRET  its secret
    NOTEBOOKUTILS_WORKSPACE_ID   the default workspace (runtime.context)
    NOTEBOOKUTILS_LAKEHOUSE_ID   the default lakehouse, if attached
    NOTEBOOKUTILS_VAULT_URL      pin every getSecret to one vault base
    NOTEBOOKUTILS_INSECURE       "1" to skip TLS verification (local certs)

Under FABRIC_TARGET=real nothing is seeded: endpoints default to the real
Azure ones, TLS verification is on, and tokens come from
DefaultAzureCredential (`az login`, managed identity, or AZURE_* env vars) —
never from a credential baked into this file. Explicit NOTEBOOKUTILS_* values
still win, which is what `python -m fabric_target env real` emits.
"""
import os
import ssl

# entra-emulator's seeded dev identity — emulator mode only, by construction.
SEED_TENANT = "11111111-1111-1111-1111-111111111111"
SEED_CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
SEED_CLIENT_SECRET = "daemon-app-secret"

REAL_FABRIC_URL = "https://api.fabric.microsoft.com"
REAL_ONELAKE_URL = "https://onelake.dfs.fabric.microsoft.com"
REAL_ENTRA_URL = "https://login.microsoftonline.com"
ONELAKE_HOST = "onelake.dfs.fabric.microsoft.com"


def _env(name, default=None, required=False):
    v = os.environ.get(name, default)
    if required and not v:
        raise RuntimeError(
            f"notebookutils: {name} is not set. In the emulator the notebook "
            "runtime context comes from the environment; see python/notebookutils/_config.py."
        )
    return v


class Config:
    def __init__(self):
        self.target = (_env("FABRIC_TARGET", "emulator") or "emulator").strip().lower()
        if self.target not in ("emulator", "real"):
            raise RuntimeError("notebookutils: FABRIC_TARGET must be 'emulator' or 'real', "
                               f"got {self.target!r}")

        if self.is_real:
            self.fabric_url = _env("NOTEBOOKUTILS_FABRIC_URL", REAL_FABRIC_URL).rstrip("/")
            self.onelake_url = _env("NOTEBOOKUTILS_ONELAKE_URL", REAL_ONELAKE_URL).rstrip("/")
            self.entra_url = _env("NOTEBOOKUTILS_ENTRA_URL", REAL_ENTRA_URL).rstrip("/")
            self.tenant = _env("NOTEBOOKUTILS_TENANT", _env("AZURE_TENANT_ID", "organizations"))
            # Real mode never carries a seeded credential: tokens come from
            # DefaultAzureCredential (see credentials.getToken).
            self.client_id = None
            self.client_secret = None
        else:
            self.fabric_url = _env("NOTEBOOKUTILS_FABRIC_URL", "http://127.0.0.1:19080").rstrip("/")
            # Emulator: the DFS surface is Host-routed on the control-plane
            # address, so the base URL is the same and only the Host differs.
            self.onelake_url = _env("NOTEBOOKUTILS_ONELAKE_URL", self.fabric_url).rstrip("/")
            self.entra_url = _env("NOTEBOOKUTILS_ENTRA_URL", "https://localhost:18443").rstrip("/")
            self.tenant = _env("NOTEBOOKUTILS_TENANT", SEED_TENANT)
            self.client_id = _env("NOTEBOOKUTILS_CLIENT_ID", SEED_CLIENT_ID)
            self.client_secret = _env("NOTEBOOKUTILS_CLIENT_SECRET", SEED_CLIENT_SECRET)

        self.onelake_host = _env("NOTEBOOKUTILS_ONELAKE_HOST", ONELAKE_HOST)
        self.workspace_id = _env("NOTEBOOKUTILS_WORKSPACE_ID")
        self.lakehouse_id = _env("NOTEBOOKUTILS_LAKEHOUSE_ID")
        # When set, every getSecret hits this base regardless of vault name —
        # the emulator serves its default vault on non-DNS hosts, so notebook
        # code using bare vault names runs unchanged against the local family.
        # Unset in real mode, so names resolve to {name}.vault.azure.net.
        self.vault_url = _env("NOTEBOOKUTILS_VAULT_URL")
        self.insecure = _env("NOTEBOOKUTILS_INSECURE", "") not in ("", "0", "false")

    @property
    def is_real(self):
        return self.target == "real"

    def ssl_context(self):
        ctx = ssl.create_default_context()
        if self.insecure:
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
        return ctx


_cfg = None


def config():
    global _cfg
    if _cfg is None:
        _cfg = Config()
    return _cfg


def reset():
    """Drop the cached profile — for tests and kernels that re-point mid-session."""
    global _cfg
    _cfg = None
