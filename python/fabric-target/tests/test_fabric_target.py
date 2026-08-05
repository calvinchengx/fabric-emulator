"""Pure-unit tests for the resolver — no emulators, no network."""
import os
import sys

import pytest

# Run from a clean checkout through the root uv workspace:
# put the package dir on sys.path. os.path, not a "/"-split — Windows.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import fabric_target
from fabric_target import Target, TargetError, target
from fabric_target.__main__ import main


@pytest.fixture(autouse=True)
def clean_env(monkeypatch):
    for k in ("FABRIC_TARGET", "FABRIC_WORKSPACE", "FABRIC_EMULATOR_URL",
              "ENTRA_EMULATOR_URL", "VAULT_EMULATOR_URL", "AZURE_CLIENT_SECRET",
              "AZURE_TENANT_ID", "FABRIC_TARGET_ALLOW_DESTRUCTIVE",
              "FABRIC_CLIENT_ID", "FABRIC_CLIENT_SECRET", "FABRIC_VAULT_URL",
              "FABRIC_URL", "ENTRA_URL", "AZURE_KEY_VAULT_URL",
              "AZURE_CLIENT_ID"):
        monkeypatch.delenv(k, raising=False)
    fabric_target._cached = None


def test_emulator_defaults_are_zero_config():
    t = Target("emulator")
    assert t.is_emulator and not t.is_real
    assert t.api_root == "https://localhost:9443/v1"
    assert t.entra_url == "https://localhost:8443"
    assert t.vault_url == "https://localhost:8444"
    assert t.tenant == fabric_target.SEED_TENANT
    assert t.tls_verify is False


def test_emulator_urls_overridable(monkeypatch):
    monkeypatch.setenv("FABRIC_EMULATOR_URL", "https://localhost:19443/")
    monkeypatch.setenv("ENTRA_EMULATOR_URL", "https://localhost:18443")
    t = Target("emulator")
    assert t.api_root == "https://localhost:19443/v1"
    assert t.entra_url == "https://localhost:18443"


def test_emulator_accepts_the_azure_names(monkeypatch):
    """A consumer driving BOTH targets from one compose file writes the Azure
    names, because real mode requires them. Emulator mode must not make it
    write every endpoint twice."""
    monkeypatch.setenv("FABRIC_URL", "https://localhost:19443")
    monkeypatch.setenv("ENTRA_URL", "https://localhost:18443")
    monkeypatch.setenv("AZURE_KEY_VAULT_URL", "https://localhost:18444")
    monkeypatch.setenv("AZURE_TENANT_ID", "22222222-2222-2222-2222-222222222222")
    monkeypatch.setenv("AZURE_CLIENT_ID", "some-app")
    t = Target("emulator")
    assert t.api_root == "https://localhost:19443/v1"
    assert t.entra_url == "https://localhost:18443"
    assert t.vault_url == "https://localhost:18444"
    assert t.tenant == "22222222-2222-2222-2222-222222222222"
    assert t.credential._id == "some-app"


def test_the_fabric_names_win_over_the_azure_ones(monkeypatch):
    """Additive, so nothing that already sets FABRIC_* changes behaviour."""
    monkeypatch.setenv("FABRIC_EMULATOR_URL", "https://localhost:1111")
    monkeypatch.setenv("FABRIC_URL", "https://localhost:2222")
    assert Target("emulator").api_root == "https://localhost:1111/v1"


def test_real_refuses_the_seeded_credential(monkeypatch):
    """The alias above means a shell left over from a local run can carry the
    seeded daemon into real mode. Refuse by VALUE — the variable name no longer
    distinguishes the two."""
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    monkeypatch.setenv("AZURE_CLIENT_SECRET", fabric_target.SEED_CLIENT_SECRET)
    with pytest.raises(TargetError, match="SEEDED"):
        Target("real")


def test_real_refuses_the_seeded_tenant(monkeypatch):
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    monkeypatch.setenv("AZURE_TENANT_ID", fabric_target.SEED_TENANT)
    with pytest.raises(TargetError, match="SEEDED"):
        Target("real")


def test_real_vault_accepts_the_azure_name(monkeypatch):
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    monkeypatch.setenv("AZURE_KEY_VAULT_URL", "https://kv.vault.azure.net")
    assert Target("real").vault_url == "https://kv.vault.azure.net"


def test_target_reads_env(monkeypatch):
    monkeypatch.setenv("FABRIC_TARGET", "emulator")
    assert target(fresh=True).is_emulator


def test_bogus_target_rejected():
    with pytest.raises(TargetError, match=r"emulator.*real"):
        Target("staging")


def test_real_requires_workspace_scope(monkeypatch):
    monkeypatch.setenv("AZURE_CLIENT_SECRET", "s")  # credential source present
    with pytest.raises(TargetError, match="FABRIC_WORKSPACE"):
        Target("real")


def test_real_requires_credential_source(monkeypatch):
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: False)
    with pytest.raises(TargetError, match="az login"):
        Target("real")


def test_real_accepts_az_login(monkeypatch):
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    t = Target("real")
    assert t.is_real
    assert t.api_root == "https://api.fabric.microsoft.com/v1"
    assert t.tls_verify is True


def test_emulator_only_guard(monkeypatch):
    Target("emulator").emulator_only("clock control")  # fine
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    with pytest.raises(TargetError, match="emulator-only"):
        Target("real").emulator_only("clock control")


def test_destructive_gate(monkeypatch):
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    t = Target("real")
    with pytest.raises(TargetError, match="ALLOW_DESTRUCTIVE"):
        t._guard_destructive("DELETE", "x")
    monkeypatch.setenv("FABRIC_TARGET_ALLOW_DESTRUCTIVE", "1")
    t._guard_destructive("DELETE", "x")  # allowed now
    Target("emulator")._guard_destructive("DELETE", "x")  # never gated locally


def test_env_emitter_emulator_contains_seeds(capsys):
    assert main(["prog", "env", "emulator"]) == 0
    out = capsys.readouterr().out
    assert "export FABRIC_TARGET=emulator" in out
    assert fabric_target.SEED_CLIENT_ID in out
    assert "NOTEBOOKUTILS_INSECURE=1" in out
    # every non-comment line is eval-safe `export K=V`
    for line in out.splitlines():
        assert line.startswith("export ") or line.startswith("#")


def test_env_emitter_real_never_leaks_seeds(monkeypatch, capsys):
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    assert main(["prog", "env", "real"]) == 0
    out = capsys.readouterr().out
    assert "export FABRIC_TARGET=real" in out
    assert fabric_target.SEED_CLIENT_ID not in out
    assert fabric_target.SEED_CLIENT_SECRET not in out
    assert "AZCOPY_AUTO_LOGIN_TYPE=AZCLI" in out


def test_show_runs(capsys):
    assert main(["prog", "show"]) == 0
    assert "api_root" in capsys.readouterr().out
