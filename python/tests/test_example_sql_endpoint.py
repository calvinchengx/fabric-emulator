"""The examples' SQL endpoint resolution, both targets, no emulator.

WHY THIS IS TESTED HERE. `sql_endpoint()` is the one piece of the examples whose
REAL branch cannot be exercised by the local e2e: it reads properties only a real
tenant fills in, and returns a different database name and TLS mode. The emulator
leg is covered end to end by e2e/medallion; this file covers the branch that leg
cannot reach, against the response shapes Microsoft's REST reference documents:

  Lakehouse: properties.sqlEndpointProperties.{connectionString, provisioningStatus}
  Warehouse: properties.connectionString

It does NOT claim the real path connects — only that what it would dial is built
from the API's answer rather than from a pinned host.
"""
import importlib
import os
import pathlib
import sys

import pytest

REPO = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "python" / "fabric-target"))
sys.path.insert(0, str(REPO / "examples" / "contoso-fixtures"))

import fabric_target  # noqa: E402


class FakeResponse:
    def __init__(self, payload):
        self._payload = payload
        self.status_code = 200

    def json(self):
        return self._payload

    def raise_for_status(self):
        pass


class FakeSession:
    """Answers the two GETs sql_endpoint makes: the generic item record (type and
    display name, which real Fabric does return there) and the typed route that
    carries the properties."""

    def __init__(self, routes):
        self.routes = routes
        self.asked = []

    def get(self, url, **_):
        self.asked.append(url)
        for suffix, payload in self.routes.items():
            if url.endswith(suffix):
                return FakeResponse(payload)
        raise AssertionError(f"unexpected GET {url}")


def load_common(monkeypatch, tmp_path, target_name, **env):
    """Import examples/contoso-fixtures/common.py against a chosen target.

    Re-imported per test because the module resolves its target once at import,
    which is the behaviour under test: a step cannot change target mid-run.
    """
    for k in ("FABRIC_TARGET", "FABRIC_WORKSPACE", "TDS_SERVER", "AZURE_CLIENT_SECRET",
              "AZURE_CLIENT_ID", "AZURE_TENANT_ID", "FABRIC_VAULT_URL",
              "AZURE_KEY_VAULT_URL", "PIPELINE_STATE"):
        monkeypatch.delenv(k, raising=False)
    # Also the notebook runtime context: apply_notebook_env deliberately does not
    # overwrite an existing value, so a previous test's target would survive into
    # this one. Each step is its own process in real life; make the test one too.
    for k in [k for k in os.environ if k.startswith("NOTEBOOKUTILS_")]:
        monkeypatch.delenv(k, raising=False)
    monkeypatch.setenv("FABRIC_TARGET", target_name)
    monkeypatch.setenv("PIPELINE_STATE", str(tmp_path / "state.json"))
    for k, v in env.items():
        monkeypatch.setenv(k, v)
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    fabric_target._cached = None
    sys.modules.pop("common", None)
    common = importlib.import_module("common")
    (tmp_path / "state.json").write_text('{"workspace": "ws-1"}')
    return common


LAKEHOUSE_REAL = {
    "displayName": "contoso_lake", "type": "Lakehouse", "id": "lh-1",
    "properties": {
        "sqlEndpointProperties": {
            "connectionString": "abc123-xyz.datawarehouse.fabric.microsoft.com",
            "id": "37dc8a41-dea9-465d-b528-3e95043b2356",
            "provisioningStatus": "Success",
        },
    },
}
WAREHOUSE_REAL = {
    "displayName": "contoso_gold", "type": "Warehouse", "id": "wh-1",
    "properties": {"connectionString": "abc123-xyz.datawarehouse.fabric.microsoft.com"},
}


def test_real_dials_the_address_the_api_reports(monkeypatch, tmp_path):
    """The whole point: on real Fabric the SQL host is assigned by the service, so
    an example that hardcodes one is emulator-only however carefully everything
    else is resolved."""
    common = load_common(monkeypatch, tmp_path, "real",
                         FABRIC_WORKSPACE="ws", FABRIC_VAULT_URL="https://kv.vault.azure.net")
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    monkeypatch.setattr(common, "S", FakeSession({
        "/items/lh-1": LAKEHOUSE_REAL,
        "/lakehouses/lh-1": LAKEHOUSE_REAL,
    }))
    t = common.sql_endpoint("lh-1")
    assert t.server == "abc123-xyz.datawarehouse.fabric.microsoft.com"
    # Fabric addresses a database by DISPLAY NAME, with the workspace in the
    # server name. The item id is the emulator's addressing and means nothing there.
    assert t.database == "contoso_lake"
    assert t.encrypt is True  # real Fabric's endpoint requires TLS


def test_real_warehouse_uses_its_own_property(monkeypatch, tmp_path):
    common = load_common(monkeypatch, tmp_path, "real",
                         FABRIC_WORKSPACE="ws", FABRIC_VAULT_URL="https://kv.vault.azure.net")
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    monkeypatch.setattr(common, "S", FakeSession({
        "/items/wh-1": WAREHOUSE_REAL,
        "/warehouses/wh-1": WAREHOUSE_REAL,
    }))
    t = common.sql_endpoint("wh-1")
    assert (t.server, t.database) == ("abc123-xyz.datawarehouse.fabric.microsoft.com", "contoso_gold")


def test_a_pending_analytics_endpoint_is_refused_by_name(monkeypatch, tmp_path):
    """Real Fabric provisions the analytics endpoint asynchronously. "InProgress"
    is a wait, not a missing feature, and saying so beats a connection timeout
    that names nothing."""
    pending = {**LAKEHOUSE_REAL, "properties": {"sqlEndpointProperties": {
        "connectionString": "abc.datawarehouse.fabric.microsoft.com",
        "provisioningStatus": "InProgress"}}}
    common = load_common(monkeypatch, tmp_path, "real",
                         FABRIC_WORKSPACE="ws", FABRIC_VAULT_URL="https://kv.vault.azure.net")
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    monkeypatch.setattr(common, "S", FakeSession({
        "/items/lh-1": pending, "/lakehouses/lh-1": pending}))
    with pytest.raises(AssertionError, match="InProgress"):
        common.sql_endpoint("lh-1")


def test_emulator_addresses_the_item_by_id_over_plain_tds(monkeypatch, tmp_path):
    """The emulator serves one host for every workspace, so it addresses by item
    id (internal/server/warehouse.go accepts both), and its TDS front terminates
    FedAuth without TLS."""
    lake = {"displayName": "contoso_lake", "type": "Lakehouse", "id": "lh-1",
            "properties": {"sqlEndpointProperties": {
                "connectionString": "localhost:1433", "provisioningStatus": "Success"}}}
    common = load_common(monkeypatch, tmp_path, "emulator")
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    monkeypatch.setattr(common, "S", FakeSession({
        "/items/lh-1": lake, "/lakehouses/lh-1": lake}))
    t = common.sql_endpoint("lh-1")
    # host:port is what the emulator advertises; host,port is what the SQL Server
    # ODBC driver accepts. A real connection string is a bare FQDN and unaffected.
    assert t.server == "localhost,1433"
    assert t.database == "lh-1"
    assert t.encrypt is False


def test_tds_server_overrides_the_address_but_not_the_endpoint_id(monkeypatch, tmp_path):
    """The emulator advertises the port it LISTENS on, not the port Docker
    published it to, so a remapped isolated stack is the one case discovery gets
    wrong. Fabric has no such distinction, which is why this is an override — of
    the ADDRESS only. The endpoint id is a different fact and sync_sql_endpoint
    needs it, so the typed route is still asked."""
    lake = {"displayName": "contoso_lake", "type": "Lakehouse", "id": "lh-1",
            "properties": {"sqlEndpointProperties": {
                "connectionString": "localhost:1433", "provisioningStatus": "Success"}}}
    common = load_common(monkeypatch, tmp_path, "emulator", TDS_SERVER="localhost,11533")
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    session = FakeSession({"/items/lh-1": lake, "/lakehouses/lh-1": lake})
    monkeypatch.setattr(common, "S", session)
    t = common.sql_endpoint("lh-1")
    assert t.server == "localhost,11533"
    assert t.endpoint_id is None  # the emulator has no SQLEndpoint item
    assert any("lakehouses" in u for u in session.asked)


def test_an_item_with_no_sql_endpoint_says_so(monkeypatch, tmp_path):
    nb = {"displayName": "bronze-orders", "type": "Notebook", "id": "nb-1"}
    common = load_common(monkeypatch, tmp_path, "emulator")
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    monkeypatch.setattr(common, "S", FakeSession({"/items/nb-1": nb}))
    with pytest.raises(AssertionError, match="no SQL endpoint"):
        common.sql_endpoint("nb-1")


def test_a_stack_without_its_sql_sidecar_names_the_override(monkeypatch, tmp_path):
    """The contract-only stack advertises no connection string. The failure should
    name the way out rather than surface as a connect timeout."""
    lake = {"displayName": "contoso_lake", "type": "Lakehouse", "id": "lh-1",
            "properties": {}}
    common = load_common(monkeypatch, tmp_path, "emulator")
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    monkeypatch.setattr(common, "S", FakeSession({
        "/items/lh-1": lake, "/lakehouses/lh-1": lake}))
    with pytest.raises(AssertionError, match="TDS_SERVER"):
        common.sql_endpoint("lh-1")


def test_odbc_server_rewrites_only_a_trailing_port(monkeypatch, tmp_path):
    common = load_common(monkeypatch, tmp_path, "emulator")
    for given, want in {
        "localhost:1433": "localhost,1433",
        "[::1]:1433": "[::1],1433",
        # A real Fabric connection string carries no port and must survive intact.
        "abc-xyz.datawarehouse.fabric.microsoft.com": "abc-xyz.datawarehouse.fabric.microsoft.com",
        "fabric-emulator:1433": "fabric-emulator,1433",
    }.items():
        assert common._odbc_server(given) == want, given


def test_the_skip_and_refuse_guards_are_opposites(monkeypatch, tmp_path):
    """skip_on_real exits 0 (the pipeline does not need this step's output on real
    Fabric); only_the_emulator_can exits non-zero (it does, and cannot have it).
    Getting these the wrong way round is how ensure_app blocked four steps from
    being portable at all."""
    common = load_common(monkeypatch, tmp_path, "real",
                         FABRIC_WORKSPACE="ws", FABRIC_VAULT_URL="https://kv.vault.azure.net")
    with pytest.raises(SystemExit) as skipped:
        common.skip_on_real("the notebook engine", "Fabric runs it on its own pool")
    assert skipped.value.code == 0

    with pytest.raises(SystemExit) as refused:
        common.only_the_emulator_can("driving Spark Connect", because="no endpoint",
                                     instead="a Notebook definition")
    assert refused.value.code != 0
    assert "instead" in str(refused.value)


def test_neither_guard_fires_under_the_emulator(monkeypatch, tmp_path):
    common = load_common(monkeypatch, tmp_path, "emulator")
    common.skip_on_real("x", "y")
    common.only_the_emulator_can("x", because="y", instead="z")


def test_the_notebook_runtime_is_wired_from_the_resolved_target(monkeypatch, tmp_path):
    """A step that uses notebookutils.credentials.getSecret and the control plane
    in one process must not get its tokens from two different entras."""
    load_common(monkeypatch, tmp_path, "emulator", ENTRA_EMULATOR_URL="https://localhost:18443")
    assert os.environ["NOTEBOOKUTILS_ENTRA_URL"] == "https://localhost:18443"
    assert os.environ["NOTEBOOKUTILS_INSECURE"] == "1"


# --- keeping the SQL endpoint in step with Delta -----------------------------
# Two mechanisms, one for each target, and the emulator's absence of an endpoint
# ID is the signal for which. dq_gate.py used to open a throwaway connection with
# the comment "re-reflect whatever silver now holds" — true locally, a silent
# no-op on real Fabric, where the endpoint syncs on its own schedule and the gate
# would then pass on data it never rebuilt.

def test_the_emulator_syncs_by_connecting(monkeypatch, tmp_path):
    lake = {"displayName": "contoso_lake", "type": "Lakehouse", "id": "lh-1",
            "properties": {"sqlEndpointProperties": {
                "connectionString": "localhost:1433", "provisioningStatus": "Success"}}}
    common = load_common(monkeypatch, tmp_path, "emulator")
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    session = FakeSession({"/items/lh-1": lake, "/lakehouses/lh-1": lake})
    monkeypatch.setattr(common, "S", session)

    class FakeConn:
        def __enter__(self): return self
        def __exit__(self, *a): return False

    connected = []
    monkeypatch.setattr(common, "tds_connect", lambda i: connected.append(i) or FakeConn())
    common.sync_sql_endpoint("lh-1")
    assert connected == ["lh-1"]
    assert not any("refreshMetadata" in u for u in session.asked)


def test_real_syncs_with_refreshmetadata_on_the_endpoint_id(monkeypatch, tmp_path):
    """Addressed by the SQLEndpoint item's own id, which is why that id being
    reported at all is what selects this branch."""
    lake = {**LAKEHOUSE_REAL}
    common = load_common(monkeypatch, tmp_path, "real",
                         FABRIC_WORKSPACE="ws", FABRIC_VAULT_URL="https://kv.vault.azure.net")
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    session = FakeSession({"/items/lh-1": lake, "/lakehouses/lh-1": lake})
    posted = []

    def post(url, **kw):
        posted.append(url)
        return FakeResponse({})

    session.post = post
    monkeypatch.setattr(common, "S", session)
    monkeypatch.setattr(common._T, "poll_lro", lambda r, **k: r)
    monkeypatch.setattr(common, "tds_connect", lambda *a, **k: (_ for _ in ()).throw(
        AssertionError("real mode must not sync by connecting")))
    common.sync_sql_endpoint("lh-1")
    assert posted and posted[0].endswith(
        "/workspaces/ws-1/sqlEndpoints/37dc8a41-dea9-465d-b528-3e95043b2356/refreshMetadata"), posted
