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


# --- the notebook runtime context -------------------------------------------
# `python -m fabric_target env` and apply_notebook_env are ONE definition
# (notebook_env), because two would let the shim and the resolver disagree about
# which entra a token comes from while both look right.

def test_apply_notebook_env_wires_the_shim_for_this_process(monkeypatch):
    monkeypatch.setenv("FABRIC_EMULATOR_URL", "https://localhost:19443")
    monkeypatch.setenv("ENTRA_EMULATOR_URL", "https://localhost:18443")
    applied = fabric_target.apply_notebook_env(Target("emulator"))
    assert os.environ["NOTEBOOKUTILS_FABRIC_URL"] == "https://localhost:19443"
    assert os.environ["NOTEBOOKUTILS_ENTRA_URL"] == "https://localhost:18443"
    # Self-signed family certs: without this, getSecret fails TLS verification
    # against the emulator's vault and the failure names the certificate, not
    # the missing wiring.
    assert os.environ["NOTEBOOKUTILS_INSECURE"] == "1"
    assert os.environ["NOTEBOOKUTILS_VAULT_URL"] == "https://localhost:8444"
    assert applied["FABRIC_TARGET"] == "emulator"


def test_apply_notebook_env_does_not_overwrite_a_deliberate_value(monkeypatch):
    """`eval "$(python -m fabric_target env ...)"` and a compose file are both
    deliberate. Silently overwriting them makes the toggle unpredictable for the
    case it exists to serve."""
    monkeypatch.setenv("NOTEBOOKUTILS_VAULT_URL", "https://mine.vault.azure.net")
    fabric_target.apply_notebook_env(Target("emulator"))
    assert os.environ["NOTEBOOKUTILS_VAULT_URL"] == "https://mine.vault.azure.net"
    fabric_target.apply_notebook_env(Target("emulator"), override=True)
    assert os.environ["NOTEBOOKUTILS_VAULT_URL"] == "https://localhost:8444"


def test_notebook_env_real_carries_no_credential(monkeypatch):
    """A notebook has no client secret to give. Real mode emits none, so
    credentials.getToken falls through to DefaultAzureCredential."""
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    env = dict(fabric_target.notebook_env(Target("real")))
    assert "NOTEBOOKUTILS_CLIENT_SECRET" not in env
    assert "NOTEBOOKUTILS_CLIENT_ID" not in env
    assert env["NOTEBOOKUTILS_INSECURE"] == "0"
    assert env["NOTEBOOKUTILS_ENTRA_URL"] == "https://login.microsoftonline.com"


def test_notebook_env_and_the_emitter_cannot_drift(monkeypatch, capsys):
    """The emitter prints exactly what apply_notebook_env applies. Asserted
    rather than assumed: they were two copies of this list until they were one."""
    monkeypatch.setenv("FABRIC_EMULATOR_URL", "https://localhost:19443")
    assert main(["prog", "env", "emulator"]) == 0
    printed = {}
    for line in capsys.readouterr().out.splitlines():
        if line.startswith("export "):
            k, _, v = line[len("export "):].partition("=")
            printed[k] = v.strip("'")
    assert printed == dict(fabric_target.notebook_env(Target("emulator")))


# --- workspace creation needs a capacity on real Fabric ----------------------
# The emulator has none, so portable code cannot name one. The session completes
# the request per target, exactly as it already does for the bearer token.

def _real(monkeypatch, **env):
    monkeypatch.setenv("FABRIC_WORKSPACE", "ws")
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    for k, v in env.items():
        monkeypatch.setenv(k, v)
    return Target("real")


def test_a_real_workspace_create_carries_the_configured_capacity(monkeypatch):
    t = _real(monkeypatch, FABRIC_CAPACITY_ID="cap-1")
    kw = {"json": {"displayName": "contoso-analytics"}}
    t._complete_workspace_create("POST", t.api_root + "/workspaces", kw)
    assert kw["json"]["capacityId"] == "cap-1"


def test_a_caller_supplied_capacity_is_never_overwritten(monkeypatch):
    t = _real(monkeypatch, FABRIC_CAPACITY_ID="cap-1")
    kw = {"json": {"displayName": "w", "capacityId": "mine"}}
    t._complete_workspace_create("POST", t.api_root + "/workspaces", kw)
    assert kw["json"]["capacityId"] == "mine"


def test_a_real_workspace_create_without_a_capacity_is_refused(monkeypatch):
    """A capacity-less workspace is created happily and then rejects every item
    in it, so the failure lands one call away from its cause."""
    t = _real(monkeypatch)
    with pytest.raises(TargetError, match="FABRIC_CAPACITY_ID"):
        t._complete_workspace_create("POST", t.api_root + "/workspaces", {"json": {"displayName": "w"}})


def test_assign_to_capacity_is_left_alone(monkeypatch):
    """A different call that happens to live under /workspaces. Injecting a body
    field into it would corrupt a request that was already correct."""
    t = _real(monkeypatch, FABRIC_CAPACITY_ID="cap-1")
    kw = {"json": {"capacityId": "other"}}
    t._complete_workspace_create("POST", t.api_root + "/workspaces/abc/assignToCapacity", kw)
    assert kw["json"] == {"capacityId": "other"}
    # And an item create under a workspace is not a workspace create.
    kw = {"json": {"displayName": "lake"}}
    t._complete_workspace_create("POST", t.api_root + "/workspaces/abc/lakehouses", kw)
    assert "capacityId" not in kw["json"]


def test_the_emulator_never_needs_a_capacity():
    t = Target("emulator")
    assert t.capacity_id is None
    kw = {"json": {"displayName": "contoso-analytics"}}
    t._complete_workspace_create("POST", t.api_root + "/workspaces", kw)
    assert "capacityId" not in kw["json"]


def test_a_get_is_not_a_create(monkeypatch):
    t = _real(monkeypatch)
    t._complete_workspace_create("GET", t.api_root + "/workspaces", {})  # must not raise
