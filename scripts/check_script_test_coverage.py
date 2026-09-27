#!/usr/bin/env python3
"""Every script in `scripts/` has a dedicated test module, or is recorded with a reason.

WHAT THE SCRIPTS IN THIS DIRECTORY ARE. They are this repository's invariant
enforcement. `make check` runs thirty of them and the `witnesses` job runs
twenty-nine; between them they assert that every parity claim names a witness,
that no route the emulator serves is undocumented, that the sidebar is complete,
that no Go comment teaches a retired justification, and that no test sleeps
without a bound. The tree's guarantees are carried by this directory.

THE FAILURE SHAPE, which this repository has already written down twice. From
python/tests/test_make_check_runs_in_ci.py: "a check that passes is
indistinguishable from a check that is running." A guard script whose own
behaviour nothing asserts has the same property one level in -- its detection can
stop matching, for a renamed directory or a tightened regex or a refactor that
drops a branch, and it will go on printing its success line and exiting 0 forever.
docs/10-testing.md's item Eight is the same thing: silence where the signal would
be, with nothing to distinguish "no violation here" from "this stopped looking".

That is not hypothetical in this tree. check_doc_drift.py's own tests record that
the naive form of its path class returned 84 findings, every one a false positive;
check_python_test_flakiness.py's `ledger_key` docstring records a kind missing
from the match key, which exempted 42 of 44 recorded symbols from the two bans
the ledger exists to enforce. BOTH were found by someone DRIVING the checker
against a violation it had to catch -- that is, by a test -- and neither would
have been found by reading it.

THE MEASURED BASELINE when this landed, and the reason this file is a checker
rather than three hand-written test modules:

    scripts/govern_ingest.py                      717 lines
    scripts/check_cron_workflow_freshness.py      289
    scripts/build_fixture_wheels.py               220
    scripts/capture_definition_shape.py           187
    scripts/vendor_notebookutils_stubs.py         150
    scripts/spark_check.py                        145
    scripts/check_cask_stanzas.py                 105
    scripts/check_image_digests.py                103
    scripts/vendor_adf_pipeline_schema.py          90
    scripts/probe_sail_merge_premise.py            72
    scripts/image_tags.py                          58

Eleven of 51 scripts with no `python/tests/test_<stem>.py`. The Go surface was
measured at the same time for comparison and has no structural gap worth
reporting: all 27 packages under internal/, pkg/ and cmd/ carry a `*_test.go`,
across 209 non-test source files. So the gap is here, and it is here alone.

Three of the eleven were closed with real unit tests in the same change --
image_tags.py, check_image_digests.py and check_cask_stanzas.py, chosen because
they are pure logic over a tmp_path fixture with no network and no Docker. The
rest are recorded in docs/script-test-coverage.json with the reason, so the
ledger ships SMALLER than the gap it records and the next reader can see which
direction it is moving.

WHY A CHECKER AND NOT JUST THE THREE TESTS. Fixing three instances leaves the
shape intact: the twelfth script lands with no test and nothing says so, exactly
as the twelfth entry in `make check` would have gone unrun by CI before
test_make_check_runs_in_ci.py asserted the list instead of the instances. The
argument is that file's, applied one directory over.

TWO FINDING KINDS, and the second is the one that does the long-term work:

  UNTESTED  a script with neither a dedicated test module nor a ledger entry.
            This is the kind the ledger exists to keep SHRINKING.

  STALE     a ledger entry naming a script that no longer exists, or one that
            has SINCE GAINED a dedicated test. A closed gap must not linger as
            an accepted one: an allowance nobody revisits is how a ledger stops
            describing the tree and starts excusing it, and the entry would
            silently re-cover the file if its test were later deleted.

Both directions, like docs/test-flakiness.json and docs/python-test-flakiness.json
before it. A one-directional ledger only ever grows.

WHAT A DEDICATED TEST MODULE MEANS, AND WHAT IT DELIBERATELY DOES NOT. The rule
is that `python/tests/test_<stem>.py` exists -- a NAME, not a coverage
measurement. A name is what makes the finding unambiguous and the fix obvious,
and coverage is already measured and gated elsewhere (92% `fail_under`,
pyproject.toml). What this adds is the one thing a percentage cannot say: that
this particular file has somewhere for its violations to be driven from. A module
that exists and asserts nothing useful would satisfy this checker, which is why
the ledger entries carry prose rather than a flag -- the reviewable claim is the
reason, not the tick.

The name is also why indirect coverage does not count. Three of the recorded
scripts ARE exercised, but under another file's name:
check_cron_workflow_freshness.py by test_cron_workflow_freshness.py (no `check_`
prefix), vendor_notebookutils_stubs.py through
test_check_notebookutils_surface.py, and govern_ingest.py incidentally by
test_check_govern_types.py and test_govern_column_schema.py. Each is recorded
with where its behaviour actually is exercised, so the ledger says what is true
instead of pretending the coverage is absent or pretending it is dedicated.

Usage:
    check_script_test_coverage.py            report findings, exit 0
    check_script_test_coverage.py --strict   exit non-zero on unrecorded findings
"""
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPTS = ROOT / "scripts"
TESTS = ROOT / "python" / "tests"
LEDGER = ROOT / "docs" / "script-test-coverage.json"


def relkey(path, root=None):
    """A path as the ledger spells it: relative to the root, forward slashes.

    The separator is not cosmetic, and the reasoning is inherited rather than
    invented -- check_test_flakiness.py shipped `str(path.relative_to(ROOT))`
    and it yielded `scripts\\image_tags.py` on Windows against
    `scripts/image_tags.py` in the checked-in JSON. That mismatch does not make a
    both-directions checker miss things; it makes it report EVERYTHING twice
    over, every script as unrecorded and every entry as stale, on one platform
    only. The Windows pytest leg is what went red.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    root = ROOT if root is None else root
    r = root if isinstance(root, pathlib.PurePath) else pathlib.PurePath(root)
    return (p.relative_to(r) if p.is_absolute() else p).as_posix()


def script_files(scripts_dir=None):
    """Every Python script in `scripts/`, sorted.

    Non-recursive on purpose: `scripts/` is flat, and the only subdirectory it
    grows is `__pycache__`, whose contents are `.pyc` rather than `.py` and are
    not source anybody tests. A `**/*.py` glob here would also walk a nested
    checkout if one ever appeared, which is the trap SKIP_DIRS exists for in the
    two flakiness checkers.
    """
    d = SCRIPTS if scripts_dir is None else scripts_dir
    return sorted(p for p in d.glob("*.py") if p.is_file())


def expected_test(script, tests_dir=None):
    """The dedicated test module a script is required to have."""
    d = TESTS if tests_dir is None else tests_dir
    return d / f"test_{script.stem}.py"


def line_count(path):
    """Lines in a file, for the report and for the ledger's recorded figure."""
    try:
        return len(path.read_text(encoding="utf-8").splitlines())
    except (UnicodeDecodeError, OSError):
        return 0


def load_ledger(ledger=None):
    """The recorded exemptions, keyed by the script path they name.

    A MALFORMED LEDGER FAILS LOUDLY rather than reading as empty, and that
    direction is the whole reason this is a function and not a comprehension at
    the call site. `json.loads` on a truncated file raises by itself, but a file
    whose entries are missing `reason` would parse fine and suppress findings
    while explaining nothing -- an exemption with no reason is an omission
    wearing a decision's clothes (test_make_check_runs_in_ci.py says it in those
    words about its own LOCAL_ONLY map).
    """
    path = LEDGER if ledger is None else ledger
    if not path.exists():
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    entries = data.get("accepted", [])
    if not isinstance(entries, list):
        raise ValueError(
            f"{relkey(path)}: `accepted` must be a list of entries, got "
            f"{type(entries).__name__}")
    out = {}
    for entry in entries:
        if not isinstance(entry, dict):
            raise ValueError(
                f"{relkey(path)}: every entry under `accepted` must be an "
                f"object, got {entry!r}")
        missing = [k for k in ("script", "reason") if not entry.get(k)]
        if missing:
            raise KeyError(
                f"a script-test-coverage entry is missing "
                f"{', '.join(missing)}: {entry!r}. Every entry needs the script "
                "it exempts and the reason it is exempt -- an entry with no "
                "reason suppresses a finding while explaining nothing, which is "
                "an omission wearing a decision's clothes.")
        if entry["script"] in out:
            raise ValueError(
                f"{relkey(path)}: {entry['script']} is recorded twice; two "
                "reasons for one script means one of them is not being read")
        out[entry["script"]] = entry
    return out


def scan(scripts_dir=None, tests_dir=None, accepted=None):
    """Both finding kinds: (untested, stale).

    `untested` are scripts with no dedicated test module and no ledger entry.
    `stale` are ledger entries whose script has gone away or has since gained
    one. Returned as a pair rather than one list because they are acted on
    differently: an UNTESTED finding is closed by writing a test or recording a
    reason, and a STALE one is closed by DELETING a line.
    """
    accepted = load_ledger() if accepted is None else accepted
    scripts = script_files(scripts_dir)
    root = ROOT if scripts_dir is None else scripts_dir.parent

    untested, covered = [], set()
    for script in scripts:
        key = relkey(script, root)
        if expected_test(script, tests_dir).exists():
            covered.add(key)
            continue
        if key in accepted:
            continue
        untested.append({
            "script": key,
            "lines": line_count(script),
            "expected": relkey(expected_test(script, tests_dir), root),
        })

    live = {relkey(s, root): s for s in scripts}
    stale = []
    for key, entry in sorted(accepted.items()):
        if key not in live:
            stale.append({"script": key, "why": "the script no longer exists",
                          "reason": entry["reason"]})
        elif key in covered:
            got = relkey(expected_test(live[key], tests_dir), root)
            stale.append({"script": key, "why": f"it now has {got}",
                          "reason": entry["reason"]})
    return untested, stale


def main(argv):
    strict = "--strict" in argv[1:]

    scripts = script_files()
    # A sweep that walked nothing reports success. The same guard both flakiness
    # checkers carry, for the same reason: a check that inspects nothing passes
    # for the wrong reason, and it passes quietly. If `scripts/` were ever moved
    # or renamed, every finding would vanish and this would print a green line.
    if not scripts:
        print("check_script_test_coverage: walked zero scripts under "
              f"{relkey(SCRIPTS)} - a check that inspects nothing passes "
              "vacuously. Either the directory moved, or the glob stopped "
              "matching.")
        return 1
    # ...and the inverse vacuity, which fails the other way: with no test
    # directory every script reads as untested for a reason that is not its own,
    # and `--strict` would report fifty findings naming the wrong problem.
    if not TESTS.is_dir():
        print(f"check_script_test_coverage: {relkey(TESTS)} does not exist, so "
              "every script would read as untested for a reason that is not its "
              "own. Either the suite moved -- update TESTS -- or it was removed.")
        return 1

    accepted = load_ledger()
    untested, stale = scan(accepted=accepted)
    tested = sum(1 for s in scripts if expected_test(s).exists())

    if not strict:
        for script in scripts:
            key = relkey(script)
            if expected_test(script).exists():
                continue
            mark = "accepted" if key in accepted else "UNTESTED"
            why = accepted[key]["reason"] if key in accepted else ""
            print(f"{mark:9} {key} ({line_count(script)} lines) {why}")
        # The stale direction too, because report mode is what someone reads
        # while editing and half a contract is not a contract. A stale allowance
        # is the direction a "flag what is new" reader would never think to ask
        # about, and it is how a closed gap goes on being excused.
        for s in stale:
            print(f"{'STALE':9} {s['script']} - recorded, but {s['why']}")
        print(f"\ncheck_script_test_coverage: {len(scripts)} script(s), "
              f"{tested} with a dedicated test module, "
              f"{len(accepted) - len(stale)} recorded, "
              f"{len(untested)} neither, "
              f"{len(stale)} ledger entr(y/ies) stale")
        return 0

    problems = []
    if untested:
        listing = "\n    ".join(
            f"{f['script']} ({f['lines']} lines) - expected {f['expected']}"
            for f in untested)
        problems.append(
            f"{len(untested)} script(s) have no dedicated test module and are not "
            f"recorded in {relkey(LEDGER)}:\n    {listing}\n"
            "  These scripts ARE this repository's invariant enforcement, and a "
            "guard whose own behaviour nothing asserts can stop guarding and go "
            "on reporting green -- a check that passes is indistinguishable from "
            "a check that is running. Write the test module, or record the "
            "script with the reason it does not have one.")
    if stale:
        problems.append(
            f"{len(stale)} ledger entr(y/ies) record a gap that is closed; a "
            "closed gap left recorded goes on excusing the file, and would "
            "silently re-cover it if its test were deleted:\n    "
            + "\n    ".join(f"{s['script']} - {s['why']}" for s in stale))

    if problems:
        print("check_script_test_coverage: " + "\n\n".join(problems))
        return 1

    print(f"check_script_test_coverage: {len(scripts)} script(s); {tested} carry "
          f"a dedicated test module and the remaining {len(accepted)} are "
          f"recorded in {relkey(LEDGER)} with a reason")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
