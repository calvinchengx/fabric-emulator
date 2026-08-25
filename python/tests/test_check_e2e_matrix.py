"""The e2e matrix is an index, and an index that silently omits is worse than none.

`docs/12-e2e-matrix.md` answers "what is proven end to end, and by which real
client". A suite missing from it does not read as undocumented — the table reads
as complete, so the absence says the proof does not exist.

Found the way these things are found: `e2e/notebook-display` shipped, ran in CI,
and was invisible there. Nothing was going to catch it.

The tests below are about the checker's two silent failures — crediting a
mention that is not one, and demanding a spelling the matrix does not use — not
about today's counts, which change with every suite added.
"""
import importlib.util
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_e2e_matrix", REPO / "scripts" / "check_e2e_matrix.py")
assert spec and spec.loader
matrix = importlib.util.module_from_spec(spec)
sys.modules["check_e2e_matrix"] = matrix
spec.loader.exec_module(matrix)


@pytest.mark.parametrize("text", [
    "see `e2e/thing/run.py` for this",
    "driven by e2e/thing/driver.py",
    "the e2e/thing suite",
    "`e2e/thing`",
])
def test_any_reference_to_the_directory_counts(text):
    """ONE SPELLING WOULD BE WRONG. The first version demanded a backticked
    `e2e/<name>/run.py` and reported six correctly-described suites as missing —
    the matrix also writes `e2e/notebook-run/real_fabric.py`, `e2e/spark-jvm`
    and `e2e/medallion-dbt-fabricspark/`. That would have meant editing good
    rows to satisfy a checker."""
    assert matrix.is_described("thing", text) is True


@pytest.mark.parametrize("text", [
    "e2e/thing-advanced/run.py",
    "e2e/thingy",
    "e2e/other/run.py",
])
def test_a_longer_name_does_not_claim_a_shorter_one_s_row(text):
    """`e2e/medallion` must not be credited by the row describing
    `e2e/medallion-advanced`: they are different suites proving different
    things, and one row cannot stand for both."""
    assert matrix.is_described("thing", text) is False


def test_the_committed_tree_accounts_for_every_suite():
    """Either described, or named in UNDOCUMENTED as owing a description.
    Nothing silently absent."""
    assert matrix.main(["--strict"]) == 0


def test_a_new_suite_that_nobody_documented_fails(monkeypatch):
    """The property the check exists for. A suite can be added, run in CI, and
    be invisible in the index — which is what happened."""
    real = matrix.suites  # captured BEFORE patching, or the lambda calls itself
    monkeypatch.setattr(matrix, "suites", lambda: [*real(), "brand-new-suite"])
    assert matrix.main(["--strict"]) == 1


def test_a_backlog_entry_that_is_now_documented_fails(monkeypatch):
    """UNDOCUMENTED can only shrink. An entry left there after its row is
    written turns a debt into furniture — which is exactly how two of
    `check_notebookutils_surface`'s waivers went stale."""
    described = "notebook-display"
    monkeypatch.setitem(matrix.UNDOCUMENTED, described, "not true any more")
    assert matrix.main(["--strict"]) == 1


def test_a_backlog_entry_for_a_deleted_suite_fails(monkeypatch):
    monkeypatch.setitem(matrix.UNDOCUMENTED, "suite-that-was-deleted", "gone")
    assert matrix.main(["--strict"]) == 1


def test_no_suites_at_all_is_vacuous_rather_than_clean(monkeypatch, tmp_path):
    """A moved directory must not turn this gate into a decoration."""
    monkeypatch.setattr(matrix, "E2E", tmp_path)
    with pytest.raises(matrix.Unreadable, match="vacuous"):
        matrix.suites()


def test_a_missing_e2e_tree_is_an_error(monkeypatch, tmp_path):
    monkeypatch.setattr(matrix, "E2E", tmp_path / "gone")
    with pytest.raises(matrix.Unreadable, match="missing"):
        matrix.suites()
