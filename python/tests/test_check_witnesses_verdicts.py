"""`check_witnesses --strict` is what keeps every 🟢 honest — so its REFUSALS matter.

`test_check_witnesses.py` covers the parsing helpers. What was never exercised
is `main()`: the verdicts. Each `FAIL` branch encodes a rule that took a real
incident to learn, and a rule whose enforcement is untested is a rule that can
stop enforcing without anyone noticing — which is precisely the failure mode
this script exists to prevent, turned on itself.

Every case below builds a small parity map and manifest in a tmp dir, so the
verdict under test is the only thing that can produce the exit code.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_witnesses as w  # noqa: E402


def parity(rows, section="Platform / fundamentals"):
    """A parity map with the given (feature, verdict) rows."""
    out = [f"## {section}", "", "| Fabric feature | Emulator | Type |", "|---|---|---|"]
    out += [f"| {feature} | does things | {verdict} |" for feature, verdict in rows]
    return "\n".join(out) + "\n"


@pytest.fixture
def repo(tmp_path, monkeypatch):
    """Point the checker at a parity map, a manifest and a CI file we control."""
    def build(rows, manifest, ci_jobs=("build", "governance")):
        p = tmp_path / "parity.md"
        p.write_text(parity(rows), encoding="utf-8")
        m = tmp_path / "witnesses.json"
        m.write_text(json.dumps(manifest), encoding="utf-8")
        c = tmp_path / "ci.yml"
        c.write_text("jobs:\n" + "".join(f"  {j}:\n    runs-on: x\n" for j in ci_jobs))
        monkeypatch.setattr(w, "PARITY", p)
        monkeypatch.setattr(w, "MANIFEST", m)
        monkeypatch.setattr(w, "CI", c)
        # Go-test discovery walks the real repo; pin it so a claim's witness is
        # decided by the manifest under test rather than by whatever exists.
        monkeypatch.setattr(w, "go_test_names", lambda: {"TestReal", "TestGated"})
        monkeypatch.setattr(w, "gated_go_tests", lambda: {"TestGated": "its own t.Skip"})
        monkeypatch.setattr(w, "py_test_names", lambda: {"test_real"})
    return build


def run(strict=True, monkeypatch=None):
    argv = ["check_witnesses.py"] + (["--strict"] if strict else [])
    sys_argv = sys.argv
    sys.argv = argv
    try:
        return w.main()
    finally:
        sys.argv = sys_argv


# --- the happy path -----------------------------------------------------------

def test_a_green_claim_with_a_real_witness_passes(repo, capsys):
    repo([("Widgets", "🟢 Real")],
         {"widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["go:TestReal"]}})
    assert run() == 0
    assert "supported capability claims: 1" in capsys.readouterr().out


def test_a_non_green_row_is_not_required_to_have_a_witness(repo):
    # 🟡/🔴 rows are the emulator saying what it does NOT do; demanding evidence
    # for them would be demanding proof of an absence. A green row rides along
    # because a map with NO green rows trips the parsed-nothing guard instead,
    # which is a different verdict (tested below).
    repo([("Widgets", "🟡 Emulated"), ("Gadgets", "🟢 Real")],
         {"gadgets": {"section": "Platform / fundamentals", "claim": "Gadgets",
                      "witnesses": ["go:TestReal"]}})
    assert run() == 0


# --- the four refusals --------------------------------------------------------

def test_a_green_claim_missing_from_the_manifest_fails(repo, capsys):
    repo([("Widgets", "🟢 Real")], {})
    assert run() == 1
    out = capsys.readouterr().out
    assert "Claims with no manifest entry" in out
    assert "needs an identified, existing witness" in out


def test_a_witness_naming_a_test_that_does_not_exist_fails(repo, capsys):
    # The whole promise: a claim names evidence that EXISTS.
    repo([("Widgets", "🟢 Real")],
         {"widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["go:TestVanished"]}})
    assert run() == 1
    out = capsys.readouterr().out
    assert "Dangling witness references" in out and "no such Go test" in out


def test_a_witness_naming_a_ci_job_that_does_not_exist_fails(repo, capsys):
    repo([("Widgets", "🟢 Real")],
         {"widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["ci:no-such-job"]}})
    assert run() == 1
    assert "no such CI job" in capsys.readouterr().out


def test_a_python_witness_that_does_not_exist_fails(repo, capsys):
    repo([("Widgets", "🟢 Real")],
         {"widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["py:test_vanished"]}})
    assert run() == 1
    assert "no such Python test" in capsys.readouterr().out


def test_a_todo_witness_fails_under_strict_but_not_otherwise(repo, capsys):
    repo([("Widgets", "🟢 Real")],
         {"widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["TODO"]}})
    assert run(strict=True) == 1
    assert run(strict=False) == 0, "without --strict this is a report, not a gate"


def test_an_undeclared_gated_witness_fails_and_prints_the_line_to_add(repo, capsys):
    # A gated witness is legitimate; an UNDECLARED one silently downgrades the
    # evidence behind a green row.
    repo([("Widgets", "🟢 Real")],
         {"widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["go:TestGated", "go:TestReal"]}})
    assert run() == 1
    out = capsys.readouterr().out
    assert "must be declared in the manifest's" in out
    assert '"go:TestGated": "<reason>"' in out, "the message must be copy-pasteable"


def test_a_declared_gate_is_accepted(repo):
    repo([("Widgets", "🟢 Real")],
         {"_gated": {"go:TestGated": "needs a real SQL Server"},
          "widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["go:TestGated", "go:TestReal"]}})
    assert run() == 0


def test_a_stale_gate_declaration_fails(repo, capsys):
    # A declaration for a witness that no longer skips is how the map drifts
    # back out of step — the dual of the rule above.
    #
    # This ALSO pins `kind != "go"` in the accept-a-declared-gate branch: without
    # it, a declared Go witness that stopped skipping is accepted as gated and
    # never reaches `stale`, silently retiring this rule.
    repo([("Widgets", "🟢 Real")],
         {"_gated": {"go:TestReal": "it used to skip"},
          "widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["go:TestReal"]}})
    assert run() == 1
    out = capsys.readouterr().out
    assert "Stale gate declarations" in out
    assert "remove the stale" in out


def test_a_claim_whose_every_witness_can_skip_fails(repo, capsys):
    # A green row that a default `go test ./...` proves nothing about.
    repo([("Widgets", "🟢 Real")],
         {"_gated": {"go:TestGated": "needs a real SQL Server"},
          "widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["go:TestGated"]}})
    assert run() == 1
    out = capsys.readouterr().out
    assert "Claims whose every witness can skip" in out
    assert "at least one witness that runs unconditionally" in out


def test_a_boundary_alone_does_not_count_as_running_evidence(repo, capsys):
    # A documented boundary scopes a claim; it is not a test that runs.
    repo([("Widgets", "🟢 Real")],
         {"_gated": {"go:TestGated": "needs a real SQL Server"},
          "widgets": {"section": "Platform / fundamentals", "claim": "Widgets",
                      "witnesses": ["go:TestGated", "boundary:documented limit"]}})
    assert run() == 1
    assert "at least one witness that runs unconditionally" in capsys.readouterr().out


# --- the self-check -----------------------------------------------------------

def test_parsing_zero_claims_is_a_broken_checker_not_a_clean_repo(repo, capsys):
    # This ran green on Windows for its whole life while matching zero claims:
    # a locale-dependent read turned the glyphs to mojibake and every rule was
    # vacuously satisfied.
    repo([("Widgets", "🟡 Emulated")], {})
    monkey = w.PARITY
    monkey.write_text("# no rows at all\n", encoding="utf-8")
    assert run() == 1
    assert "parsed 0 supported claims" in capsys.readouterr().out


def test_a_witness_carrying_many_claims_is_reported(repo, capsys):
    # Bundling is where one witness quietly covers claims it does not assert.
    rows = [(f"Widget {i}", "🟢 Real") for i in range(5)]
    manifest = {w.key_for(f"Widget {i}"): {"section": "Platform / fundamentals",
                                           "claim": f"Widget {i}",
                                           "witnesses": ["go:TestReal"]}
                for i in range(5)}
    repo(rows, manifest)
    assert run() == 0
    assert "carrying many claims" in capsys.readouterr().out
