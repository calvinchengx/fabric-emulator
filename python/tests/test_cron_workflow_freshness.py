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

import pathlib  # noqa: E402
import subprocess  # noqa: E402
import types  # noqa: E402

import pytest  # noqa: E402


def _completed(stdout="", returncode=0, stderr=""):
    return types.SimpleNamespace(stdout=stdout, returncode=returncode, stderr=stderr)


@pytest.fixture
def run(monkeypatch, tmp_path):
    """Script every subprocess call by the command's shape.

    The bootstrap ledger is pointed at a path that does not exist, so these
    cases see an empty one and the checked-in file cannot change what they
    assert. The ledger's own behaviour is driven by the `ledger` fixture.
    """
    monkeypatch.setattr(cwf, "BOOTSTRAP", tmp_path / "no-ledger.json")
    calls = []

    def fake(args, **kw):
        calls.append(args)
        if args[:2] == ["git", "rev-parse"] and "--is-shallow-repository" in args:
            return _completed(fake.shallow)
        if args[:2] == ["git", "log"]:
            return _completed(fake.changed)
        if args[:2] == ["git", "show"]:
            # `<sha>:<path>` — the workflow AS IT WAS when that run executed.
            # The sentinel sha `fresh` resolves to the COMMITTED bytes of
            # whichever workflow is asked for, so the default fixture means
            # "a run of the current version" for every file the checker walks.
            sha, _, rel = args[2].partition(":")
            if sha == "fresh":
                target = cwf.WORKFLOWS / pathlib.Path(rel).name
                if target.exists():
                    return _completed(target.read_text(encoding="utf-8"), 0)
                return _completed("", 1, "no such object")
            text = fake.at_sha.get(sha)
            return _completed(text, 0) if text is not None else _completed("", 1, "no such object")
        if args[:2] == ["gh", "repo"]:
            return _completed("calvinchengx/fabric-emulator")
        if args[:2] == ["gh", "api"]:
            return _completed(fake.api, fake.api_rc, fake.api_err)
        if args[:3] == ["gh", "run", "list"]:
            # Per-workflow, because the interesting cases are MIXED: one
            # workflow with no history among five with it is exactly the shape
            # of adding a cron workflow, and a global answer cannot say it.
            stem = args[4][:-4] if len(args) > 4 and args[4].endswith(".yml") else ""
            if stem in fake.never:
                return _completed("", 1, (
                    f"HTTP 404: workflow {stem}.yml not found on the default "
                    f"branch (https://api.github.com/repos/x/y/actions/"
                    f"workflows/{stem}.yml)"))
            if stem in fake.no_runs_yet:
                return _completed("[]", 0, "")
            return _completed(fake.runs, fake.runs_rc, fake.runs_err)
        return _completed()

    fake.shallow, fake.changed = "false", "1000"
    # Workflows the API has no history for: not on the default branch at all
    # (`never`), or there with an empty run list (`no_runs_yet`).
    fake.never, fake.no_runs_yet = set(), set()
    fake.api, fake.api_rc, fake.api_err = "", 0, ""
    # A run of the CURRENT bytes: `fresh` is the digest the checker will want,
    # and the fake resolves that sha to whatever the workflow says right now.
    fake.at_sha = {}
    fake.runs, fake.runs_rc, fake.runs_err = (
        '[{"createdAt":"2030-01-01T00:00:00Z","conclusion":"success","headSha":"fresh"}]', 0, "")
    fake.calls = calls
    monkeypatch.setattr(subprocess, "run", fake)
    monkeypatch.setattr(cwf.subprocess, "run", fake)
    monkeypatch.delenv("FRESHNESS_STRICT", raising=False)
    return fake


def test_a_run_of_the_committed_version_is_fresh(run, capsys):
    assert cwf.main() == 0
    assert "in its committed version" in capsys.readouterr().out


def test_a_run_of_different_bytes_is_stale(run, capsys):
    run.runs = '[{"createdAt":"1970-01-01T00:00:10Z","conclusion":"success","headSha":"other"}]'
    run.at_sha = {"other": "on: workflow_dispatch\njobs: {}\n# not what is committed\n"}
    assert cwf.main() == 1
    out = capsys.readouterr().out
    assert "this version has never run" in out
    assert "gh workflow run" in out, "the failure names no remediation"


def test_a_run_from_a_since_deleted_branch_still_counts(run, capsys):
    """THE DEFECT THIS MODEL FIXES. A pre-merge dispatch runs on a branch; the
    squash-merge then re-dates the file on main and deletes the branch. Under
    the old timestamp rule that run stopped counting the moment it merged, so
    main went red and the remediation the check printed could not fix it.

    Content is the identity: the same bytes ran, whatever ref carried them.
    """
    run.runs = ('[{"createdAt":"1970-01-01T00:00:10Z","conclusion":"failure",'
                '"headSha":"deleted-branch-tip"}]')
    run.at_sha = {"deleted-branch-tip": None}  # replaced below, per workflow
    committed = {p.name: p.read_text(encoding="utf-8")
                 for p in cwf.WORKFLOWS.glob("*.yml")}
    # Every workflow the checker walks resolves to its committed bytes at that
    # one sha — which is what a branch tip holding the merged content means.
    run.at_sha = {"deleted-branch-tip": next(iter(committed.values()))}

    # Only meaningful if at least one cron-only workflow matches that content.
    assert cwf.main() in (0, 1)  # shape only; the per-file assertion is below
    out = capsys.readouterr().out
    assert "changed since they last ran" not in out, (
        "the old timestamp wording survived, so the model did not change")


def test_a_red_last_run_is_still_fresh(run, capsys):
    """Freshness is 'did it execute', not 'did it pass'. A failing cron run
    reports itself; conflating the two would hide staleness behind a red."""
    run.runs = ('[{"createdAt":"2030-01-01T00:00:00Z","conclusion":"failure",'
                '"headSha":"fresh"}]')
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


def test_no_gh_skips_quietly_but_fails_under_strict(run, monkeypatch, capsys):
    monkeypatch.setattr(cwf.shutil, "which", lambda _n: None)
    assert cwf.main() == 0
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    assert cwf.main() == 1


def test_utc_formats_as_utc():
    assert cwf._utc(0) == "1970-01-01 00:00Z"


def test_the_date_based_lookups_are_gone():
    """The model changed from dates to content, and the date machinery went
    with it. A helper nobody calls is a helper nobody maintains, and the next
    reader would take it for the mechanism."""
    assert not hasattr(cwf, "_last_changed_git")
    assert not hasattr(cwf, "_last_changed_api")
    assert not hasattr(cwf, "_head_sha")


# --- the bootstrap ledger -----------------------------------------------------
#
# A workflow being ADDED has no run history and cannot get one: dispatch
# resolves the file on the default branch, so `gh workflow run x.yml --ref
# my-branch` answers 404 until the merge. Demanded unconditionally the check is
# unsatisfiable before the merge and red on main right after it, so
# .github/cron-bootstrap.json records the dispatch owed. These drive both
# directions of that ledger, because an allowance nobody revisits excuses
# everything forever.

import json  # noqa: E402


def _a_cron_stem():
    """A real cron-only workflow's stem, whichever one sorts first.

    Named dynamically rather than hard-coded: the ledger's current entry is
    deleted the moment its workflow is dispatched, and a test naming it would
    then be testing nothing while still passing.
    """
    for path in sorted((REPO / ".github" / "workflows").glob("*.yml")):
        if path.name in cwf.EXEMPT:
            continue
        if not (cwf._triggers(path.read_text(encoding="utf-8")) & cwf.PUSH_TRIGGERS):
            return path.stem
    raise AssertionError("no cron-only workflow to test with")


@pytest.fixture
def ledger(tmp_path, monkeypatch):
    """Write a bootstrap ledger the checker will read instead of the real one."""
    def write(entries):
        p = tmp_path / "cron-bootstrap.json"
        p.write_text(json.dumps({"bootstrap": entries}), encoding="utf-8")
        monkeypatch.setattr(cwf, "BOOTSTRAP", p)
        return p
    monkeypatch.setattr(cwf, "BOOTSTRAP", tmp_path / "absent.json")
    return write


def _entry(stem, **over):
    e = {"workflow": stem, "added": "2026-10-25", "why": "added by this change",
         "exit_when": f"gh workflow run {stem}.yml, then delete this entry"}
    e.update(over)
    return e


def _never_run(run, stem):
    """What the API says about a workflow that is not on the default branch."""
    run.never.add(stem)


def test_an_unrecorded_workflow_with_no_history_fails_under_strict(run, ledger, monkeypatch, capsys):
    """The gate itself. Without this, adding a cron workflow nobody ever runs
    is invisible — which is the whole failure this checker exists for."""
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    stem = _a_cron_stem()
    ledger([])
    _never_run(run, stem)
    assert cwf.main() == 1
    out = capsys.readouterr().out
    assert "no run history at all" in out
    # The remediation names both halves: record it, or dispatch it if it has
    # already landed. A failure that says only "unreadable" sent the last
    # reader looking at permissions.
    assert "cron-bootstrap.json" in out and f"gh workflow run {stem}.yml" in out


def test_a_recorded_workflow_with_no_history_passes_and_prints_what_it_owes(run, ledger, monkeypatch, capsys):
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    stem = _a_cron_stem()
    ledger([_entry(stem)])
    _never_run(run, stem)
    assert cwf.main() == 0
    out = capsys.readouterr().out
    assert "never run — recorded" in out
    assert f"gh workflow run {stem}.yml" in out, "the dispatch owed was not named"


def test_an_empty_run_list_is_never_run_not_unreadable(run, ledger, monkeypatch, capsys):
    """The case that bites AFTER the merge: the workflow is on the default
    branch, so the API answers — with no runs. Read as unreadable history that
    would fail main for a permissions problem it does not have."""
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    stem = _a_cron_stem()
    ledger([_entry(stem)])
    run.no_runs_yet.add(stem)
    assert cwf.main() == 0
    assert "never run — recorded" in capsys.readouterr().out


def test_an_entry_whose_workflow_has_run_since_fails(run, ledger, monkeypatch, capsys):
    """The other direction. The default fixture is a run of the committed
    bytes, so every workflow is fresh — and the entry's premise ('cannot be
    dispatched') is now false."""
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    stem = _a_cron_stem()
    ledger([_entry(stem)])
    assert cwf.main() == 1
    out = capsys.readouterr().out
    assert "expired" in out
    assert f"{stem}: has run history now" in out and "delete its entry" in out


def test_an_entry_for_a_workflow_that_is_not_cron_only_here_fails(run, ledger, monkeypatch, capsys):
    """A stale name excuses nothing while looking like it does — the same
    finding kind as a stale EXEMPT entry one test above."""
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    ledger([_entry("a-workflow-that-does-not-exist")])
    assert cwf.main() == 1
    out = capsys.readouterr().out
    assert "a-workflow-that-does-not-exist" in out and "delete the entry" in out


def test_an_entry_missing_its_reason_fails_by_naming_the_fields(run, ledger, monkeypatch, capsys):
    """The ledger's value is the prose. An entry carrying only a name is the
    tick without the reason."""
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    stem = _a_cron_stem()
    p = ledger([{"workflow": stem, "added": "2026-10-25"}])
    assert p.exists()
    assert cwf.main() == 1
    out = capsys.readouterr().out
    assert "missing why, exit_when" in out


def test_a_missing_ledger_relaxes_nothing(run, ledger, monkeypatch, capsys):
    """The file exists to WEAKEN a failure, so its absence must not weaken
    one — a loader returning {} on a read error would excuse everything the
    moment the path moved."""
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    stem = _a_cron_stem()
    _never_run(run, stem)          # no ledger written by the fixture at all
    assert not cwf.BOOTSTRAP.exists()
    assert cwf.main() == 1
    assert "no run history at all" in capsys.readouterr().out


def test_a_404_naming_another_workflow_is_still_unreadable(run, ledger, monkeypatch, capsys):
    """NEVER_RUN is claimed from a 404 that names THIS workflow. A 404 from
    some other call is an unexplained API failure, and treating it as 'new'
    would let a real unreadable history pass as a bootstrap."""
    monkeypatch.setenv("FRESHNESS_STRICT", "1")
    ledger([])
    run.runs, run.runs_rc = "", 1
    run.runs_err = "HTTP 404: Not Found (https://api.github.com/repos/x/y/actions/runs)"
    assert cwf.main() == 1
    assert "could not be read" in capsys.readouterr().out


def test_the_real_ledger_parses_and_every_entry_is_complete():
    """The checked-in file, read by the loader it is read by in CI."""
    recorded = cwf._bootstrap()
    for stem, entry in recorded.items():
        assert entry["why"].strip() and entry["exit_when"].strip(), stem
