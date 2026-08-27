"""The conformance checker must actually catch drift, not merely run.

`scripts/check_conformance.py` is the thing that enforces the correspondence
between the contracts docs/38 defines and the backends CI proves them on. A
checker nobody tests is the same failure it exists to prevent, one level up: it
stays green while quietly catching nothing, and the first evidence is a contract
that was never proven by anybody.

So each case below breaks the tree in one specific way and asserts the checker
says so. The happy path is checked against the REAL repository documents, not a
fixture, because a fixture would keep passing after docs/38 changed shape.
"""
import importlib.util
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_conformance", REPO / "scripts" / "check_conformance.py")
# spec_from_file_location returns Optional; a None here means the path
# is wrong, and failing at import beats an AttributeError mid-test.
assert spec and spec.loader
cc = importlib.util.module_from_spec(spec)
sys.modules["check_conformance"] = cc
spec.loader.exec_module(cc)


GOOD_DOC = """# 38 — Framework conformance

### 1. First contract

prose

### 2. Second contract

prose

<!-- APPLICABILITY:BEGIN (scripts/check_conformance.py parses this table) -->

| # | Contract | sail | jvm | warehouse |
|---|---|---|---|---|
| 1 | First contract | required | required | n/a |
| 2 | Second contract | required | control | required |

<!-- APPLICABILITY:END -->
"""


def write(tmp_path, doc=GOOD_DOC, matrix=None, witnesses=None):
    """Point the checker at a throwaway tree and return its exit code."""
    docs = tmp_path / "docs"
    docs.mkdir(exist_ok=True)
    (docs / "38.md").write_text(doc, encoding="utf-8")
    cc.DOC = docs / "38.md"
    cc.MATRIX = docs / "conformance-matrix.md"
    cc.WITNESSES = docs / "witnesses.json"
    if matrix is not None:
        cc.MATRIX.write_text(matrix, encoding="utf-8")
    if witnesses is not None:
        cc.WITNESSES.write_text(witnesses, encoding="utf-8")


def run(argv=()):
    sys.argv = ["check_conformance.py", *argv]
    return cc.main()


# --- the real repository, which must pass ---------------------------------

def test_the_actual_repo_passes():
    """docs/38 as committed satisfies its own invariant.

    Deliberately not a fixture: this is what fails if someone adds a contract
    section and forgets the applicability row.
    """
    cc.DOC = REPO / "docs" / "38-framework-conformance.md"
    cc.MATRIX = REPO / "docs" / "conformance-matrix.md"
    cc.WITNESSES = REPO / "docs" / "witnesses.json"
    assert run() == 0


def test_the_actual_repo_passes_strict():
    """The matrix has landed, so --strict must pass on the real tree."""
    cc.DOC = REPO / "docs" / "38-framework-conformance.md"
    cc.MATRIX = REPO / "docs" / "conformance-matrix.md"
    cc.WITNESSES = REPO / "docs" / "witnesses.json"
    assert run(["--strict"]) == 0


def test_the_actual_repo_defines_every_contract_it_promises():
    """Eight contracts, and none of them n/a everywhere."""
    cc.DOC = REPO / "docs" / "38-framework-conformance.md"
    table, errors = cc.load_applicability()
    assert errors == []
    assert len(table) == 8
    # Contract 8 is required everywhere: the refusals differ per surface —
    # OneLake's on the lakehouse, Class B strict on the warehouse — but no
    # surface is without one, so n/a would be a decision rather than a fact.
    assert set(table[8].values()) == {"required"}
    assert set(table[4]) == {"sail", "jvm", "warehouse"}
    # Contract 6 is the one the JVM column exists to make provable.
    assert table[6]["jvm"] == "control"
    # Contracts 1-3 are notebook-session properties; Warehouse has no session.
    assert table[1]["warehouse"] == "n/a"


# --- document drift --------------------------------------------------------

def test_contract_defined_but_never_assigned(tmp_path, capsys):
    doc = GOOD_DOC.replace("### 2. Second contract", "### 3. Orphan\n\nprose\n\n### 2. Second contract")
    write(tmp_path, doc=doc)
    assert run() == 1
    assert "contract 3 (Orphan) is defined but missing" in capsys.readouterr().out


def test_contract_assigned_but_never_defined(tmp_path, capsys):
    doc = GOOD_DOC.replace("| 2 | Second contract | required | control | required |",
                           "| 2 | Second contract | required | control | required |\n"
                           "| 9 | Ghost | required | required | required |")
    write(tmp_path, doc=doc)
    assert run() == 1
    assert "contract 9, which no '### 9.' section defines" in capsys.readouterr().out


def test_contract_na_on_every_backend_proves_nothing(tmp_path, capsys):
    doc = GOOD_DOC.replace("| 1 | First contract | required | required | n/a |",
                           "| 1 | First contract | n/a | n/a | n/a |")
    write(tmp_path, doc=doc)
    assert run() == 1
    assert "n/a on every backend, so nothing would ever prove it" in capsys.readouterr().out


def test_unknown_applicability_value(tmp_path, capsys):
    doc = GOOD_DOC.replace("| 1 | First contract | required | required | n/a |",
                           "| 1 | First contract | maybe | required | n/a |")
    write(tmp_path, doc=doc)
    assert run() == 1
    assert "'maybe' is not one of" in capsys.readouterr().out.replace('"', "'")


def test_missing_applicability_block(tmp_path, capsys):
    write(tmp_path, doc="# 38\n\n### 1. Lonely\n\nprose\n")
    assert run() == 1
    assert "no APPLICABILITY:BEGIN/END block" in capsys.readouterr().out


def test_row_with_wrong_column_count(tmp_path, capsys):
    doc = GOOD_DOC.replace("| 1 | First contract | required | required | n/a |",
                           "| 1 | First contract | required |")
    write(tmp_path, doc=doc)
    assert run() == 1
    assert "columns, expected" in capsys.readouterr().out


def test_em_dash_reads_as_not_applicable(tmp_path):
    """The table may spell n/a as an em dash; both mean the same thing."""
    doc = GOOD_DOC.replace("| 1 | First contract | required | required | n/a |",
                           "| 1 | First contract | required | required | — |")
    write(tmp_path, doc=doc)
    assert run() == 0


# --- the matrix ------------------------------------------------------------

MATRIX_OK = """# Conformance matrix

| # | Contract | sail | jvm | warehouse |
|---|---|---|---|---|
| 1 | First contract | ✅ | ✅ | — |
| 2 | Second contract | ✅ | ✅ | ✅ |
"""


def test_matrix_absent_is_a_loud_note_not_a_silent_pass(tmp_path, capsys):
    write(tmp_path)
    assert run() == 0
    out = capsys.readouterr().out
    assert "does not exist yet" in out and "NOT enforced" in out


def test_matrix_absent_fails_under_strict(tmp_path, capsys):
    write(tmp_path)
    assert run(["--strict"]) == 1
    assert "missing and --strict was given" in capsys.readouterr().out


def test_matrix_covering_every_cell_passes(tmp_path):
    write(tmp_path, matrix=MATRIX_OK)
    assert run() == 0


def test_matrix_missing_a_required_cell(tmp_path, capsys):
    matrix = MATRIX_OK.replace("| 2 | Second contract | ✅ | ✅ | ✅ |\n", "")
    write(tmp_path, matrix=matrix)
    assert run() == 1
    out = capsys.readouterr().out
    assert "contract 2/sail is required but" in out
    # the `control` cell must be demanded too, not just the required ones
    assert "contract 2/jvm is control but" in out


def test_failing_cell_needs_a_pointer(tmp_path, capsys):
    matrix = MATRIX_OK.replace("| 1 | First contract | ✅ | ✅ | — |",
                               "| 1 | First contract | ❌ | ✅ | — |")
    write(tmp_path, matrix=matrix)
    assert run() == 1
    assert "❌ with no pointer" in capsys.readouterr().out


@pytest.mark.parametrize("cell", ["❌ see [37](37.md)", "❌ #412"])
def test_failing_cell_with_a_pointer_is_allowed(tmp_path, cell):
    """The kit lands before every contract passes; a red with a pointer is the
    documented way to record that, so it must not fail the build."""
    matrix = MATRIX_OK.replace("| 1 | First contract | ✅ | ✅ | — |",
                               f"| 1 | First contract | {cell} | ✅ | — |")
    write(tmp_path, matrix=matrix)
    assert run() == 0


def test_na_cell_needs_no_verdict(tmp_path):
    """Contract 1 is n/a on warehouse, so an empty warehouse cell is correct."""
    matrix = MATRIX_OK.replace("| 1 | First contract | ✅ | ✅ | — |",
                               "| 1 | First contract | ✅ | ✅ |  |")
    write(tmp_path, matrix=matrix)
    assert run() == 0


# --- witnesses -------------------------------------------------------------

def test_matrix_naming_an_unknown_witness(tmp_path, capsys):
    matrix = MATRIX_OK.replace("| 2 | Second contract | ✅ | ✅ | ✅ |",
                               "| 2 | Second contract | ✅ | ✅ | ✅ ci:conformance-warehouse |")
    write(tmp_path, matrix=matrix, witnesses='{"other": {"witnesses": ["ci:x"]}}')
    assert run() == 1
    assert "names witness ci:conformance-warehouse" in capsys.readouterr().out


def test_known_witness_passes(tmp_path):
    matrix = MATRIX_OK.replace("| 2 | Second contract | ✅ | ✅ | ✅ |",
                               "| 2 | Second contract | ✅ | ✅ | ✅ ci:conformance-warehouse |")
    write(tmp_path, matrix=matrix,
          witnesses='{"c": {"witnesses": ["ci:conformance-warehouse"]}}')
    assert run() == 0


def test_gated_witnesses_count_as_known(tmp_path):
    """A witness that skips without a backend is still a real witness — the
    `_gated` block is how the witness map records exactly that."""
    matrix = MATRIX_OK.replace("| 2 | Second contract | ✅ | ✅ | ✅ |",
                               "| 2 | Second contract | ✅ | ✅ | ✅ ci:conformance-warehouse |")
    write(tmp_path, matrix=matrix,
          witnesses='{"_gated": {"ci:conformance-warehouse": "needs SQL Server"}}')
    assert run() == 0


def test_missing_doc_fails(tmp_path, capsys):
    cc.DOC = tmp_path / "nope.md"
    assert run() == 1
    assert "is missing" in capsys.readouterr().out
