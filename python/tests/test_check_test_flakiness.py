"""Tests for the test-flakiness checker.

The awkward case, and the same one test_check_workflow_concurrency.py faces: the
invariant this checker guards is currently HELD — every unbounded sleep in the
tree was rewritten before the checker landed — so running it against the real
repository proves only that it did not crash. A checker that has never rejected
anything is indistinguishable from one whose regex stopped matching.

So every test here drives it with a violation it MUST catch, and with a
correct-looking near-miss it must NOT catch. The real-repo case is the single
control at the end.

The near-misses are the point. A checker that flagged every `time.Sleep` would
be trivially correct and would be switched off within a week, because 26 of this
suite's 29 sleeps were already inside properly bounded loops.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_test_flakiness as c  # noqa: E402

# --- the shape that must FAIL -------------------------------------------------

BARE_SLEEP = """\
package api

import (
	"testing"
	"time"
)

func TestSomethingStaysOpen(t *testing.T) {
	start(t)
	time.Sleep(100 * time.Millisecond)
	if done(t) {
		t.Fatal("it should not have finished")
	}
}
"""

# --- the shapes that must PASS ------------------------------------------------

DEADLINE_POLL = """\
package api

import (
	"testing"
	"time"
)

func TestSomethingFinishes(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done(t) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("never finished")
}
"""

TIME_AFTER_POLL = """\
package store

import (
	"testing"
	"time"
)

func TestReplayFills(t *testing.T) {
	deadline := time.After(3 * time.Second)
	for len(all) < 3 {
		select {
		case <-deadline:
			t.Fatal("ring never filled")
		default:
		}
		all = s.Replay(0)
		time.Sleep(10 * time.Millisecond)
	}
}
"""

# The real line from internal/server/tds_strict_test.go, trailing comment and
# all. The first draft of the checker anchored the `for` header on `{$` against
# the raw line and therefore missed this one, reporting a correctly bounded
# retry loop as an unbounded sleep.
COUNTED_WITH_COMMENT = """\
package server

import (
	"testing"
	"time"
)

func TestRelay(t *testing.T) {
	var lastErr error
	for i := 0; i < 60; i++ { // SQL Server may still be starting
		if lastErr = run(); lastErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
}
"""

# --- the unbounded spin -------------------------------------------------------

UNBOUNDED_POLL = """\
package api

import (
	"testing"
	"time"
)

func TestSpinsForever(t *testing.T) {
	for {
		if done(t) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
"""


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """Write Go test sources into a fake repo root and point the checker at it."""
    def build(files, ledger=None):
        root = tmp_path / "repo"
        for name, body in files.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body)
        docs = root / "docs"
        docs.mkdir(parents=True, exist_ok=True)
        ledger_path = docs / "test-flakiness.json"
        ledger_path.write_text(json.dumps(ledger if ledger is not None else {"accepted": []}))
        monkeypatch.setattr(c, "ROOT", root)
        monkeypatch.setattr(c, "LEDGER", ledger_path)
        return root
    return build


# --- direction one: it catches what it is for --------------------------------

def test_a_bare_sleep_before_an_assertion_fails(tree, capsys):
    tree({"internal/api/drive_test.go": BARE_SLEEP})
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "unbounded-sleep" in out
    assert "TestSomethingStaysOpen" in out, "the finding must name the test"
    assert "drive_test.go:10" in out, "the finding must name the line"


def test_an_unbounded_polling_loop_fails(tree, capsys):
    tree({"internal/api/spin_test.go": UNBOUNDED_POLL})
    assert c.main(["check", "--strict"]) == 1
    assert "unbounded-poll" in capsys.readouterr().out


def test_a_long_sleep_is_reported_so_it_must_be_recorded(tree, capsys):
    # Bounded, correct, and still surfaced: the aggregate cost of these is real
    # and invisible at any one call site.
    tree({"internal/server/tds_test.go": COUNTED_WITH_COMMENT})
    assert c.main(["check", "--strict"]) == 1
    assert "long-sleep" in capsys.readouterr().out


# --- direction two: it does NOT catch correct code ---------------------------

@pytest.mark.parametrize("name,body", [
    ("deadline poll", DEADLINE_POLL),
    ("time.After poll", TIME_AFTER_POLL),
])
def test_a_bounded_poll_passes(tree, capsys, name, body):
    tree({"internal/api/poll_test.go": body})
    assert c.main(["check", "--strict"]) == 0, f"{name} is the CORRECT shape and must not be flagged"


def test_a_counted_loop_with_a_trailing_comment_is_bounded(tree):
    # The regression this pins: stripping the trailing comment before matching
    # the `for` header. Without it this reads as an unbounded sleep.
    findings = c.findings_for(pathlib.Path("x_test.go"), COUNTED_WITH_COMMENT)
    assert [f["kind"] for f in findings] == ["long-sleep"], \
        "a counted loop is bounded; only its one-second interval should be surfaced"


# --- the ledger, in both directions ------------------------------------------

def test_a_recorded_site_passes(tree, capsys):
    tree({"internal/api/drive_test.go": BARE_SLEEP},
         ledger={"accepted": [{
             "file": "internal/api/drive_test.go",
             "symbol": "TestSomethingStaysOpen",
             "bucket": "container-retry",
             "reason": "deliberate, for the test",
         }]})
    assert c.main(["check", "--strict"]) == 0
    assert "recorded" in capsys.readouterr().out


def test_a_ledger_entry_for_a_site_that_no_longer_exists_fails(tree, capsys):
    # The direction a "flag what is new" checker would never look. A site gets
    # fixed, its allowance stays, and the next sleep added to that same test is
    # waved through by an entry that was written about different code.
    tree({"internal/api/poll_test.go": DEADLINE_POLL},
         ledger={"accepted": [{
             "file": "internal/api/gone_test.go",
             "symbol": "TestDeleted",
             "bucket": "container-retry",
             "reason": "this site was rewritten and the entry was not removed",
         }]})
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "no longer flagged" in out
    assert "TestDeleted" in out


# --- the ledger key, on a platform that is not this one ----------------------

# WHAT THESE PIN, and why they are written against `PureWindowsPath` rather
# than left to the Windows CI leg to discover. The ledger is a checked-in JSON
# file with forward slashes in it; the checker built its lookup key with
# `str(path.relative_to(ROOT))`, which is `internal\server\x_test.go` on
# Windows. Nothing matched, so on that platform alone EVERY recorded site read
# as unrecorded and EVERY ledger entry read as stale -- both directions of a
# both-directions check firing at once, from one missing `as_posix()`.
#
# Every test above passes on a POSIX runner whether or not that bug is present,
# which is exactly how it reached review behind a green Linux run. Driving the
# Windows path flavour explicitly is what moves the guard onto every leg.

def test_a_nested_finding_is_keyed_the_way_the_ledger_spells_it():
    findings = c.findings_for(
        pathlib.PureWindowsPath(r"internal\api\drive_test.go"), BARE_SLEEP)
    assert [f["file"] for f in findings] == ["internal/api/drive_test.go"], \
        "the ledger is written with forward slashes, so the key must be too"


def test_relkey_strips_the_root_whatever_the_separator():
    assert c.relkey(
        pathlib.PureWindowsPath(r"C:\repo\internal\server\tds_test.go"),
        root=pathlib.PureWindowsPath(r"C:\repo"),
    ) == "internal/server/tds_test.go"
    assert c.relkey(
        pathlib.PurePosixPath("/repo/internal/server/tds_test.go"),
        root=pathlib.PurePosixPath("/repo"),
    ) == "internal/server/tds_test.go"


def test_a_windows_flavoured_finding_matches_a_forward_slash_ledger_entry(tree, capsys):
    # The end-to-end half: the same source, recorded in the ledger the only way
    # a checked-in file can spell it. Before the fix this failed on Windows with
    # the site unrecorded AND the entry stale, and passed everywhere else.
    tree({"internal/api/drive_test.go": BARE_SLEEP},
         ledger={"accepted": [{
             "file": "internal/api/drive_test.go",
             "symbol": "TestSomethingStaysOpen",
             "bucket": "container-retry",
             "reason": "deliberate, for the test",
         }]})
    accepted = c.load_ledger()
    findings = c.findings_for(
        pathlib.PureWindowsPath(r"internal\api\drive_test.go"), BARE_SLEEP)
    assert [f"{f['file']}:{f['symbol']}" in accepted for f in findings] == [True], \
        "a ledger entry must match its site regardless of the host's separator"


# --- the vacuity guard --------------------------------------------------------

def test_walking_zero_files_is_a_failure_not_a_pass(tree, capsys):
    tree({})
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
