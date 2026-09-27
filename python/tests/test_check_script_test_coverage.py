"""Tests for the script-test-coverage checker.

THE AWKWARD POSITION every checker's test file in this repository shares, and
this one most sharply of all: the invariant is HELD as of the commit that added
it -- three of the eleven measured gaps were closed with real tests and the other
eight are recorded -- so running it against this repository proves only that it
did not crash. A checker that has never rejected anything is indistinguishable
from one whose detection stopped matching.

Sharper here because of what this checker IS. It exists to say that a guard
script with no test can stop guarding and go on reporting green. A version of
that argument that applied to every script but itself would be the joke it sounds
like, so every case below drives it over a synthetic `scripts/` and
`python/tests/` under tmp_path with a violation it MUST catch or a near-miss it
must NOT, and the real repository appears exactly once at the end as the control.

WHY SYNTHETIC TREES AND NOT THE REAL ONE. A test asserting "this repository has
eight recorded scripts" fails on the next script added, the next test written and
the next exemption closed -- three things that SHOULD happen. A test that must be
edited whenever the tree legitimately improves is a test people re-baseline
without reading, which is the mechanism check_comment_drift.py's own design notes
give for keying its pins on counts rather than line numbers.

THE STALE DIRECTION GETS THE MOST CASES HERE, deliberately. Unrecorded findings
are loud by construction -- somebody adds a script and the build goes red the
same day. A stale entry is silent: the gap is closed, the build is green, and the
allowance sits there re-covering the file if its test is ever deleted. That is
the half a "flag what is new" reader would never think to ask about, and the half
docs/python-test-flakiness.json says in its own header that a ledger without it
only ever grows.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_script_test_coverage as c  # noqa: E402

REPO = pathlib.Path(__file__).resolve().parents[2]


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """A synthetic repository: scripts/, python/tests/ and a ledger.

    Returns a builder taking the scripts to create, the test modules to create,
    and the ledger entries to record. Every global the checker reads is
    redirected, so nothing here can reach the real tree -- which matters more
    than usual: this checker walks `scripts/`, and a test that leaked would
    assert against the very files it is shipping beside.
    """
    def build(scripts=(), tests=(), accepted=None, ledger_text=None):
        root = tmp_path / "repo"
        (root / "scripts").mkdir(parents=True, exist_ok=True)
        (root / "python" / "tests").mkdir(parents=True, exist_ok=True)
        for name in scripts:
            (root / "scripts" / name).write_text(
                f'"""{name}"""\n\n\ndef main():\n    return 0\n', encoding="utf-8")
        for name in tests:
            (root / "python" / "tests" / name).write_text(
                "def test_x():\n    assert True\n", encoding="utf-8")
        ledger = root / "docs" / "script-test-coverage.json"
        ledger.parent.mkdir(parents=True, exist_ok=True)
        if ledger_text is not None:
            ledger.write_text(ledger_text, encoding="utf-8")
        elif accepted is not None:
            ledger.write_text(json.dumps({"accepted": list(accepted)}, indent=2),
                              encoding="utf-8")
        monkeypatch.setattr(c, "ROOT", root)
        monkeypatch.setattr(c, "SCRIPTS", root / "scripts")
        monkeypatch.setattr(c, "TESTS", root / "python" / "tests")
        monkeypatch.setattr(c, "LEDGER", ledger)
        return root
    return build


# --- a script WITH a test module is silent ------------------------------------


def test_a_script_with_a_matching_test_module_is_not_reported(tree):
    tree(scripts=["check_thing.py"], tests=["test_check_thing.py"])
    untested, stale = c.scan()
    assert untested == [] and stale == []
    assert c.main(["check", "--strict"]) == 0


def test_a_test_module_with_a_near_miss_name_does_not_count(tree):
    """The rule is `test_<stem>.py` exactly, and this is the case the real
    ledger's largest naming entry is about: `check_cron_workflow_freshness.py` IS
    driven by `test_cron_workflow_freshness.py`, which drops the `check_` prefix.
    A checker that matched loosely would have silently accepted that and said
    nothing, so nobody would ever learn the name was wrong -- and loose matching
    is how `test_delta_ops_merge.py` would come to satisfy `delta_ops.py`, which
    is a different claim about a different file."""
    tree(scripts=["check_thing.py"], tests=["test_thing.py", "test_check_thing_more.py"])
    untested, _ = c.scan()
    assert [f["script"] for f in untested] == ["scripts/check_thing.py"]
    assert c.main(["check", "--strict"]) == 1


# --- an UNRECORDED untested script fails --------------------------------------


def test_an_unrecorded_untested_script_is_reported_and_strict_fails(tree, capsys):
    tree(scripts=["check_thing.py", "lonely.py"], tests=["test_check_thing.py"],
         accepted=[])
    untested, stale = c.scan()
    assert [f["script"] for f in untested] == ["scripts/lonely.py"]
    assert stale == []
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "scripts/lonely.py" in out
    assert "python/tests/test_lonely.py" in out, \
        "the finding must name the module to write, or the reader has to derive it"


def test_the_report_names_the_line_count(tree, capsys):
    """The size is what tells a reader which gap to close next -- 717 lines and 58
    lines are the same finding and not remotely the same job."""
    tree(scripts=["lonely.py"], accepted=[])
    untested, _ = c.scan()
    assert untested[0]["lines"] > 0
    assert c.main(["check"]) == 0
    assert "lines" in capsys.readouterr().out


def test_report_mode_exits_zero_even_with_findings(tree):
    """Report mode is what someone runs while editing; only `--strict` is the
    gate. Conflating them would mean the informational command fails, and a
    command that always fails stops being run."""
    tree(scripts=["lonely.py"], accepted=[])
    assert c.main(["check"]) == 0
    assert c.main(["check", "--strict"]) == 1


# --- a RECORDED script is suppressed ------------------------------------------


def test_a_ledger_entry_with_a_reason_suppresses_the_finding(tree):
    tree(scripts=["lonely.py"],
         accepted=[{"script": "scripts/lonely.py", "reason": "an investigation "
                    "probe in no target and no workflow; it enforces nothing"}])
    untested, stale = c.scan()
    assert untested == [] and stale == []
    assert c.main(["check", "--strict"]) == 0


def test_an_entry_with_an_empty_reason_is_refused_by_name(tree):
    """An exemption with no reason is an omission wearing a decision's clothes --
    test_make_check_runs_in_ci.py's words about its own LOCAL_ONLY map. It must
    fail LOUDLY rather than silently suppressing, because a suppression that
    explains nothing is indistinguishable from a script somebody simply forgot."""
    tree(scripts=["lonely.py"],
         accepted=[{"script": "scripts/lonely.py", "reason": ""}])
    with pytest.raises(KeyError) as e:
        c.load_ledger()
    assert "reason" in str(e.value)


def test_an_entry_with_no_script_key_is_refused(tree):
    tree(scripts=["lonely.py"], accepted=[{"reason": "because"}])
    with pytest.raises(KeyError):
        c.load_ledger()


# --- the STALE direction ------------------------------------------------------


def test_an_entry_for_a_deleted_script_is_stale(tree, capsys):
    """The script is gone, so the allowance describes nothing. Left in place it
    is a line nobody can evaluate, and the next reader cannot tell a deliberate
    exemption from a fossil."""
    tree(scripts=["check_thing.py"], tests=["test_check_thing.py"],
         accepted=[{"script": "scripts/gone.py", "reason": "a probe"}])
    untested, stale = c.scan()
    assert untested == []
    assert [s["script"] for s in stale] == ["scripts/gone.py"]
    assert "no longer exists" in stale[0]["why"]
    assert c.main(["check", "--strict"]) == 1
    assert "scripts/gone.py" in capsys.readouterr().out


def test_an_entry_for_a_script_that_has_gained_a_test_is_stale(tree, capsys):
    """THE CASE THIS CHECKER EXISTS FOR, and the one that is silent without it.
    The gap is CLOSED: the script now has its dedicated module, the build is
    green, and the entry excusing it sits there indefinitely. If that test were
    later deleted the entry would silently re-cover the file, so the ledger would
    absorb a real regression without a word. A one-directional ledger only ever
    grows."""
    tree(scripts=["lonely.py"], tests=["test_lonely.py"],
         accepted=[{"script": "scripts/lonely.py", "reason": "no test yet"}])
    untested, stale = c.scan()
    assert untested == []
    assert [s["script"] for s in stale] == ["scripts/lonely.py"]
    assert "test_lonely.py" in stale[0]["why"], \
        "the finding must name the test that closed the gap, so the fix is to delete this line"
    assert c.main(["check", "--strict"]) == 1


def test_report_mode_shows_stale_entries_too(tree, capsys):
    """Report mode was reporting only half the contract in the sibling checker,
    and this is the half a reader would never think to ask for."""
    tree(scripts=["lonely.py"], tests=["test_lonely.py"],
         accepted=[{"script": "scripts/lonely.py", "reason": "no test yet"}])
    assert c.main(["check"]) == 0
    out = capsys.readouterr().out
    assert "STALE" in out and "scripts/lonely.py" in out


def test_both_directions_can_fire_at_once(tree, capsys):
    """An unrecorded script AND a stale entry in one run. Reporting only the
    first would let a ledger cleanup hide behind a new finding, and vice versa."""
    tree(scripts=["lonely.py", "fresh.py"], tests=["test_lonely.py"],
         accepted=[{"script": "scripts/lonely.py", "reason": "no test yet"}])
    untested, stale = c.scan()
    assert [f["script"] for f in untested] == ["scripts/fresh.py"]
    assert [s["script"] for s in stale] == ["scripts/lonely.py"]
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "scripts/fresh.py" in out and "scripts/lonely.py" in out


# --- a MALFORMED ledger fails loudly -----------------------------------------


def test_unparseable_json_raises_rather_than_reading_as_empty(tree):
    """A truncated or hand-broken ledger must not read as "nothing is recorded".
    It would take every recorded script to UNTESTED at once, which is loud -- but
    the same mechanism with a ledger that parses to `{}` is silent, so the
    refusal is asserted rather than assumed."""
    tree(scripts=["lonely.py"], ledger_text='{"accepted": [ {"script": ')
    with pytest.raises(json.JSONDecodeError):
        c.load_ledger()


def test_accepted_must_be_a_list(tree):
    """`{"accepted": {...}}` is a plausible hand-edit and it iterates as its KEYS
    -- strings, not entries -- so a naive reader would produce a ledger of
    nonsense that suppresses nothing and reports nothing wrong."""
    tree(scripts=["lonely.py"], ledger_text='{"accepted": {"scripts/lonely.py": "why"}}')
    with pytest.raises(ValueError):
        c.load_ledger()


def test_a_script_recorded_twice_is_refused(tree):
    """Two reasons for one script means one of them is not being read, and the
    one that loses is decided by file order. Whichever a later reader edits, half
    the time nothing changes."""
    tree(scripts=["lonely.py"],
         accepted=[{"script": "scripts/lonely.py", "reason": "first"},
                   {"script": "scripts/lonely.py", "reason": "second"}])
    with pytest.raises(ValueError) as e:
        c.load_ledger()
    assert "twice" in str(e.value)


def test_a_missing_ledger_reads_as_empty_and_that_is_correct(tree):
    """The one absence that IS benign: no ledger means nothing is excused, so
    every untested script is reported. That fails loudly in the right direction,
    which is why it is not an error."""
    tree(scripts=["lonely.py"])
    assert c.load_ledger() == {}
    assert c.main(["check", "--strict"]) == 1


# --- the vacuity guards -------------------------------------------------------


def test_walking_zero_scripts_fails_rather_than_passing(tree, capsys):
    """A check that inspects nothing passes for the wrong reason, and it passes
    quietly. If `scripts/` were renamed, every finding would vanish and this
    would print a green line -- the exact shape this checker was written about."""
    tree(scripts=[], accepted=[])
    assert c.main(["check", "--strict"]) == 1
    assert "zero scripts" in capsys.readouterr().out


def test_a_missing_test_directory_fails_rather_than_flagging_everything(tree, capsys):
    """The inverse vacuity, which fails the other way: with no `python/tests/`
    every script reads as untested, and `--strict` would report fifty findings
    naming the wrong problem. A checker that is confidently wrong about fifty
    files is one people stop believing about the one that matters."""
    root = tree(scripts=["a.py", "b.py"], accepted=[])
    for p in (root / "python" / "tests").iterdir():
        p.unlink()
    (root / "python" / "tests").rmdir()
    assert c.main(["check", "--strict"]) == 1
    assert "does not exist" in capsys.readouterr().out


def test_only_python_files_are_required_to_have_tests(tree):
    """`scripts/` also holds shell (`status.sh`, `doctor.sh`, `docker-smoke.sh`).
    Demanding a pytest module for a shell script would be a finding nobody can
    close in the terms the message asks for."""
    root = tree(scripts=["check_thing.py"], tests=["test_check_thing.py"])
    (root / "scripts" / "status.sh").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    untested, stale = c.scan()
    assert untested == [] and stale == []


def test_a_pycache_directory_is_not_walked(tree):
    """`scripts/__pycache__` is the only subdirectory this directory grows, and a
    recursive glob would demand a test module for a `.pyc`'s stem."""
    root = tree(scripts=["check_thing.py"], tests=["test_check_thing.py"])
    cache = root / "scripts" / "__pycache__"
    cache.mkdir()
    (cache / "check_thing.cpython-312.pyc").write_bytes(b"\x00")
    (cache / "stray.py").write_text("x = 1\n", encoding="utf-8")
    untested, _ = c.scan()
    assert untested == [], f"walked into __pycache__: {untested}"


# --- path spelling ------------------------------------------------------------


def test_the_ledger_key_is_posix_on_every_platform():
    """The separator is not cosmetic, and the Go sibling paid for learning it:
    `str(path.relative_to(ROOT))` yields `scripts\\x.py` on Windows against
    `scripts/x.py` in the checked-in JSON. That does not make a both-directions
    checker miss things -- it makes it report EVERY script as unrecorded and
    EVERY entry as stale, on one platform only, which is the shape that survives
    review behind a green Linux run. Asserted against `PureWindowsPath` directly
    rather than left to the Windows pytest leg to discover."""
    win = pathlib.PureWindowsPath(r"C:\repo\scripts\image_tags.py")
    assert c.relkey(win, pathlib.PureWindowsPath(r"C:\repo")) == "scripts/image_tags.py"
    posix = pathlib.PurePosixPath("/repo/scripts/image_tags.py")
    assert c.relkey(posix, pathlib.PurePosixPath("/repo")) == "scripts/image_tags.py"


# --- the live-repo control ----------------------------------------------------


def test_the_checked_in_ledger_is_consistent_with_the_tree_today():
    """THE ONE ASSERTION OVER THE REAL REPOSITORY, and what makes the guard bite
    rather than merely exist: the committed ledger must agree with the committed
    tree, in both directions. Everything above is synthetic, and a checker whose
    path resolution had drifted from this repository's actual layout would pass
    all of it.

    Deliberately NOT an assertion about the counts. "Eight recorded, forty-three
    tested" fails on the next script added, the next test written and the next
    exemption closed -- three things that should happen freely."""
    assert c.main(["check", "--strict"]) == 0


def test_every_recorded_script_still_exists_and_is_still_untested():
    """The same claim stated over `scan` rather than through the exit code, so a
    failure names which entry is wrong instead of only that something is."""
    untested, stale = c.scan()
    assert untested == [], f"unrecorded untested script(s): {untested}"
    assert stale == [], f"stale ledger entr(y/ies): {stale}"


def test_the_real_ledger_is_smaller_than_the_measured_baseline():
    """The ledger shipped SMALLER than the gap it records -- eleven measured, three
    closed with real tests, eight recorded -- and that is a claim about direction
    rather than a number to defend. Asserted as a CEILING so closing another gap
    passes and adding an exemption without closing anything does not."""
    accepted = c.load_ledger()
    assert len(accepted) <= 8, (
        f"the ledger has grown to {len(accepted)} entries from the 8 it shipped "
        "with. An exemption is cheaper to write than a test, which is exactly why "
        "the count needs a ceiling: raise it in the same change that argues for "
        "the new entry, and the raise is the review.")
    for script in accepted:
        assert (REPO / script).exists(), f"{script} is recorded but does not exist"
