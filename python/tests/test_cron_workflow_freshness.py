"""The freshness checker's trigger parse, which is the part that can fail silently.

If `_triggers` under-reads, a cron-only workflow looks push-triggered and is
skipped; if it over-reads, `ci.yml` looks cron-only and the check reports drift
that cannot exist. Either way the failure is quiet, so the parse is asserted
directly rather than inferred from the script exiting 0.

STDLIB-ONLY BY REQUIREMENT, not preference. The first version used pyyaml,
which is declared in the `test` group and absent from three of the jobs that
run this script — it died on ModuleNotFoundError in all three. pyproject.toml
already records that exact trap ("a developer machine usually has pyyaml lying
around and CI does not"); a stale local .venv is why it passed here first.
"""
import importlib.util
import pathlib

REPO = pathlib.Path(__file__).resolve().parents[2]
_spec = importlib.util.spec_from_file_location(
    "cwf", REPO / "scripts" / "check_cron_workflow_freshness.py")
assert _spec and _spec.loader  # the house idiom; ty needs the narrowing
cwf = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(cwf)


def test_the_module_imports_with_no_third_party_dependency():
    """Loading it above already proves this; the assertion states the property
    so a future `import yaml` fails here rather than on a runner."""
    src = (REPO / "scripts" / "check_cron_workflow_freshness.py").read_text(encoding="utf-8")
    for banned in ("import yaml", "import requests"):
        assert banned not in src, f"{banned!r} is absent from the jobs that run this"


def test_block_form_triggers():
    assert cwf._triggers(
        "name: x\non:\n  schedule:\n    - cron: '0 0 * * 1'\n  workflow_dispatch:\n\njobs:\n  a:\n"
    ) == {"schedule", "workflow_dispatch"}


def test_push_form_is_recognised_so_it_is_not_treated_as_cron_only():
    got = cwf._triggers("on:\n  push:\n    branches: [main]\n  pull_request:\n\njobs:\n")
    assert got == {"push", "pull_request"}
    assert got & cwf.PUSH_TRIGGERS


def test_inline_list_form():
    assert cwf._triggers("on: [push, pull_request]\n") == {"push", "pull_request"}


def test_inline_scalar_form():
    assert cwf._triggers("on: workflow_dispatch\n") == {"workflow_dispatch"}


def test_the_parse_stops_at_the_next_top_level_key():
    """`jobs:` and everything under it must not be read as triggers — that would
    make every workflow look cron-only and the check vacuous."""
    got = cwf._triggers(
        "on:\n  schedule:\n    - cron: '0 0 * * 1'\n\npermissions:\n  contents: read\n\njobs:\n  build:\n    steps: []\n")
    assert got == {"schedule"}


# --- against the repository's own files, which is what it actually parses ----

def _workflow(name):
    return (REPO / ".github" / "workflows" / name).read_text(encoding="utf-8")


def test_ci_yml_is_push_triggered():
    """The single most important negative: if ci.yml ever reads as cron-only,
    the checker would demand a dispatch for the workflow that runs on every
    commit."""
    assert cwf._triggers(_workflow("ci.yml")) & cwf.PUSH_TRIGGERS


def test_the_known_cron_only_workflows_read_as_cron_only():
    for name in ("spark-jvm.yml", "real-fabric.yml", "sempy.yml", "xmla.yml"):
        got = cwf._triggers(_workflow(name))
        assert got, f"{name}: parsed no triggers at all"
        assert not (got & cwf.PUSH_TRIGGERS), f"{name} read as push-triggered: {got}"


def test_every_workflow_parses_to_something():
    """A regex that matches nothing returns an empty set, which reads as
    'not push-triggered' and would quietly add a workflow to the cron list."""
    for path in sorted((REPO / ".github" / "workflows").glob("*.yml")):
        assert cwf._triggers(path.read_text(encoding="utf-8")), f"{path.name}: no triggers parsed"


def test_exemptions_carry_a_reason():
    for name, why in cwf.EXEMPT.items():
        assert why.strip(), f"{name} is exempt with no reason"
        assert (REPO / ".github" / "workflows" / name).exists(), (
            f"{name} is exempt but no longer exists — a stale exemption hides "
            "the next real one")
