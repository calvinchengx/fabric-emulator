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


# --- the logic around the subprocess boundary --------------------------------
#
# Mocked at `subprocess.run`, which is the only place this script touches the
# world. Everything above it — staleness, strictness, which failure is fatal —
# is ordinary logic and is asserted here rather than discovered on a runner,
# which is how the last four defects in this file were found.

import subprocess  # noqa: E402
import types  # noqa: E402

import pytest  # noqa: E402


def _completed(stdout="", returncode=0, stderr=""):
    return types.SimpleNamespace(stdout=stdout, returncode=returncode, stderr=stderr)


@pytest.fixture
def run(monkeypatch):
    """Script every subprocess call by the command's shape."""
    calls = []

    def fake(args, **kw):
        calls.append(args)
        if args[:2] == ["git", "rev-parse"] and "--is-shallow-repository" in args:
            return _completed(fake.shallow)
        if args[:2] == ["git", "log"]:
            return _completed(fake.changed)
        if args[:2] == ["gh", "repo"]:
            return _completed("calvinchengx/fabric-emulator")
        if args[:2] == ["gh", "api"]:
            return _completed(fake.api, fake.api_rc, fake.api_err)
        if args[:3] == ["gh", "run", "list"]:
            return _completed(fake.runs, fake.runs_rc, fake.runs_err)
        return _completed()

    fake.shallow, fake.changed = "false", "1000"
    fake.api, fake.api_rc, fake.api_err = "", 0, ""
    fake.runs, fake.runs_rc, fake.runs_err = '[{"createdAt":"2030-01-01T00:00:00Z","conclusion":"success"}]', 0, ""
    fake.calls = calls
    monkeypatch.setattr(subprocess, "run", fake)
    monkeypatch.setattr(cwf.subprocess, "run", fake)
    monkeypatch.delenv("FRESHNESS_STRICT", raising=False)
    return fake


def test_a_run_after_the_change_is_fresh(run, capsys):
    assert cwf.main() == 0
    assert "has run since its last change" in capsys.readouterr().out


def test_a_run_before_the_change_is_stale(run, capsys):
    run.runs = '[{"createdAt":"1970-01-01T00:00:10Z","conclusion":"success"}]'
    assert cwf.main() == 1
    out = capsys.readouterr().out
    assert "changed since they last ran" in out
    assert "gh workflow run" in out, "the failure names no remediation"


def test_a_red_last_run_is_still_fresh(run, capsys):
    """Freshness is 'did it execute', not 'did it pass'. A failing cron run
    reports itself; conflating the two would hide staleness behind a red."""
    run.runs = '[{"createdAt":"2030-01-01T00:00:00Z","conclusion":"failure"}]'
    assert cwf.main() == 0


def test_unreadable_run_history_skips_when_not_strict(run, capsys):
    run.runs, run.runs_rc, run.runs_err = "", 1, "HTTP 403"
    assert cwf.main() == 0
    assert "HTTP 403" in capsys.readouterr().out, "the reason was swallowed"


def test_unreadable_run_history_fails_when_strict(run, monkeypatch, capsys):
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    run.runs, run.runs_rc, run.runs_err = "", 1, "HTTP 403"
    assert cwf.main() == 1
    assert "could not be read" in capsys.readouterr().out


def test_a_shallow_clone_reads_dates_from_the_api(run, capsys):
    """git log on a depth-1 clone returns the TIP date for every file, so every
    workflow looks changed-just-now. This is the fallback that replaced it."""
    run.shallow = "true"
    run.api = "2020-01-01T00:00:00Z"
    assert cwf.main() == 0
    out = capsys.readouterr().out
    assert "shallow clone" in out
    assert any(a[:2] == ["gh", "api"] for a in run.calls), "the API was never consulted"


def test_the_api_call_is_pinned_to_a_sha(run, monkeypatch):
    """Without `sha=` the API answers for the DEFAULT BRANCH, so a PR changing a
    cron workflow reads main's history and is never flagged — the one case this
    check exists for."""
    run.shallow = "true"
    run.api = "2020-01-01T00:00:00Z"
    monkeypatch.setenv("GITHUB_SHA", "deadbeef")
    cwf.main()
    api = [a for a in run.calls if a[:2] == ["gh", "api"]][0]
    assert "sha=deadbeef" in api, f"not pinned: {api}"
    # and the arguments must be well formed — a splice bug once produced
    # `-f -f sha=S path=P`, which failed every lookup silently.
    assert "-f -f" not in " ".join(api)
    for i, tok in enumerate(api):
        if tok == "-f":
            assert "=" in api[i + 1], f"-f without a key=value: {api}"


def test_an_unreadable_change_date_is_reported_not_silent(run, capsys):
    run.shallow = "true"
    run.api, run.api_rc, run.api_err = "", 1, "HTTP 404"
    assert cwf.main() == 0
    assert "HTTP 404" in capsys.readouterr().out


def test_no_gh_skips_quietly_but_fails_under_strict(run, monkeypatch, capsys):
    monkeypatch.setattr(cwf.shutil, "which", lambda _n: None)
    assert cwf.main() == 0
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    assert cwf.main() == 1


def test_utc_formats_as_utc():
    assert cwf._utc(0) == "1970-01-01 00:00Z"
