"""The portability gate must fail on the states it exists to prevent.

A checker nobody has watched fail is a longer sentence, not a gate. Each test
below constructs the violation in a temp tree and asserts the checker reports
it — and the last two assert what it must NOT report, because the first version
of this checker flagged 31 things, none of them real: it read `.venv` and treated
every `"path"` key as a definition part, including shortcut targets and URLs.
"""
import importlib.util
import pathlib
import sys

REPO = pathlib.Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_example_portability", REPO / "scripts" / "check_example_portability.py")
assert spec and spec.loader
cep = importlib.util.module_from_spec(spec)
sys.modules["check_example_portability"] = cep
spec.loader.exec_module(cep)


def _tree(tmp_path, files, resolver="from fabric_target import target\n"):
    """Build a fake repo: examples/<...> plus the resolver the checker expects."""
    (tmp_path / "examples" / "contoso-fixtures").mkdir(parents=True, exist_ok=True)
    (tmp_path / "examples" / "contoso-fixtures" / "common.py").write_text(resolver)
    for rel, body in files.items():
        p = tmp_path / "examples" / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(body)
    cep.ROOT = tmp_path
    cep.EXAMPLES = tmp_path / "examples"
    problems = []
    cep.check_no_pinned_target(problems)
    cep.check_no_emulator_only_leaks(problems)
    cep.check_resolver_uses_the_contract(problems)
    cep.check_definition_parts(problems)
    return problems


def test_a_step_hardcoding_the_seeded_secret_is_caught(tmp_path):
    problems = _tree(tmp_path, {"demo/step.py": 'SECRET = "daemon-app-secret"\n'})
    assert any("seeded credential" in p for p in problems), problems


def test_a_step_hardcoding_a_localhost_endpoint_is_caught(tmp_path):
    problems = _tree(tmp_path, {"demo/step.py": 'API = "https://localhost:9443"\n'})
    assert any("localhost:9443" in p for p in problems), problems


def test_a_step_naming_the_sql_endpoint_is_caught(tmp_path):
    """The SQL address has no URL scheme, so the localhost rule above cannot see
    it — and on real Fabric it is per-item and assigned by the service, which
    makes a named host the same opt-out as a named control plane."""
    for body in ('SERVER = "localhost,1433"\n', 'cs = "localhost:1433"\n'):
        problems = _tree(tmp_path, {"demo/gold.py": body})
        assert any("SQL endpoint" in p for p in problems), (body, problems)


def test_a_resolver_that_restates_the_contract_is_caught(tmp_path):
    problems = _tree(tmp_path, {}, resolver='TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"\n')
    assert any("does not import fabric_target" in p for p in problems), problems


def test_an_invented_definition_part_path_is_caught(tmp_path):
    problems = _tree(tmp_path, {
        "demo/step.py": 'create_item("n", "Notebook", {"notebook.py": BODY})\n'})
    assert any("source-format path" in p for p in problems), problems


def test_the_real_part_paths_are_accepted(tmp_path):
    problems = _tree(tmp_path, {
        "demo/nb.py": 'create_item("n", "Notebook", {"notebook-content.py": BODY})\n',
        "demo/pl.py": 'create_item("p", "DataPipeline", {"pipeline-content.json": BODY})\n',
        "demo/sm.py": '[{"path": "definition/tables/Sales.tmdl", "payloadType": "InlineBase64"}]\n',
    })
    assert problems == [], problems


# --- the false positives the first version produced ------------------------

def test_a_shortcut_target_is_not_a_definition_part(tmp_path):
    """`"path": "Tables/bronze_orders"` is a OneLake shortcut target. It has no
    payloadType, is not item source, and flagging it made the gate noise."""
    problems = _tree(tmp_path, {
        "demo/step.py": 'body = {"target": {"path": "Tables/bronze_orders"}}\n'})
    assert problems == [], problems


def test_installed_dependencies_are_not_scanned(tmp_path):
    """site-packages is other people's code; reading it trains readers to ignore
    the gate."""
    problems = _tree(tmp_path, {
        "demo/.venv/lib/python3.13/site-packages/dep/x.py":
            'SECRET = "daemon-app-secret"\nAPI = "https://localhost:9443"\n'})
    assert problems == [], problems


def test_a_companion_service_endpoint_is_not_a_violation(tmp_path):
    """OpenMetadata and mock source APIs have no real-Fabric counterpart, so the
    toggle has nothing to resolve them to. Only the Fabric surfaces (9443 control
    plane, 8443 authority, 8444 vault) are its business — the same line
    contoso-data-platform's own check draws."""
    problems = _tree(tmp_path, {
        "demo/govern.py": 'OM = "http://localhost:8585/api/v1"\n',
        "demo/sources.py": 'POS = "http://localhost:18090"\n'})
    assert problems == [], problems


def test_a_fabric_surface_endpoint_IS_a_violation(tmp_path):
    for port in ("9443", "8443", "8444"):
        problems = _tree(tmp_path, {"demo/step.py": f'API = "https://localhost:{port}"\n'})
        assert any(port in p for p in problems), (port, problems)


def test_a_tls_bypass_outside_the_resolver_is_caught(tmp_path):
    """Real Fabric serves real certificates. A bypass in a step ships to
    production invisibly — the emulator would go on passing."""
    for body in ('r = S.get(url, verify=False)\n',
                 'opts["azure_allow_invalid_certificates"] = "true"\n'):
        problems = _tree(tmp_path, {"demo/step.py": body})
        assert any("TLS" in p for p in problems), (body, problems)


def test_an_emulator_only_control_lever_is_caught(tmp_path):
    """The clock, fault injection and the event stream have no real counterpart.
    Using one unguarded is the clearest possible pin to the emulator."""
    problems = _tree(tmp_path, {
        "demo/step.py": 'S.post(f"{API}/_emulator/clock", json={"advance": 60})\n'})
    assert any("emulator-only control endpoint" in p for p in problems), problems


def test_the_resolver_may_still_hold_local_reality(tmp_path):
    """The bypass belongs somewhere — in the resolver, behind the target."""
    problems = _tree(
        tmp_path, {},
        resolver='from fabric_target import target\nS.verify = False\n')
    assert problems == [], problems
