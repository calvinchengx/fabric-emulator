"""Tests for the Python/npm advisory scan.

Same awkward shape as test_check_dependency_risk.py next door: the checker
passes against this repository today, and a checker that passes tells you
nothing about whether it works. So every test below drives it with a state it
must catch, and asserts that the message NAMES the offending lockfile,
advisory or hold. A checker failing for the wrong reason reads exactly like a
checker working.

OFFLINE, with no osv-scanner binary and no network. `scan` is the only function
that shells out, so it is stubbed with canned osv-scanner JSON -- including one
test that feeds it the REAL report shape to pin the two fields the rest of the
suite trusts: that `results[].packages[].package` carries name and version, and
that a package with no vulnerabilities still appears under `--all-packages`.
That second fact is the one this file exists to protect, because the first
version of the scan omitted the flag and counted only AFFECTED packages -- so
nine clean lockfiles reported zero packages each, indistinguishable from nine
lockfiles the parser had silently ignored.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_advisories as c  # noqa: E402

HOLD = {
    "id": "GHSA-rj75-hqrm-r3gf",
    "ecosystem": "npm",
    "package": "postcss-selector-parser",
    "lockfile": "pnpm-lock.yaml",
    "why": ["Build-time only, over the package's own stylesheet."],
    "exit": "LIFT THIS when @expressive-code/core admits postcss-nested 8.x.",
}


def finding(lockfile="pnpm-lock.yaml", package="postcss-selector-parser",
            ident="GHSA-rj75-hqrm-r3gf", aliases=(), version="6.1.4"):
    return {"lockfile": lockfile, "package": package, "version": version,
            "id": ident, "aliases": set(aliases), "summary": "CPU exhaustion"}


@pytest.fixture
def harness(tmp_path, monkeypatch):
    """Point the checker at a fake tree, a fake ledger and a stubbed scanner."""
    def build(lockfiles=("pnpm-lock.yaml",), holds=None, results=None,
              packages=736, scanner="/usr/local/bin/osv-scanner"):
        ledger = tmp_path / "docs" / "advisory-holds.json"
        if holds is not None:
            ledger.parent.mkdir(parents=True, exist_ok=True)
            ledger.write_text(json.dumps({"holds": holds}), encoding="utf-8")
        monkeypatch.setattr(c, "ROOT", tmp_path)
        monkeypatch.setattr(c, "LEDGER", ledger)
        monkeypatch.setattr(c, "tracked", lambda patterns, root: list(lockfiles))
        monkeypatch.setattr(c.shutil, "which", lambda name: scanner)

        def fake_scan(lockfile, root):
            if results is None:
                return packages, []
            found = results.get(lockfile, [])
            return packages, list(found)
        monkeypatch.setattr(c, "scan", fake_scan)
        return tmp_path
    return build


# --- the control -------------------------------------------------------------

def test_a_clean_tree_passes_and_prints_the_tally(harness, capsys):
    """A pass must be distinguishable from a no-op. A scanner that found no
    inputs reports exactly the same green as one that scanned everything, so
    the success line names the lockfiles and the package counts."""
    harness(lockfiles=("uv.lock", "examples/alpha/uv.lock"), packages=270)
    assert c.main(["--ecosystem", "uv"]) == 0, capsys.readouterr().err
    out = capsys.readouterr().out
    assert "540 locked packages across 2 tracked lockfiles" in out
    assert "examples/alpha/uv.lock (270)" in out
    assert "git ls-files" in out, "the success line states the design"
    assert "NOT reachability-filtered" in out, "must not overclaim parity"


# --- a finding nothing accounts for fails ------------------------------------

def test_an_unheld_finding_fails(harness, capsys):
    """The whole point of the job. cryptography 49.0.0 was this case: a CVSS
    8.2 whose fixed version was the one dependabot.yml was holding it below,
    with nothing asking whether that had happened."""
    harness(results={"pnpm-lock.yaml": [finding()]}, holds=[])
    assert c.main(["--ecosystem", "npm"]) == 1
    err = capsys.readouterr().err
    assert "postcss-selector-parser 6.1.4" in err
    assert "GHSA-rj75-hqrm-r3gf" in err
    assert "LIFT THIS" in err, "must say how to record it if it cannot be fixed"


def test_a_held_finding_passes(harness, capsys):
    harness(results={"pnpm-lock.yaml": [finding()]}, holds=[HOLD])
    assert c.main(["--ecosystem", "npm"]) == 0, capsys.readouterr().err
    assert "1 advisory finding(s) accepted" in capsys.readouterr().out


def test_a_hold_matches_on_an_alias(harness, capsys):
    """OSV reports one advisory under PYSEC, GHSA and CVE names
    interchangeably. Matching the primary id alone would let a hold stop
    working when OSV reshuffles which name is primary -- silently, and in the
    direction that turns an accepted finding into a red build."""
    harness(results={"pnpm-lock.yaml": [
                finding(ident="PYSEC-2026-9999",
                        aliases=("GHSA-rj75-hqrm-r3gf",))]},
            holds=[HOLD])
    assert c.main(["--ecosystem", "npm"]) == 0, capsys.readouterr().err


def test_a_hold_does_not_excuse_another_lockfile(harness, capsys):
    """The reasoning in a hold is about where the package SITS. 'Build-time
    only, in the website build' says nothing about the same advisory arriving
    in a lockfile that ships inside an image, so the match is scoped to the
    lockfile the argument was made about."""
    harness(lockfiles=("uv.lock",),
            results={"uv.lock": [finding(lockfile="uv.lock")]},
            holds=[HOLD])
    assert c.main(["--ecosystem", "uv"]) == 1
    assert "uv.lock" in capsys.readouterr().err


# --- the ledger is checked in both directions --------------------------------

def test_a_hold_matching_nothing_fails(harness, capsys):
    """A closed hold left recorded is not harmless: it goes on excusing the
    finding and would silently re-cover it if the dependency rolled back --
    the direction a 'what's new' reader would never think to check. Same rule
    as docs/security-footguns.json and docs/script-test-coverage.json."""
    harness(results={"pnpm-lock.yaml": []}, holds=[HOLD])
    assert c.main(["--ecosystem", "npm"]) == 1
    err = capsys.readouterr().err
    assert "GHSA-rj75-hqrm-r3gf" in err
    assert "matches NOTHING" in err


def test_a_hold_for_an_unscanned_lockfile_is_not_reported_stale(harness, capsys):
    """Scoped to the lockfiles THIS run scanned, so the uv job cannot fail
    over an npm hold it was never in a position to see. Without this the two
    jobs would each fail on the other's holds, and the only way to make both
    green would be to delete the ledger."""
    harness(lockfiles=("uv.lock",), results={"uv.lock": []}, holds=[HOLD])
    assert c.main(["--ecosystem", "uv"]) == 0, capsys.readouterr().err


def test_a_hold_with_no_exit_condition_fails(harness, capsys):
    """Validated in the gating job as well as in make check, so a hold cannot
    suppress a finding on the strength of a field nobody checked."""
    harness(results={"pnpm-lock.yaml": [finding()]},
            holds=[{**HOLD, "exit": "waiting for upstream"}])
    assert c.main(["--ecosystem", "npm"]) == 1
    err = capsys.readouterr().err
    assert "LIFT THIS" in err and "PERMANENT HOLD" in err


def test_a_hold_missing_a_required_field_fails(harness, capsys):
    harness(results={"pnpm-lock.yaml": [finding()]},
            holds=[{k: v for k, v in HOLD.items() if k != "why"}])
    assert c.main(["--ecosystem", "npm"]) == 1
    assert "'why'" in capsys.readouterr().err


def test_an_absent_ledger_means_nothing_is_held(harness, capsys):
    harness(holds=None)
    assert c.main(["--ecosystem", "npm"]) == 0, capsys.readouterr().err


def test_a_broken_ledger_fails_rather_than_reading_as_empty(harness, capsys):
    tree = harness(holds=[HOLD])
    (tree / "docs" / "advisory-holds.json").write_text("{nope", encoding="utf-8")
    assert c.main(["--ecosystem", "npm"]) == 1
    assert "cannot read" in capsys.readouterr().err


# --- refusing to pass vacuously ----------------------------------------------

def test_discovering_no_lockfiles_fails(harness, capsys):
    """The failure mode this job is most exposed to. Patterns that match
    nothing scan nothing and report green, which is the same green a fully
    covered tree produces -- and is what seven unwatched example lockfiles
    looked like from the outside."""
    harness(lockfiles=())
    assert c.main(["--ecosystem", "uv"]) == 1
    err = capsys.readouterr().err
    assert "inspected nothing" in err
    assert "uv.lock" in err, "must name the patterns that matched nothing"


def test_a_lockfile_scanning_to_zero_packages_fails(harness, capsys):
    """The bug this guard actually caught. The first version of `scan`
    omitted `--all-packages`, so osv-scanner's JSON listed only AFFECTED
    packages and all nine clean uv.lock files came back as zero -- identical
    to a lockfile the parser had silently ignored."""
    harness(lockfiles=("uv.lock",), packages=0)
    assert c.main(["--ecosystem", "uv"]) == 1
    err = capsys.readouterr().err
    assert "uv.lock" in err
    assert "ZERO packages" in err


def test_a_missing_scanner_binary_fails_rather_than_skipping(harness, capsys):
    """check_dismissed_advisories.py SKIPS when `gh` is absent because a
    laptop legitimately has no GitHub CLI. This runs only in CI, where a
    missing binary means the install step broke -- and a skip there would be a
    green check over nothing scanned."""
    harness(scanner=None)
    assert c.main(["--ecosystem", "npm"]) == 1
    err = capsys.readouterr().err
    assert "not on PATH" in err
    assert "nothing was scanned" in err


def test_one_bad_lockfile_does_not_mask_the_others(harness, capsys, monkeypatch):
    """Accumulated, never short-circuited. A single unparseable lockfile must
    not hide the findings in the other eight."""
    def fake_scan(lockfile, root):
        if lockfile == "uv.lock":
            raise RuntimeError("osv-scanner produced no JSON for uv.lock")
        return 10, [finding(lockfile=lockfile, package="jupyter-server")]
    monkeypatch.setattr(c, "scan", fake_scan)
    harness(lockfiles=("uv.lock", "examples/alpha/uv.lock"), holds=[])
    monkeypatch.setattr(c, "scan", fake_scan)
    assert c.main(["--ecosystem", "uv"]) == 1
    err = capsys.readouterr().err
    assert "no JSON for uv.lock" in err, "the broken lockfile is reported"
    assert "jupyter-server" in err, "and so is the OTHER lockfile's finding"


# --- the scanner contract ----------------------------------------------------

def test_scan_reads_the_real_osv_scanner_report_shape(tmp_path, monkeypatch):
    """Pins the two facts the rest of this file trusts, against a payload in
    osv-scanner's actual `--format json --all-packages` shape: a clean package
    is still counted, and an affected one yields name, version, id and
    aliases. A stub agreeing with a wrong understanding of the format would
    make every test above pass while the real scan reported nothing."""
    report = {"results": [{"source": {"path": "uv.lock"}, "packages": [
        {"package": {"name": "pytest", "version": "8.4.2", "ecosystem": "PyPI"}},
        {"package": {"name": "cryptography", "version": "49.0.0",
                     "ecosystem": "PyPI"},
         "vulnerabilities": [{"id": "PYSEC-2026-3552",
                              "aliases": ["GHSA-g6cj-pr64-35w5"],
                              "summary": "Vulnerable to a Bleichenbacher attack"}]},
    ]}]}

    class Proc:
        stdout = json.dumps(report)
        stderr = ""
        returncode = 1  # osv-scanner exits 1 WHEN IT FINDS SOMETHING.

    monkeypatch.setattr(c.subprocess, "run", lambda *a, **k: Proc())
    packages, findings = c.scan("uv.lock", tmp_path)
    assert packages == 2, "a clean package must still count toward the tally"
    assert len(findings) == 1
    assert findings[0]["package"] == "cryptography"
    assert findings[0]["version"] == "49.0.0"
    assert findings[0]["aliases"] == {"GHSA-g6cj-pr64-35w5"}
    # The flag whose absence was the original bug.
    captured = {}

    def record(cmd, **kwargs):
        captured["cmd"] = cmd
        return Proc()

    monkeypatch.setattr(c.subprocess, "run", record)
    c.scan("uv.lock", tmp_path)
    assert "--all-packages" in captured["cmd"]


def test_non_json_output_is_an_error_not_an_empty_scan(tmp_path, monkeypatch):
    """A scanner that died printing a usage message must not read as a clean
    lockfile."""
    class Proc:
        stdout = "usage: osv-scanner ..."
        stderr = "unknown flag"
        returncode = 127

    monkeypatch.setattr(c.subprocess, "run", lambda *a, **k: Proc())
    with pytest.raises(RuntimeError, match="no JSON"):
        c.scan("uv.lock", tmp_path)


# --- the checker holds itself and the workflow to account --------------------

def test_the_pinned_version_matches_the_composite_action():
    """The pin lives in two files -- SCANNER_VERSION here and OSV_VERSION in
    .github/actions/osv-scanner/action.yml -- and the failure mode of letting
    them drift is that the two ecosystems get scanned by different parsers
    while both report green. The action's own comment promises this test."""
    action = (pathlib.Path(__file__).resolve().parents[2]
              / ".github" / "actions" / "osv-scanner" / "action.yml")
    text = action.read_text(encoding="utf-8")
    assert f"OSV_VERSION: {c.SCANNER_VERSION}" in text, (
        f"action.yml must pin osv-scanner {c.SCANNER_VERSION}")


def test_the_real_ledger_parses_and_every_hold_declares_an_exit():
    """Guards against vacuity the way its neighbour does: a ledger read that
    silently returned nothing would make the hold tests above assert about an
    empty list."""
    holds = c.read_ledger(c.LEDGER)
    assert holds, "this repository ships two real holds"
    assert not c.hold_problems(holds)
    assert {h["lockfile"] for h in holds} <= {"pnpm-lock.yaml"}


def test_every_ecosystem_pattern_matches_something_in_this_repository():
    """The patterns are the whole input contract. One that matches nothing
    would make a real job pass having scanned nothing, and this is the only
    test here that touches the actual tree."""
    root = pathlib.Path(__file__).resolve().parents[2]
    for ecosystem, patterns in c.ECOSYSTEMS.items():
        found = c.tracked(patterns, root)
        assert found, f"{ecosystem}: {patterns} matched no tracked file"
    assert len(c.tracked(c.ECOSYSTEMS["uv"], root)) == 9, (
        "nine tracked uv.lock files: the root, e2e/fabric-cli and seven examples")
