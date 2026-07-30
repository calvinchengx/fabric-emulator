"""T2: the notebookutils shim resolves emulator vs real Fabric from
FABRIC_TARGET — same notebook code, one env var apart.

Pure unit tests: no emulator, no network, no Azure. The live emulator leg is
e2e/notebookutils; the live real leg is a user's own tenant.
"""
import os
import sys

import pytest

# python/ — the shim's parent. os.path, not a "/"-split: Windows paths use
# backslashes, so a hardcoded separator silently fails to find the package.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from notebookutils import _config, credentials  # noqa: E402

TARGET_ENV = [
    "FABRIC_TARGET", "AZURE_TENANT_ID",
    "NOTEBOOKUTILS_FABRIC_URL", "NOTEBOOKUTILS_ONELAKE_URL",
    "NOTEBOOKUTILS_ONELAKE_HOST", "NOTEBOOKUTILS_ENTRA_URL",
    "NOTEBOOKUTILS_TENANT", "NOTEBOOKUTILS_CLIENT_ID",
    "NOTEBOOKUTILS_CLIENT_SECRET", "NOTEBOOKUTILS_VAULT_URL",
    "NOTEBOOKUTILS_WORKSPACE_ID", "NOTEBOOKUTILS_LAKEHOUSE_ID",
    "NOTEBOOKUTILS_INSECURE",
]


@pytest.fixture(autouse=True)
def clean_env(monkeypatch):
    for k in TARGET_ENV:
        monkeypatch.delenv(k, raising=False)
    _config.reset()
    credentials._real_credential = None
    yield
    _config.reset()
    credentials._real_credential = None


def test_emulator_is_the_default_and_seeded():
    c = _config.config()
    assert c.target == "emulator" and not c.is_real
    assert c.client_id == _config.SEED_CLIENT_ID
    assert c.client_secret == _config.SEED_CLIENT_SECRET
    assert c.tenant == _config.SEED_TENANT
    # DFS is Host-routed on the control-plane address locally.
    assert c.onelake_url == c.fabric_url
    assert c.onelake_host == _config.ONELAKE_HOST


def test_real_resolves_azure_endpoints_and_never_seeds(monkeypatch):
    monkeypatch.setenv("FABRIC_TARGET", "real")
    monkeypatch.setenv("AZURE_TENANT_ID", "contoso-tenant")
    c = _config.config()
    assert c.is_real
    assert c.fabric_url == _config.REAL_FABRIC_URL
    # Real OneLake is a DIFFERENT host, not the control plane + Host header.
    assert c.onelake_url == _config.REAL_ONELAKE_URL
    assert c.onelake_url != c.fabric_url
    assert c.entra_url == _config.REAL_ENTRA_URL
    assert c.tenant == "contoso-tenant"
    # The seeded dev identity must never leak into real mode.
    assert c.client_id is None and c.client_secret is None
    # TLS verification stays on.
    assert c.insecure is False
    assert c.ssl_context().verify_mode is not None


def test_real_without_tenant_falls_back_to_organizations(monkeypatch):
    monkeypatch.setenv("FABRIC_TARGET", "real")
    assert _config.config().tenant == "organizations"


def test_explicit_env_still_wins_in_real_mode(monkeypatch):
    # This is what `python -m fabric_target env real` emits.
    monkeypatch.setenv("FABRIC_TARGET", "real")
    monkeypatch.setenv("NOTEBOOKUTILS_FABRIC_URL", "https://api.example.test/")
    monkeypatch.setenv("NOTEBOOKUTILS_ONELAKE_URL", "https://lake.example.test/")
    monkeypatch.setenv("NOTEBOOKUTILS_TENANT", "explicit-tenant")
    c = _config.config()
    assert c.fabric_url == "https://api.example.test"
    assert c.onelake_url == "https://lake.example.test"
    assert c.tenant == "explicit-tenant"


def test_bogus_target_rejected(monkeypatch):
    monkeypatch.setenv("FABRIC_TARGET", "staging")
    with pytest.raises(RuntimeError, match="emulator.*real"):
        _config.config()


def test_vault_url_unset_in_real_resolves_public_dns(monkeypatch):
    monkeypatch.setenv("FABRIC_TARGET", "real")
    assert _config.config().vault_url is None
    assert credentials._vault_url("myvault") == "https://myvault.vault.azure.net"


def test_vault_override_pins_every_lookup_in_emulator(monkeypatch):
    monkeypatch.setenv("NOTEBOOKUTILS_VAULT_URL", "https://localhost:8444")
    assert credentials._vault_url("anything") == "https://localhost:8444"


def test_real_token_uses_default_azure_credential(monkeypatch):
    monkeypatch.setenv("FABRIC_TARGET", "real")
    seen = {}

    class FakeToken:
        token = "real-token"

    class FakeCredential:
        def get_token(self, scope):
            seen["scope"] = scope
            return FakeToken()

    # Stand in for azure.identity without requiring it to be installed.
    import types
    mod = types.ModuleType("azure.identity")
    mod.DefaultAzureCredential = lambda *a, **kw: FakeCredential()
    azure = types.ModuleType("azure")
    azure.identity = mod
    monkeypatch.setitem(sys.modules, "azure", azure)
    monkeypatch.setitem(sys.modules, "azure.identity", mod)

    # No HTTP may happen in real mode — client-credentials is emulator-only.
    def boom(*a, **kw):
        raise AssertionError("real mode must not POST to an STS itself")
    monkeypatch.setattr(credentials, "request", boom)

    assert credentials.getToken("storage") == "real-token"
    assert seen["scope"] == "https://storage.azure.com/.default"
    # Fabric + vault aliases resolve to their real resources too.
    credentials.getToken("keyvault")
    assert seen["scope"] == "https://vault.azure.net/.default"
    credentials.getToken("fabric")
    assert seen["scope"] == "https://api.fabric.microsoft.com/.default"


def test_real_token_without_azure_identity_explains_itself(monkeypatch):
    monkeypatch.setenv("FABRIC_TARGET", "real")

    import builtins
    real_import = builtins.__import__

    def no_azure(name, *a, **kw):
        if name.startswith("azure"):
            raise ImportError("no azure-identity")
        return real_import(name, *a, **kw)

    monkeypatch.setattr(builtins, "__import__", no_azure)
    with pytest.raises(RuntimeError, match="azure-identity"):
        credentials.getToken("storage")


def test_emulator_token_still_uses_client_credentials(monkeypatch):
    captured = {}

    def fake_request(method, url, *, body=None, headers=None, **kw):
        captured["url"] = url
        captured["body"] = body.decode()
        return {"access_token": "emulator-token"}

    monkeypatch.setattr(credentials, "request", fake_request)
    assert credentials.getToken("storage") == "emulator-token"
    assert "/oauth2/v2.0/token" in captured["url"]
    assert "grant_type=client_credentials" in captured["body"]
    assert _config.SEED_CLIENT_ID in captured["body"]
