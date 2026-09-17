"""The freshness checker must catch a summary that outlived its sub-plan.

`scripts/check_plan_freshness.py` exists because three rows in docs/24 pointed
maintainers at work that was already finished, and nothing noticed. A checker
for that failure which is itself untested would be the same defect one level
up — green, catching nothing, and only found when a fourth row goes stale.

Each case below breaks a throwaway tree in one specific way and asserts the
checker names it. The happy path runs against the REAL repository docs, because
a fixture keeps passing after docs/24 changes shape.
"""
import importlib.util
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_plan_freshness", REPO / "scripts" / "check_plan_freshness.py")
assert spec and spec.loader
pf = importlib.util.module_from_spec(spec)
sys.modules["check_plan_freshness"] = pf
spec.loader.exec_module(pf)


CLOSED_SUBPLAN = """# 37 — a sub-plan

## Order of work

| # | Capability | Size | Why this position |
|---|---|---|---|
| ~~1~~ ✅ | ~~First thing~~ | S | Done |
| ~~2~~ ✅ | ~~Second thing~~ | M | Done |
| 3b | Agent pool | L | Deferred; revisit only if it becomes real |
"""

OPEN_SUBPLAN = """# 37 — a sub-plan

## Order of work

| # | Capability | Size | Why this position |
|---|---|---|---|
| ~~1~~ ✅ | ~~First thing~~ | S | Done |
| 2 | Second thing | M | Not started |
"""

NARRATIVE_SUBPLAN = "# 37 — a sub-plan\n\n## Bounds\n\nprose only.\n"

STALE_ROW = ("| **Runtime divergences** | two of these are done; the rest "
             "remains. Scoped in [37-x.md](37-x.md) | XS-M remaining |\n")
CLOSED_ROW = ("| ~~**Runtime divergences**~~ ✅ | all closed. Scoped in "
              "[37-x.md](37-x.md) | — |\n")


def write(tmp_path, summary_row, subplan, monkeypatch):
    """Point the checker at a throwaway tree and return (exit code, output)."""
    docs = tmp_path / "docs"
    docs.mkdir(exist_ok=True)
    (docs / "24-parity-completion.md").write_text(
        "# 24\n\n| Gap | Scope | Size |\n|---|---|---|\n" + summary_row,
        encoding="utf-8")
    (docs / "37-x.md").write_text(subplan, encoding="utf-8")
    monkeypatch.setattr(pf, "DOCS", docs)
    monkeypatch.setattr(pf, "SUMMARY", docs / "24-parity-completion.md")
    monkeypatch.setattr(pf, "UNTRACKED_SUBPLANS", frozenset({"37-x.md"}))
    return pf.check()


def test_a_finished_subplan_under_a_row_still_asking_for_work_is_the_failure(
        tmp_path, monkeypatch):
    """The defect this script was written for, reproduced."""
    errors, _, checked = write(tmp_path, STALE_ROW, CLOSED_SUBPLAN, monkeypatch)
    assert checked == 1
    assert len(errors) == 1
    assert "37-x.md" in errors[0]
    assert "still asks for work" in errors[0]


def test_a_deferred_item_does_not_keep_a_subplan_open(tmp_path, monkeypatch):
    """A decision recorded is not a backlog.

    Without this, the deferred agent-per-session pool would hold docs/37 'open'
    forever and the checker would never fire at all — passing vacuously.
    """
    errors, _, _ = write(tmp_path, STALE_ROW, CLOSED_SUBPLAN, monkeypatch)
    assert errors, "the deferred row must not count as open work"


def test_an_open_subplan_leaves_its_row_alone(tmp_path, monkeypatch):
    """The check is one-directional on purpose: a summary may be coarser."""
    errors, _, checked = write(tmp_path, STALE_ROW, OPEN_SUBPLAN, monkeypatch)
    assert checked == 1
    assert errors == []


def test_a_closed_row_over_a_closed_subplan_passes(tmp_path, monkeypatch):
    errors, _, _ = write(tmp_path, CLOSED_ROW, CLOSED_SUBPLAN, monkeypatch)
    assert errors == []


def test_the_word_done_inside_prose_does_not_close_a_row(tmp_path, monkeypatch):
    """The real stale row said 'are done' while asking for two finished things.

    A looser done-marker read that as the row closing itself, and the checker
    reported ok against the very defect it was written for. Measured, not
    theorised.
    """
    row = ("| **Runtime divergences** | Environments are done; "
           "`input_file_name()` remains. [37-x.md](37-x.md) | XS-M remaining |\n")
    errors, _, _ = write(tmp_path, row, CLOSED_SUBPLAN, monkeypatch)
    assert errors, "'are done' in prose must not mark the row closed"


def test_a_narrative_subplan_is_reported_unchecked_not_passed(
        tmp_path, monkeypatch):
    """An unreadable sub-plan and a finished one must not print the same line."""
    errors, untracked, checked = write(
        tmp_path, STALE_ROW, NARRATIVE_SUBPLAN, monkeypatch)
    assert checked == 0
    assert untracked == {"37-x.md"}
    assert errors == []


def test_the_checklist_convention_is_read_too(tmp_path, monkeypatch):
    """docs/39 tracks phases as headings, not as an Order-of-work table."""
    subplan = ("# 39\n\n## Checklist\n\n"
               "### Phase 0 — pin the oracle ✅\n\nprose\n\n"
               "### Phase 1 — the work ✅\n\nprose\n")
    errors, _, checked = write(tmp_path, STALE_ROW, subplan, monkeypatch)
    assert checked == 1
    assert errors and "Checklist" in errors[0]


def test_an_unfinished_phase_leaves_the_row_alone(tmp_path, monkeypatch):
    subplan = ("# 39\n\n## Checklist\n\n"
               "### Phase 0 — pin the oracle ✅\n\nprose\n\n"
               "### Phase 1 — the work\n\nprose\n")
    errors, _, _ = write(tmp_path, STALE_ROW, subplan, monkeypatch)
    assert errors == []


def test_the_actual_repo_is_fresh():
    """The happy path, against the documents themselves."""
    errors, untracked, checked = pf.check()
    assert errors == [], errors
    assert checked >= 2, "no sub-plan was read — the check would be vacuous"
    assert untracked == set(pf.UNTRACKED_SUBPLANS), (
        "the allowlist and what the repo actually shows have diverged")


def test_the_check_is_registered_where_it_will_run():
    """A checker nobody runs is a file, not an invariant."""
    assert "check_plan_freshness.py --strict" in (
        REPO / "Makefile").read_text(encoding="utf-8")
    assert "check_plan_freshness.py --strict" in (
        REPO / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
