"""Tests for the Python-surface test-flakiness checker.

THE AWKWARD CASE, and the same one test_check_test_flakiness.py faces for the Go
sibling: the invariant this checker guards is HELD as of the commit that added
it -- the three genuinely load-dependent sites were rewritten and the two
intentional ones recorded -- so running it against the real repository proves
only that it did not crash. A checker that has never rejected anything is
indistinguishable from one whose detection stopped matching.

So every test here drives it with a violation it MUST catch, and with a
correct-looking near-miss it must NOT catch. The real repository is the single
control at the end.

THE NEAR-MISSES ARE THE POINT. A checker that flagged every `time.sleep` would
be trivially correct and switched off within a week: 56 of this surface's 58
flagged sites are correctly bounded already, and `e2e/fabric-cicd/driver.py`
carries two `while True` loops whose deadline lives in the loop BODY. Reporting
those as violations would make this something people argue with rather than fix,
which is the failure mode the Go checker's own history documents.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_python_test_flakiness as c  # noqa: E402

# --- the shapes that must FAIL ------------------------------------------------

# A module-level sleep, which is the e2e drivers' natural shape: they are
# top-level scripts, so this has no enclosing function at all. The real one this
# is modelled on (e2e/adls-sdk/driver.py) asserted a NEGATIVE right after it.
MODULE_LEVEL_SLEEP = '''\
"""An e2e driver: a top-level script, so this sleep has no enclosing function."""
import time

trigger_a_write()
time.sleep(2)
assert not runs(), "a write outside the prefix started a run"
'''

# `while True` with nothing bounding it anywhere.
UNBOUNDED_POLL = '''\
import time


def test_spins_forever():
    while True:
        if done():
            return
        time.sleep(0.5)
'''

# Correctly bounded and still surfaced, because the aggregate cost of these is
# real and invisible at any one call site.
BOUNDED_LONG_SLEEP = '''\
import time


def test_waits_for_a_container():
    for _ in range(60):
        if healthy():
            break
        time.sleep(1)
'''

# --- the shapes that must PASS ------------------------------------------------

# (a) a `for` over a finite iterable. The interval is deliberately sub-second so
# this tests the BOUND and not the long-sleep rule on top of it.
FOR_RANGE_POLL = '''\
import time


def test_polls_a_finite_number_of_times():
    for _ in range(60):
        if done():
            return
        time.sleep(0.5)
    raise AssertionError("never finished")
'''

# (b) a `while` whose test consults a monotonic clock.
MONOTONIC_DEADLINE_POLL = '''\
import time


def test_polls_until_a_deadline():
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if done():
            return
        time.sleep(0.5)
    raise AssertionError("never finished")
'''

# (b) a `while` bounded by a counter the body increments.
COUNTER_WHILE_POLL = '''\
import time


def test_polls_a_bounded_number_of_times():
    tries = 0
    while tries < 60:
        if done():
            return
        tries += 1
        time.sleep(0.5)
'''

# (c) THE BODY-DEADLINE SHAPE, transcribed from e2e/fabric-cicd/driver.py's LRO
# poll. This is correct code, and a checker that only inspected the loop HEADER
# reports it as an unbounded poll -- which is why shape (c) is mandatory rather
# than a refinement.
BODY_DEADLINE_ASSERT = '''\
import time


def poll_lro(url, deadline=60):
    end, states = time.time() + deadline, []
    while True:
        body = get(url)
        states.append(body["status"])
        if body["status"] == "Succeeded":
            break
        assert body["status"] in ("NotStarted", "Running"), body
        assert time.time() < end, f"never left {body['status']}"
        time.sleep(0.2)
    return states
'''

# (c) the other spelling: `if <clock>: raise`. e2e/waiting.py's own `wait_for`
# has exactly this shape, and raises rather than asserts BECAUSE an `assert` is
# stripped under `python -O` -- a deadline guard that vanishes under -O is a
# real bug, so the checker has to recognise both forms.
BODY_DEADLINE_RAISE = '''\
import time


def wait_for(timeout, cond, msg):
    deadline = time.monotonic() + timeout
    while True:
        got = cond()
        if got:
            return got
        if time.monotonic() >= deadline:
            raise AssertionError(msg)
        time.sleep(0.05)
'''


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """Write Python sources into a fake repo root and point the checker at it."""
    def build(files, ledger=None):
        root = tmp_path / "repo"
        for name, body in files.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body)
        docs = root / "docs"
        docs.mkdir(parents=True, exist_ok=True)
        ledger_path = docs / "python-test-flakiness.json"
        ledger_path.write_text(json.dumps(ledger if ledger is not None else {"accepted": []}))
        monkeypatch.setattr(c, "ROOT", root)
        monkeypatch.setattr(c, "LEDGER", ledger_path)
        return root
    return build


# --- direction one: it catches what it is for --------------------------------

def test_a_module_level_sleep_before_an_assertion_fails(tree, capsys):
    tree({"e2e/adls-sdk/driver.py": MODULE_LEVEL_SLEEP})
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "unbounded-sleep" in out
    assert "driver.py:5" in out, "the finding must name the line"
    assert "<module>" in out, "a top-level e2e statement has no enclosing function"


def test_an_unbounded_polling_loop_fails(tree, capsys):
    tree({"python/tests/test_spin.py": UNBOUNDED_POLL})
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "unbounded-poll" in out
    assert "test_spins_forever" in out, "the finding must name the test"


def test_a_bounded_long_sleep_is_reported_so_it_must_be_recorded(tree, capsys):
    # Bounded, correct, and still surfaced: the aggregate cost of these is real
    # and invisible at any one call site.
    tree({"e2e/livy/client.py": BOUNDED_LONG_SLEEP})
    assert c.main(["check", "--strict"]) == 1
    assert "long-sleep" in capsys.readouterr().out


# --- direction two: it does NOT catch correct code ---------------------------

@pytest.mark.parametrize("name,body", [
    ("for over range", FOR_RANGE_POLL),
    ("while on a monotonic deadline", MONOTONIC_DEADLINE_POLL),
    ("while on a body-incremented counter", COUNTER_WHILE_POLL),
    ("while True + assert time.time() < end", BODY_DEADLINE_ASSERT),
    ("while True + if clock: raise", BODY_DEADLINE_RAISE),
])
def test_a_bounded_poll_passes(tree, name, body):
    tree({"e2e/suite/driver.py": body})
    assert c.main(["check", "--strict"]) == 0, \
        f"{name} is the CORRECT shape and must not be flagged"


def test_the_body_deadline_shape_from_fabric_cicd_is_bounded():
    """THE REGRESSION THIS PINS, and the one the plan called mandatory.

    `e2e/fabric-cicd/driver.py` has two `while True` loops whose only bound is
    `assert time.time() < end` in the BODY. A checker reading loop headers alone
    reports both as unbounded polls -- correct code, reported as a violation, in
    the suite that publishes through Microsoft's own fabric-cicd tool. The Go
    sibling learned the same lesson and searches its loop body for a deadline
    rather than only its header.
    """
    findings = c.findings_for(pathlib.PurePosixPath("e2e/fabric-cicd/driver.py"),
                              BODY_DEADLINE_ASSERT)
    assert findings == [], "a body-asserted deadline bounds the loop"


def test_the_real_fabric_cicd_driver_is_not_flagged():
    """The same claim against the actual file, not a transcription of it.

    The test above could pass while the real driver had drifted into a shape the
    checker no longer recognises. This reads the file on disk.
    """
    import importlib
    real = importlib.reload(c)
    path = real.ROOT / "e2e" / "fabric-cicd" / "driver.py"
    findings = real.findings_for(path, path.read_text(encoding="utf-8"))
    assert findings == [], \
        f"fabric-cicd's body-deadline polls must not be flagged, got {findings}"


def test_an_infinite_generator_is_not_a_finite_for(tree, capsys):
    """`for _ in itertools.count()` is a `for` loop that never ends.

    Shape (a) says a `for` is bounded because its iterable must exhaust. That is
    true of `range(60)` and false of `itertools.count()`, so the exemption is
    keyed on the iterable rather than on the keyword.
    """
    tree({"e2e/suite/driver.py": '''\
import itertools
import time


def test_spins():
    for _ in itertools.count():
        if done():
            return
        time.sleep(0.5)
'''})
    assert c.main(["check", "--strict"]) == 1
    assert "unbounded-poll" in capsys.readouterr().out


def test_a_bare_comparison_is_not_a_bound(tree, capsys):
    """`while len(rows) < 3` spins forever if the rows never arrive.

    It is a Compare in the loop test, like `while tries < 60` is -- but nothing
    in the body advances it toward termination, so treating every comparison as
    a bound would wave through exactly the hang this checker is for.
    """
    tree({"e2e/suite/driver.py": '''\
import time

rows = []


def test_waits_for_rows():
    while len(rows) < 3:
        rows.extend(fetch())
        time.sleep(0.5)
'''})
    assert c.main(["check", "--strict"]) == 1
    assert "unbounded-poll" in capsys.readouterr().out


# --- the symbol, and its `<module>` fallback ----------------------------------

def test_a_nested_helper_is_named_by_the_test_that_contains_it():
    """The OUTERMOST enclosing function, not the innermost.

    python/tests/test_notebookutils_shim.py had its sleeps inside a nested
    `other()` closure; keying on that would name a helper three files could
    share, where the test name is what a reader and a ledger both need. The Go
    sibling gets this for free because Go's nested functions are anonymous.
    """
    findings = c.findings_for(pathlib.PurePosixPath("python/tests/test_x.py"), '''\
import time


def test_two_threads_interleave():
    def other():
        time.sleep(0.05)
    other()
''')
    assert [f["symbol"] for f in findings] == ["test_two_threads_interleave"]


def test_every_module_level_site_in_a_file_shares_one_key():
    """One `<module>` entry covers them all -- the same many-to-one the Go
    ledger already carries for tds_reflect_test.go, which has two sites under
    one symbol."""
    findings = c.findings_for(pathlib.PurePosixPath("e2e/suite/driver.py"), '''\
import time

time.sleep(2)
go()
time.sleep(3)
''')
    assert [f["symbol"] for f in findings] == ["<module>", "<module>"]
    assert len({f"{f['file']}:{f['symbol']}" for f in findings}) == 1


# --- the long-sleep threshold -------------------------------------------------

def test_a_sub_second_bounded_sleep_is_not_a_long_sleep():
    findings = c.findings_for(pathlib.PurePosixPath("e2e/suite/driver.py"), FOR_RANGE_POLL)
    assert findings == []


def test_a_non_literal_interval_is_not_claimed_to_be_long():
    """`time.sleep(min(float(resp.headers.get("Retry-After", "1")), 60))` is a
    real line in this repo. No static answer exists for its interval, so the
    site is not claimed to be a long sleep -- silence means "not known to be
    >= 1s", never "known to be short"."""
    findings = c.findings_for(pathlib.PurePosixPath("e2e/suite/driver.py"), '''\
import time


def poll(resp):
    for _ in range(10):
        time.sleep(min(float(resp.headers.get("Retry-After", "1")), 60))
''')
    assert findings == []


# --- the ledger, in both directions ------------------------------------------

def test_a_recorded_site_passes(tree, capsys):
    tree({"e2e/adls-sdk/driver.py": MODULE_LEVEL_SLEEP},
         ledger={"accepted": [{
             "file": "e2e/adls-sdk/driver.py",
             "symbol": "<module>",
             "bucket": "liveness-probe",
             "reason": "deliberate, for the test",
         }]})
    assert c.main(["check", "--strict"]) == 0
    assert "recorded" in capsys.readouterr().out


def test_a_ledger_entry_for_a_site_that_no_longer_exists_fails(tree, capsys):
    # The direction a "flag what is new" checker would never look. A site gets
    # fixed, its allowance stays, and the next sleep added to that same function
    # is waved through by an entry that was written about different code.
    tree({"e2e/suite/driver.py": FOR_RANGE_POLL},
         ledger={"accepted": [{
             "file": "e2e/gone/driver.py",
             "symbol": "wait_health",
             "bucket": "service-cold-start",
             "reason": "this site was rewritten and the entry was not removed",
         }]})
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "no longer flagged" in out
    assert "wait_health" in out


# --- the ledger key, on a platform that is not this one ----------------------

# WHAT THESE PIN, and why they are written against `PureWindowsPath` rather than
# left to the Windows CI leg to discover. The ledger is a checked-in JSON file
# with forward slashes in it; the GO sibling built its lookup key with
# `str(path.relative_to(ROOT))`, which is `internal\server\x_test.go` on
# Windows. Nothing matched, so on that platform alone EVERY recorded site read
# as unrecorded and EVERY ledger entry read as stale -- both directions of a
# both-directions check firing at once, from one missing `as_posix()`. It took
# the windows leg of make-targets.yml and the windows leg of the pytest job red
# while every POSIX leg stayed green.
#
# Every test above passes on a POSIX runner whether or not that bug is present,
# which is exactly how it reached review behind a green Linux run. The Windows
# pytest leg runs THIS file too, so driving the Windows flavour explicitly is
# what makes a regression from `as_posix()` to `str()` fail on darwin and Linux
# as well -- where it is cheap to notice -- instead of only on Windows.

def test_a_nested_finding_is_keyed_the_way_the_ledger_spells_it():
    findings = c.findings_for(
        pathlib.PureWindowsPath(r"e2e\adls-sdk\driver.py"), MODULE_LEVEL_SLEEP)
    assert [f["file"] for f in findings] == ["e2e/adls-sdk/driver.py"], \
        "the ledger is written with forward slashes, so the key must be too"


def test_relkey_strips_the_root_whatever_the_separator():
    assert c.relkey(
        pathlib.PureWindowsPath(r"C:\repo\e2e\adls-sdk\driver.py"),
        root=pathlib.PureWindowsPath(r"C:\repo"),
    ) == "e2e/adls-sdk/driver.py"
    assert c.relkey(
        pathlib.PurePosixPath("/repo/e2e/adls-sdk/driver.py"),
        root=pathlib.PurePosixPath("/repo"),
    ) == "e2e/adls-sdk/driver.py"


def test_a_windows_flavoured_finding_matches_a_forward_slash_ledger_entry(tree):
    # The end-to-end half: the same source, recorded in the ledger the only way
    # a checked-in file can spell it. Before the fix the Go sibling failed on
    # Windows with the site unrecorded AND the entry stale, and passed elsewhere.
    tree({"e2e/adls-sdk/driver.py": MODULE_LEVEL_SLEEP},
         ledger={"accepted": [{
             "file": "e2e/adls-sdk/driver.py",
             "symbol": "<module>",
             "bucket": "liveness-probe",
             "reason": "deliberate, for the test",
         }]})
    accepted = c.load_ledger()
    findings = c.findings_for(
        pathlib.PureWindowsPath(r"e2e\adls-sdk\driver.py"), MODULE_LEVEL_SLEEP)
    assert [f"{f['file']}:{f['symbol']}" in accepted for f in findings] == [True], \
        "a ledger entry must match its site regardless of the host's separator"


# --- the vacuity guard --------------------------------------------------------

def test_walking_zero_files_is_a_failure_not_a_pass(tree, capsys):
    tree({})
    assert c.main(["check", "--strict"]) == 1
    assert "vacuously" in capsys.readouterr().out


# --- the duplicate-copy hazard ------------------------------------------------

def test_a_checked_in_build_copy_is_not_walked(tree, capsys):
    """`python/fabric-target/build/lib/` is a CHECKED-IN setuptools staging copy
    of a package that also lives at `python/fabric-target/fabric_target/`. A
    sweep that walked it reports the same site under two paths, only one of
    which anyone edits -- the Python member of the family `.claude` is in the
    list for (a nested git worktree, i.e. a second checkout of this repo).
    """
    tree({"python/fabric-target/tests/build/lib/copy.py": MODULE_LEVEL_SLEEP})
    # Walked zero files, so the vacuity guard fires rather than a finding — the
    # build copy contributed nothing, which is the claim.
    assert c.main(["check", "--strict"]) == 1
    assert "vacuously" in capsys.readouterr().out


# --- the control: the real repository ----------------------------------------

def test_the_real_repository_passes():
    """The single control. It says nothing on its own — see the module docstring."""
    import importlib
    real = importlib.reload(c)
    assert real.main(["check", "--strict"]) == 0


def test_no_finding_over_the_real_tree_carries_a_native_separator():
    """Vacuous on POSIX, load-bearing on Windows — which is where it broke.

    The three tests above hold the separator invariant on every platform; this
    one holds it over the REAL tree, so a path built somewhere other than
    `relkey` cannot reintroduce it for the files that actually exist.
    """
    import importlib
    real = importlib.reload(c)
    offenders = [f for f in real.scan() if "\\" in f["file"]]
    assert offenders == [], "a finding is keyed with the host's separator, not the ledger's"


def test_the_real_ledger_has_no_entry_without_a_reason():
    """A bucket with no reason is a waiver, not a record."""
    import importlib
    real = importlib.reload(c)
    for key, entry in real.load_ledger().items():
        assert entry.get("bucket"), f"{key} has no bucket"
        assert len(entry.get("reason", "")) > 40, f"{key} has no substantive reason"
