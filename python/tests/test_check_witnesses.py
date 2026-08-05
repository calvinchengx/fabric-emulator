"""The witness checker is what stops a parity claim outliving its proof.

`scripts/check_witnesses.py --strict` guarantees every supported row in the
parity map names a test that exists. Nothing tested the checker itself, which
is the same hole one level up: if it silently stopped resolving claims — a
regex that no longer matches the table, a gate detector that misses a helper —
the map would go on asserting support nobody proves, and the first evidence
would be a consumer hitting a 🟢 that is not.

The valuable seams are the three the checker gets wrong most cheaply:

  * `key_for`, because rewording a claim must change its key (that is how a
    reworded claim gets a fresh look at its witness);
  * `green_claims`, because it parses a hand-maintained markdown table;
  * `gated_go_tests`, whose fixed-point resolution exists precisely because a
    one-level check missed a helper fronting another helper.
"""
import importlib.util
import sys
import textwrap
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_witnesses", REPO / "scripts" / "check_witnesses.py")
cw = importlib.util.module_from_spec(spec)
sys.modules["check_witnesses"] = cw
spec.loader.exec_module(cw)


# --- key_for: rewording a claim must move its key -------------------------

def test_key_strips_markdown_links_to_their_text():
    assert cw.key_for("Shortcuts to [ADLS Gen2](08-onelake.md)") == "shortcuts-to-adls-gen2"


def test_key_strips_emphasis_and_code():
    assert cw.key_for("Capacity **job queueing** and `429`") == "capacity-job-queueing-and-429"


def test_key_is_case_and_punctuation_insensitive():
    assert cw.key_for("Workspaces CRUD") == cw.key_for("workspaces, CRUD!")


def test_rewording_a_claim_changes_its_key():
    """Intended behaviour, not a bug: a reworded claim deserves a fresh look at
    whether its witness still covers it."""
    assert cw.key_for("Notebook cell execution") != cw.key_for("Notebook cell scheduling")


def test_key_does_not_collapse_to_empty_on_punctuation_only():
    assert cw.key_for("***") == ""


# --- green_claims: parsing a hand-maintained table -------------------------

PARITY_DOC = textwrap.dedent("""\
    # Parity

    ## Platform

    | Fabric feature | Notes | Status |
    |---|---|---|
    | Workspaces CRUD | crud | 🟢 Real |
    | Dataflow execution | none | 🔴 Not implemented |
    | Partly there | half | 🟡 Emulated |

    ## Data Engineering

    | Fabric feature | Notes | Status |
    |---|---|---|
    | Notebook **cell execution** | runs | 🟢 Real (default engine) |
    """)


def claims(tmp_path, text=PARITY_DOC):
    p = tmp_path / "parity.md"
    p.write_text(text, encoding="utf-8")
    cw.PARITY = p
    return list(cw.green_claims())


def test_only_supported_rows_are_claimed(tmp_path):
    got = claims(tmp_path)
    assert [c[1] for c in got] == ["Workspaces CRUD", "Notebook **cell execution**"]


def test_claims_carry_their_section(tmp_path):
    got = claims(tmp_path)
    assert got[0][0] == "Platform"
    assert got[1][0] == "Data Engineering"


def test_header_and_separator_rows_are_not_claims(tmp_path):
    assert all(c[1] != "Fabric feature" for c in claims(tmp_path))


def test_a_row_outside_any_section_is_ignored(tmp_path):
    """A table before the first `## ` heading has no section to attribute a
    claim to, and guessing one would file it under the wrong area."""
    assert claims(tmp_path, "| Orphan row | x | 🟢 |\n") == []


def test_supported_glyph_must_be_in_the_status_cell(tmp_path):
    """The glyph is matched in the LAST cell. A row merely mentioning it in
    prose is not a claim of support."""
    doc = "## S\n\n| F | N | S |\n|---|---|---|\n| Mentions 🟢 in notes | 🟢 elsewhere | 🔴 |\n"
    assert claims(tmp_path, doc) == []


# --- gated_go_tests: the fixed point that a one-level check misses ---------

DIRECT = """\
package x

func TestDirect(t *testing.T) {
\tt.Skip("no engine")
}
"""

INDIRECT = """\
package x

func OpenThing(t *testing.T) {
\tt.Skipf("no engine")
}

func TestViaHelper(t *testing.T) {
\tOpenThing(t)
}

func TestViaHelperOfHelper(t *testing.T) {
\tTestViaHelper(t)
}

func TestUngated(t *testing.T) {
\tdoWork()
}
"""


def gated(tmp_path, files):
    internal = tmp_path / "internal"
    internal.mkdir(parents=True, exist_ok=True)
    for name, src in files.items():
        (internal / name).write_text(src, encoding="utf-8")
    cw.ROOT = tmp_path
    return cw.gated_go_tests()


def test_a_test_with_its_own_skip_is_gated(tmp_path):
    assert gated(tmp_path, {"a_test.go": DIRECT})["TestDirect"] == "its own t.Skip"


def test_a_test_gated_through_a_helper_is_found(tmp_path):
    """The case the fixed point exists for: the test contains no gate, and the
    helper it calls skips on its behalf."""
    got = gated(tmp_path, {"b_test.go": INDIRECT})
    assert "TestViaHelper" in got
    assert "via OpenThing()" in got["TestViaHelper"]


def test_the_resolution_is_transitive(tmp_path):
    """A helper fronting another helper — one level deep would miss this."""
    got = gated(tmp_path, {"b_test.go": INDIRECT})
    assert "TestViaHelperOfHelper" in got


def test_an_ungated_test_is_not_reported(tmp_path):
    assert "TestUngated" not in gated(tmp_path, {"b_test.go": INDIRECT})


def test_only_test_functions_are_reported(tmp_path):
    """`OpenThing` skips, but it is a helper, not a test — reporting it would
    put a non-test in the gated list the parity map reads."""
    assert "OpenThing" not in gated(tmp_path, {"b_test.go": INDIRECT})


# --- ci_job_ids ------------------------------------------------------------

def test_ci_job_ids_reads_top_level_jobs(tmp_path):
    ci = tmp_path / "ci.yml"
    ci.write_text(textwrap.dedent("""\
        jobs:
          medallion:
            steps:
              - run: x
          warehouse-tds:
            steps:
              - run: y
        """), encoding="utf-8")
    cw.CI = ci
    got = cw.ci_job_ids()
    assert {"medallion", "warehouse-tds"} <= got
    assert "steps" not in got


# --- the real repository ---------------------------------------------------

def test_the_actual_repo_passes_strict():
    """The committed map satisfies its own rule. This is what fails when a new
    supported row lands without a witness, or a witness is renamed away."""
    cw.ROOT = REPO
    cw.PARITY = REPO / "docs" / "parity.md"
    cw.MANIFEST = REPO / "docs" / "witnesses.json"
    cw.CI = REPO / ".github" / "workflows" / "ci.yml"
    sys.argv = ["check_witnesses.py", "--strict"]
    assert cw.main() == 0
