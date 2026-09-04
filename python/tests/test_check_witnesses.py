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
import json
import sys
import textwrap
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_witnesses", REPO / "scripts" / "check_witnesses.py")
# spec_from_file_location returns Optional; a None here means the path
# is wrong, and failing at import beats an AttributeError mid-test.
assert spec and spec.loader
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
    cw.README = REPO / "README.md"
    sys.argv = ["check_witnesses.py", "--strict"]
    assert cw.main() == 0


# --- README glance table ---------------------------------------------------
#
# The landing page interpolates the Real count; README typed it, and the typed
# copy sat at 113 after the checker had moved. --strict now fails that. The
# tests below drive the mismatch from the failing side: a checker over a
# README that currently matches passes whether or not it looks at the cell.


def test_readme_real_count_matches():
    text = "| 🟢 **Real** | **7** | Witnessed |\n"
    assert cw.readme_real_mismatch(7, text) is None


def test_readme_real_count_mismatch_is_the_bug_this_binds():
    """The live case: glance table 113, checker 124."""
    text = "| 🟢 **Real** | **113** | Witnessed |\n"
    msg = cw.readme_real_mismatch(124, text)
    assert msg is not None
    assert "113" in msg
    assert "124" in msg


def test_readme_real_count_missing_row_is_reported():
    """Deleting the table must fail, not silently unbind the count."""
    msg = cw.readme_real_mismatch(124, "# Parity\n\nNo glance table.\n")
    assert msg is not None
    assert "no" in msg.lower()


def test_readme_real_count_unbolded_number_does_not_match():
    """`| 🟢 **Real** | 124 |` is a rewrite this regex does not see."""
    text = "| 🟢 **Real** | 124 | Witnessed |\n"
    msg = cw.readme_real_mismatch(124, text)
    assert msg is not None


def test_readme_real_count_duplicate_rows_are_reported():
    text = "| 🟢 **Real** | **1** | a |\n| 🟢 **Real** | **2** | b |\n"
    msg = cw.readme_real_mismatch(1, text)
    assert msg is not None
    assert "2" in msg


def test_strict_fails_when_readme_count_is_stale(tmp_path, capsys):
    """Wiring: a helper that is never called still leaves --strict green."""
    stale = tmp_path / "README.md"
    stale.write_text("| 🟢 **Real** | **113** | Witnessed |\n", encoding="utf-8")
    cw.ROOT = REPO
    cw.PARITY = REPO / "docs" / "parity.md"
    cw.MANIFEST = REPO / "docs" / "witnesses.json"
    cw.CI = REPO / ".github" / "workflows" / "ci.yml"
    cw.README = stale
    sys.argv = ["check_witnesses.py", "--strict"]
    assert cw.main() == 1
    out = capsys.readouterr().out
    assert "113" in out
    assert "README" in out
    assert "FAIL:" in out


def test_non_strict_reports_a_stale_readme_without_failing(tmp_path, capsys):
    stale = tmp_path / "README.md"
    stale.write_text("| 🟢 **Real** | **113** | Witnessed |\n", encoding="utf-8")
    cw.ROOT = REPO
    cw.PARITY = REPO / "docs" / "parity.md"
    cw.MANIFEST = REPO / "docs" / "witnesses.json"
    cw.CI = REPO / ".github" / "workflows" / "ci.yml"
    cw.README = stale
    sys.argv = ["check_witnesses.py"]
    assert cw.main() == 0
    assert "113" in capsys.readouterr().out


# --- the ecosystem-conformance table -----------------------------------------
#
# That section is SKIPPED by the claim scanner, correctly: its rows are clients,
# not capabilities. Which left its 🟢 marks governed by nothing, and two real
# client suites sitting outside the manifest — `dbt-fabricspark`, which runs in
# five CI jobs and was cited by no claim, and `vscode-extension`. The tests
# below drive the reconciliation from the failing side, because a checker over
# a table that currently satisfies it passes whether or not it works.

@pytest.fixture(autouse=True)
def _restore_module_globals():
    """Every test here rebinds module-level paths and maps on `cw`.

    Autouse and module-wide on purpose: without it the last test to run leaves
    its tmp_path bound to cw.PARITY, and whichever test is appended next reads
    a directory that pytest has deleted. Nothing had bitten yet, which is the
    only reason it was safe to leave.
    """
    saved = {name: getattr(cw, name)
             for name in ("PARITY", "MANIFEST", "ROOT", "CI", "README",
                          "JOB_FOR", "UNCREDITED")
             if hasattr(cw, name)}
    yield
    for name, value in saved.items():
        setattr(cw, name, value)


ECO_DOC = textwrap.dedent("""\
    # Parity

    ## Platform

    | Fabric feature | Notes | Status |
    |---|---|---|
    | Workspaces CRUD | crud | 🟢 Real |

    ## Ecosystem conformance: real OSS/vendor clients as witnesses

    | Real client (pinned) | Surface exercised | Status |
    |---|---|---|
    | `some-client` | Control plane | 🟢 `e2e/some-client` — debug→run |
    | `go-mssqldb` | TDS | 🟢 `internal/server`, `internal/tds` |
    | `not-yet` | Nothing | 🔴 planned |

    Prose underneath mentioning `e2e/type-map`, which is a probe, not a row.
    """)


def eco(tmp_path, manifest, text=ECO_DOC, job_for=None, uncredited=None):
    p = tmp_path / "parity.md"
    p.write_text(text, encoding="utf-8")
    cw.PARITY = p
    cw.JOB_FOR = {} if job_for is None else job_for
    cw.UNCREDITED = {} if uncredited is None else uncredited
    return cw.ecosystem_gaps(manifest)


def cited(*jobs):
    return {"a-claim": {"witnesses": [f"ci:{j}" for j in jobs]}}


def test_a_credited_client_row_is_clean(tmp_path):
    assert eco(tmp_path, cited("some-client")) == []


def test_a_client_row_nothing_cites_is_reported(tmp_path):
    """The live case. Deleting that CI job would turn nothing red."""
    problems = eco(tmp_path, cited("something-else"))
    assert len(problems) == 1
    assert "e2e/some-client" in problems[0]
    assert "ci:some-client" in problems[0]


def test_a_row_naming_no_e2e_suite_is_not_checked(tmp_path):
    """`go-mssqldb` is witnessed from internal/, and that is legitimate."""
    assert all("mssqldb" not in p for p in eco(tmp_path, cited("some-client")))


def test_an_unsupported_row_is_not_required_to_have_a_witness(tmp_path):
    assert all("not-yet" not in p for p in eco(tmp_path, cited("some-client")))


def test_prose_below_the_table_is_not_read_as_a_row(tmp_path):
    """Reading the whole section reported `e2e/type-map` — a probe another
    suite invokes — as an uncredited witness. A false alarm gets a gate
    switched off, so the parser takes rows, not section text."""
    assert all("type-map" not in p for p in eco(tmp_path, cited("some-client")))


def test_a_suite_whose_job_is_named_differently_resolves_through_job_for(tmp_path):
    assert eco(tmp_path, cited("client-job"),
               job_for={"some-client": "client-job"}) == []


def test_a_stale_job_for_entry_is_reported(tmp_path):
    """A mapping to a job that witnesses nothing: renamed again, or wrong all
    along. Either way it is a declaration nothing checks, which is the shape of
    the original bug."""
    problems = eco(tmp_path, cited("some-client"),
                   job_for={"some-client": "gone-away"})
    assert any("JOB_FOR" in p and "gone-away" in p for p in problems)


def test_an_exempt_suite_is_not_required_to_be_credited(tmp_path):
    assert eco(tmp_path, cited("nothing"),
               uncredited={"some-client": "no graded row covers it"}) == []


def test_an_exemption_that_is_now_credited_is_reported(tmp_path):
    """Exemptions describe the present or they describe the past."""
    problems = eco(tmp_path, cited("some-client"),
                   uncredited={"some-client": "stale reason"})
    assert any("UNCREDITED" in p for p in problems)


def test_a_map_without_the_section_is_not_forced_to_have_one(tmp_path):
    """A synthetic map in a unit test has no ecosystem table, and requiring one
    is how #367 broke four tests: an invariant its own fixtures could not
    satisfy. That THIS repo's map carries the section is asserted against the
    repo, below, where it can be true."""
    text = ECO_DOC.split("## Ecosystem")[0]
    assert eco(tmp_path, cited("some-client"), text=text) == []


def test_a_table_with_no_suite_at_all_is_an_error_not_a_pass(tmp_path):
    """Vacuous success is the failure mode this whole file exists for."""
    text = ECO_DOC.replace("🟢 `e2e/some-client` — debug→run", "🟢 somewhere")
    with pytest.raises(cw.Unreadable, match=r"vacuous"):
        eco(tmp_path, cited("some-client"), text=text)


def test_the_committed_map_still_has_a_readable_ecosystem_table():
    """The control the generic tests cannot give: all of them build their own
    table, so every one would pass with the real section renamed away and the
    reconciliation quietly disabled."""
    cw.PARITY = REPO / "docs" / "parity.md"
    suites = cw.ecosystem_suites()
    assert len(suites) > 10, suites
    assert "dbt-fabricspark" in suites


def test_the_committed_map_credits_dbt_fabricspark(tmp_path):
    """The regression this was opened for: Microsoft's Spark adapter drives the
    Livy high-concurrency surface and was credited to no claim, while the two
    `ci:dbt-fabric` citations sat on the warehouse rows."""
    manifest = json.loads((REPO / "docs" / "witnesses.json").read_text(encoding="utf-8"))
    cites = {w for entry in manifest.values() if isinstance(entry, dict)
             for w in entry.get("witnesses", [])}
    assert "ci:dbt-fabricspark" in cites
