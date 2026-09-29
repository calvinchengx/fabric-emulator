"""Tests for the vitest-surface test-flakiness checker.

THE SAME AWKWARD CASE both siblings document: the invariant this checker guards
is HELD as of the commit that added it. The suite's one real-clock
`setTimeout` (Flow.test.ts, asserting a dropped-count chip stayed absent) was
rewritten onto `staysAbsent` (src/testing.ts) in the same change, so running
this checker against the real tree proves only that it did not crash on it.

So every test here drives it with a violation it MUST catch, and with a
correct-looking near-miss it must NOT catch — the fake-clock idiom this suite
already uses nine times across five files, which a checker that flagged every
`setTimeout` would treat as nine new violations and get argued with rather than
fixed, exactly the failure mode the Go checker's own history records.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_vitest_test_flakiness as c  # noqa: E402


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """Write a fake portal tree and point the checker at it."""
    def build(files, ledger=None, skip_src=False):
        root = tmp_path / "repo"
        if not skip_src:
            (root / "portal" / "src").mkdir(parents=True, exist_ok=True)
        for name, body in files.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body)
        docs = root / "docs"
        docs.mkdir(parents=True, exist_ok=True)
        ledger_path = docs / "vitest-test-flakiness.json"
        ledger_path.write_text(json.dumps(ledger if ledger is not None else {"accepted": []}))
        monkeypatch.setattr(c, "ROOT", root)
        monkeypatch.setattr(c, "LEDGER", ledger_path)
        return root
    return build


# --- vacuity guards ------------------------------------------------------------

def test_a_tree_with_no_test_files_fails(tree, capsys):
    tree({})
    assert c.main(["check"]) == 1
    assert "walked zero" in capsys.readouterr().out


def test_a_removed_scan_root_fails_rather_than_reporting_zero_findings(tree, capsys):
    """`portal/src` renamed away must not read the same as `portal/src` present
    and clean -- both currently print "zero test files", but only one of them
    means the suite was actually inspected."""
    tree({}, skip_src=True)
    out = c.main(["check"])
    assert out == 1
    assert "scan root" in capsys.readouterr().out or "walked zero" in capsys.readouterr().out


# --- direction one: it catches what it is for ----------------------------------

def test_a_bare_settimeout_before_an_assertion_fails(tree, capsys):
    tree({"portal/src/Widget.test.ts": '''\
import { expect, it } from 'vitest';

it('shows the badge eventually', async () => {
  trigger();
  await new Promise((r) => setTimeout(r, 20));
  expect(screen.getByText('done')).toBeInTheDocument();
});
'''})
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "bare-sleep" in out
    assert "Widget.test.ts:5" in out
    assert "shows the badge eventually" in out


def test_a_long_sleep_is_reported_as_its_own_kind(tree, capsys):
    tree({"portal/src/Widget.test.ts": '''\
import { expect, it } from 'vitest';

it('waits a while', async () => {
  await new Promise((r) => setTimeout(r, 1500));
  expect(true).toBe(true);
});
'''})
    assert c.main(["check", "--strict"]) == 1
    assert "long-sleep" in capsys.readouterr().out


def test_a_short_sleep_is_a_bare_sleep_not_a_long_one(tree, capsys):
    tree({"portal/src/Widget.test.ts": '''\
import { expect, it } from 'vitest';

it('waits briefly', async () => {
  await new Promise((r) => setTimeout(r, 999));
  expect(true).toBe(true);
});
'''})
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "bare-sleep" in out
    assert "long-sleep" not in out


# --- direction two: it does NOT catch the fake-clock idiom --------------------

def test_settimeout_under_fake_timers_is_not_flagged(tree):
    tree({"portal/src/Widget.test.ts": '''\
import { vi, expect, it } from 'vitest';

it('debounces the search box', async () => {
  vi.useFakeTimers();
  type('a');
  await vi.advanceTimersByTimeAsync(1);
  await new Promise((r) => setTimeout(r, 500));
  expect(calls()).toBe(1);
});
'''})
    assert c.main(["check", "--strict"]) == 0


def test_a_sleep_after_switching_back_to_real_timers_is_flagged(tree, capsys):
    """`useFakeTimers` does not cover the rest of the file — a test that
    switches back must be judged on the real clock again, the same as the one
    real site in this suite does (Flow.test.ts: `useRealTimers()` at line
    1035, then a bare `setTimeout` at line 1140)."""
    tree({"portal/src/Widget.test.ts": '''\
import { vi, expect, it } from 'vitest';

it('one', async () => {
  vi.useFakeTimers();
  await vi.advanceTimersByTimeAsync(1);
  vi.useRealTimers();
});

it('two', async () => {
  await new Promise((r) => setTimeout(r, 20));
  expect(true).toBe(true);
});
'''})
    assert c.main(["check", "--strict"]) == 1
    findings_lines = [
        line for line in capsys.readouterr().out.splitlines() if "[bare-sleep]" in line
    ]
    assert len(findings_lines) == 1
    assert " two " in findings_lines[0]


def test_advancing_the_fake_clock_is_never_a_sleep_call(tree):
    """`vi.advanceTimersByTimeAsync` and `vi.advanceTimersByTime` do not spell
    `setTimeout` at all — nothing to exempt because nothing matches."""
    tree({"portal/src/Widget.test.ts": '''\
import { vi, expect, it } from 'vitest';

it('advances a lot', async () => {
  vi.useFakeTimers();
  await vi.advanceTimersByTimeAsync(30000);
  expect(true).toBe(true);
});
'''})
    assert c.main(["check", "--strict"]) == 0


# --- the symbol, and its `<module>` fallback -----------------------------------

def test_the_symbol_is_the_enclosing_it_blocks_title(tree, capsys):
    tree({"portal/src/Widget.test.ts": '''\
import { expect, it, describe } from 'vitest';

describe('Widget', () => {
  it('flakes here', async () => {
    await new Promise((r) => setTimeout(r, 20));
    expect(true).toBe(true);
  });
});
'''})
    assert c.main(["check", "--strict"]) == 1
    assert "flakes here" in capsys.readouterr().out


def test_a_site_outside_any_it_block_falls_back_to_module(tree, capsys):
    """A `beforeEach` hook (or any top-level statement) has no enclosing
    `it`/`test`, the same position the Python checker's `<module>` fallback
    exists for on its e2e drivers."""
    tree({"portal/src/Widget.test.ts": '''\
import { beforeEach } from 'vitest';

beforeEach(async () => {
  await new Promise((r) => setTimeout(r, 20));
});
'''})
    assert c.main(["check", "--strict"]) == 1
    assert "<module>" in capsys.readouterr().out


# --- noise stripping: comments and string/template literals -------------------

def test_a_settimeout_mentioned_in_a_comment_is_not_flagged(tree):
    tree({"portal/src/Widget.test.ts": '''\
import { expect, it } from 'vitest';

// TODO: stop calling setTimeout(r, 20) here one day
it('is clean', async () => {
  expect(true).toBe(true);
});
'''})
    assert c.main(["check", "--strict"]) == 0


def test_a_settimeout_inside_a_string_literal_is_not_flagged(tree):
    tree({"portal/src/Widget.test.ts": '''\
import { expect, it } from 'vitest';

it('renders the literal source of a snippet', async () => {
  const code = "await new Promise((r) => setTimeout(r, 20))";
  expect(code).toContain('setTimeout');
});
'''})
    assert c.main(["check", "--strict"]) == 0


def test_a_multiline_block_comment_does_not_shift_line_numbers(tree, capsys):
    """The block-comment blank-out must preserve line count, or every finding
    after a JSDoc block would be reported against the wrong line."""
    tree({"portal/src/Widget.test.ts": '''\
/** A long
 * JSDoc block
 * spanning several lines.
 */
import { expect, it } from 'vitest';

it('flakes on line six', async () => {
  await new Promise((r) => setTimeout(r, 20));
  expect(true).toBe(true);
});
'''})
    assert c.main(["check", "--strict"]) == 1
    assert "Widget.test.ts:8" in capsys.readouterr().out


def test_a_title_with_backslashes_is_still_captured_correctly():
    """CodeQL flagged an earlier version of `_IT`'s title group
    (`(?:\\\\.|(?!\\1).)*`) as exponential-backtracking: `.` and `\\.` both
    match a lone backslash, so a title with many of them can be partitioned
    between the two alternatives in exponentially many ways before the engine
    gives up on a non-match. The fix excludes `\\` from the plain-char branch
    (`(?:\\\\.|(?!\\1)[^\\\\])*`), which is what this test is actually pinning:
    a real title using an escape must still parse the same as before, not just
    "does not hang" -- the hang itself is a job for CodeQL, run on every PR,
    to keep catching."""
    findings = c.findings_for(pathlib.PurePosixPath("portal/src/Widget.test.ts"), '''\
import { expect, it } from 'vitest';

it('handles a path like C:\\\\Users\\\\x', async () => {
  await new Promise((r) => setTimeout(r, 20));
  expect(true).toBe(true);
});
''')
    assert [f["symbol"] for f in findings] == ['handles a path like C:\\\\Users\\\\x']


# --- the ledger, both directions -----------------------------------------------

def test_ledger_key_refuses_an_entry_with_no_kind():
    with pytest.raises(KeyError):
        c.ledger_key({"file": "portal/src/x.test.ts", "symbol": "it"})


def test_an_accepted_site_passes_strict(tree):
    tree(
        {"portal/src/Widget.test.ts": '''\
import { expect, it } from 'vitest';

it('flakes here', async () => {
  await new Promise((r) => setTimeout(r, 20));
  expect(true).toBe(true);
});
'''},
        ledger={"accepted": [{
            "file": "portal/src/Widget.test.ts", "symbol": "flakes here",
            "kind": "bare-sleep", "bucket": "accepted-for-a-test",
            "reason": "test fixture",
        }]},
    )
    assert c.main(["check", "--strict"]) == 0


def test_a_stale_ledger_entry_fails(tree, capsys):
    """An entry naming a site that is no longer flagged must fail too — the
    direction a "what's new" reader would never think to check."""
    tree(
        {"portal/src/Widget.test.ts": '''\
import { expect, it } from 'vitest';

it('is clean now', async () => {
  expect(true).toBe(true);
});
'''},
        ledger={"accepted": [{
            "file": "portal/src/Widget.test.ts", "symbol": "is clean now",
            "kind": "bare-sleep", "bucket": "accepted-for-a-test",
            "reason": "used to sleep, does not any more",
        }]},
    )
    assert c.main(["check", "--strict"]) == 1
    assert "stale" in capsys.readouterr().out.lower()


# --- report mode ----------------------------------------------------------------

def test_report_mode_always_exits_zero_and_marks_new_vs_accepted(tree, capsys):
    tree({"portal/src/Widget.test.ts": '''\
import { expect, it } from 'vitest';

it('flakes here', async () => {
  await new Promise((r) => setTimeout(r, 20));
  expect(true).toBe(true);
});
'''})
    assert c.main(["check"]) == 0
    out = capsys.readouterr().out
    assert "NEW" in out
    assert "1 not recorded" in out


# --- path separators, the same defect 66ccfd7b's sibling test caught -----------

def test_relkey_compares_as_posix_even_from_a_windows_style_path():
    key = c.relkey(pathlib.PureWindowsPath(r"C:\repo\portal\src\Widget.test.ts"),
                    root=pathlib.PureWindowsPath(r"C:\repo"))
    assert key == "portal/src/Widget.test.ts"


# --- the real tree ----------------------------------------------------------------

def test_the_real_portal_suite_has_no_unrecorded_findings():
    """The claim above, against the actual file rather than a transcription of
    it: the real Flow.test.ts site was rewritten onto `staysAbsent`, not
    recorded, so the real ledger should need to hold nothing at all."""
    import importlib
    real = importlib.reload(c)
    assert real.main(["check", "--strict"]) == 0


def test_the_real_flow_test_no_longer_has_a_bare_settimeout():
    import importlib
    real = importlib.reload(c)
    path = real.ROOT / "portal" / "src" / "Flow.test.ts"
    findings = real.findings_for(path, path.read_text(encoding="utf-8"))
    assert findings == [], f"expected staysAbsent to have removed this, got {findings}"
