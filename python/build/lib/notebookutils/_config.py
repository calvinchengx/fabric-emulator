"""Runtime wiring for the emulator.

Real Fabric injects the notebook's runtime context (workspace, endpoints,
managed identity) into the kernel. The emulator can't inject into an arbitrary
kernel, so this shim reads the same context from the environment — one place,
so every module agrees on where the family lives and who the notebook runs as.

    NOTEBOOKUTILS_FABRIC_URL     control plane, e.g. http://127.0.0.1:19080
    NOTEBOOKUTILS_ONELAKE_HOST   DFS Host header, default onelake.dfs.fabric.microsoft.com
    NOTEBOOKUTILS_ENTRA_URL      STS base, e.g. https://localhost:18443
    NOTEBOOKUTILS_TENANT         tenant id used in the token path
    NOTEBOOKUTILS_CLIENT_ID      the notebook's identity (client-credentials)
    NOTEBOOKUTILS_CLIENT_SECRET  its secret
    NOTEBOOKUTILS_WORKSPACE_ID   the default workspace (runtime.context)
    NOTEBOOKUTILS_LAKEHOUSE_ID   the default lakehouse, if attached
    NOTEBOOKUTILS_INSECURE       "1" to skip TLS verification (local certs)
"""
import os
import ssl


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
        self.fabric_url = _env("NOTEBOOKUTILS_FABRIC_URL", "http://127.0.0.1:19080").rstrip("/")
        self.onelake_host = _env("NOTEBOOKUTILS_ONELAKE_HOST", "onelake.dfs.fabric.microsoft.com")
        self.entra_url = _env("NOTEBOOKUTILS_ENTRA_URL", "https://localhost:18443").rstrip("/")
        self.tenant = _env("NOTEBOOKUTILS_TENANT", "11111111-1111-1111-1111-111111111111")
        self.client_id = _env("NOTEBOOKUTILS_CLIENT_ID", "cccccccc-0000-0000-0000-000000000002")
        self.client_secret = _env("NOTEBOOKUTILS_CLIENT_SECRET", "daemon-app-secret")
        self.workspace_id = _env("NOTEBOOKUTILS_WORKSPACE_ID")
        self.lakehouse_id = _env("NOTEBOOKUTILS_LAKEHOUSE_ID")
        # When set, every getSecret hits this base regardless of vault name —
        # the emulator serves its default vault on non-DNS hosts, so notebook
        # code using bare vault names runs unchanged against the local family.
        self.vault_url = _env("NOTEBOOKUTILS_VAULT_URL")
        self.insecure = _env("NOTEBOOKUTILS_INSECURE", "") not in ("", "0", "false")

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
