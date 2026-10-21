"""scripts/check_clock_test_flakiness.py, driven against violations it must
catch and near-misses it must not.

WHY THIS FILE AND NOT A READING OF THE CHECKER. The checker passes against the
tree today, which is the awkward case docs/60-test-flakiness.md §5 names: a
check that passes now passes whether or not it works. Both defects this
repository has recorded in a checker were found by DRIVING one -- the 84 false
positives in check_doc_drift's first path class, and the kind-blind match key
that exempted 42 of 44 symbols in check_python_test_flakiness -- and neither
would have been found by reading it. Two more were found by writing THIS file:
the missing two-statement spelling (`now := time.Now()` then `now.Format(...)`)
and, in the oracle for the helper the sweep added, the very defect the sweep
was removing.
"""
import json
import pathlib
import sys

import pytest

REPO = pathlib.Path(__file__).resolve().parents[2]
# Imported the way every sibling checker's test module imports its subject, so
# `ty` sees a real module rather than the `ModuleSpec | None` an importlib
# round-trip hands it.
sys.path.insert(0, str(REPO / "scripts"))

import check_clock_test_flakiness as c  # noqa: E402


def findings(name, source):
    """Findings for a synthetic file, with no filesystem involved."""
    return c.findings_for(REPO / name, source=source)


def kinds(name, source):
    return [(f["symbol"], f["kind"]) for f in findings(name, source)]


# --------------------------------------------------------------------------
# Violations it must catch.
# --------------------------------------------------------------------------

def test_a_go_day_read_in_a_test_is_flagged():
    src = '''package server_test

import (
	"testing"
	"time"
)

func TestAudit(t *testing.T) {
	day := time.Now().UTC().Format("2006-01-02")
	_ = day
}
'''
    assert kinds("internal/server/audit_test.go", src) == [
        ("TestAudit", "day-granular-now")]


def test_the_two_statement_go_spelling_is_flagged():
    """THE SHAPE THE FIRST VERSION MISSED, and the reason the alias clause
    exists. Anchoring on one expression walks straight past the assignment that
    spells the identical defect across two lines -- which is R8's measured
    lesson in check_security_footguns.py about a bare name bound to
    `spark.sql`, one checker over."""
    src = '''package server_test

func TestAudit(t *testing.T) {
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	_ = day
}
'''
    assert kinds("internal/server/audit_test.go", src) == [
        ("TestAudit", "day-granular-now")]


def test_a_go_read_outside_any_func_is_named_module():
    src = '''package server_test

var today = time.Now().UTC().Format("2006-01-02")
'''
    assert kinds("internal/server/audit_test.go", src) == [
        ("<module>", "day-granular-now")]


def test_a_python_day_read_is_flagged():
    src = '''from datetime import UTC, datetime


def test_audit():
    day = datetime.now(UTC).strftime("%Y-%m-%d")
    assert day
'''
    assert kinds("python/tests/test_audit.py", src) == [
        ("test_audit", "day-granular-now")]


def test_the_two_statement_python_spelling_is_flagged():
    src = '''from datetime import UTC, datetime


def test_audit():
    now = datetime.now(UTC)
    assert now.strftime("%Y-%m-%d")
'''
    assert kinds("python/tests/test_audit.py", src) == [
        ("test_audit", "day-granular-now")]


def test_date_today_is_flagged():
    """`date.today()` has no format string to inspect -- it IS day-granular by
    construction, which is what makes it a second shape rather than a special
    case of the first."""
    src = '''import datetime as dt


def test_fresh():
    assert dt.date.today()
'''
    assert kinds("python/tests/test_fresh.py", src) == [
        ("test_fresh", "day-granular-now")]


def test_utcnow_is_flagged():
    src = '''from datetime import datetime


def test_audit():
    assert datetime.utcnow().strftime("%Y-%m-%d")
'''
    assert kinds("python/tests/test_audit.py", src) == [
        ("test_audit", "day-granular-now")]


def test_a_module_level_e2e_read_is_named_module():
    """The e2e harnesses are top-level scripts, so many reads have no enclosing
    function -- a position Go's half has almost none of. One `<module>` entry
    covers every module-level read in that file OF THAT KIND, the same
    many-to-one the Python sleep ledger already carries."""
    src = '''from datetime import UTC, datetime

DAY = datetime.now(UTC).strftime("%Y-%m-%d")
'''
    assert kinds("e2e/thing/driver.py", src) == [
        ("<module>", "day-granular-now")]


def test_the_outermost_function_names_a_nested_read():
    """Outermost, not innermost: a read inside a nested helper is named by the
    test containing it, which is both more useful to a reader and more stable
    than the helper's name."""
    src = '''from datetime import UTC, datetime


def test_audit():
    def window():
        return datetime.now(UTC).strftime("%Y-%m-%d")
    assert window()
'''
    assert kinds("python/tests/test_audit.py", src) == [
        ("test_audit", "day-granular-now")]


# --------------------------------------------------------------------------
# Near-misses it must NOT catch. These are what keep the checker something
# people fix rather than argue with.
# --------------------------------------------------------------------------

def test_a_full_timestamp_layout_is_not_day_granular():
    """The load-bearing discrimination. A layout carrying a time is not
    TRUNCATED, so there is no midnight boundary for it to straddle -- and
    e2e/az-rest/driver.py has two of these (the schedule's start/end bounds)
    that must stay silent."""
    go = '''package server_test

func TestStamp(t *testing.T) {
	at := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_ = at
}
'''
    assert findings("internal/server/stamp_test.go", go) == []
    # A DIRECT now-read, not an offset one. The first version of this test
    # used `(datetime.now(UTC) + timedelta(hours=1))`, which the checker
    # suppressed because the receiver was arithmetic rather than because the
    # layout had a time in it -- so the discrimination under test here was not
    # actually being asserted, and disabling it left this suite green.
    # Mutation-testing found that; the offset case is now its own test below.
    py = '''from datetime import UTC, datetime


def test_stamp():
    at = datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")
    assert at
'''
    assert findings("python/tests/test_stamp.py", py) == []
    # ...and the e2e schedule bounds that motivated the near-miss, in the
    # spelling e2e/az-rest/driver.py actually uses.
    offset = '''from datetime import UTC, datetime, timedelta


def test_schedule():
    start = (datetime.now(UTC) + timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    assert start
'''
    assert findings("python/tests/test_schedule.py", offset) == []


def test_a_day_granular_read_offset_by_a_timedelta_is_still_flagged():
    """"Yesterday" has the same midnight boundary as "today". The checker's
    first version looked only at a DIRECT receiver, so an offset day read
    reported green -- safe-looking right up until the offset is in days."""
    src = '''from datetime import UTC, datetime, timedelta


def test_window():
    day = (datetime.now(UTC) - timedelta(days=1)).strftime("%Y-%m-%d")
    assert day
'''
    assert kinds("python/tests/test_window.py", src) == [
        ("test_window", "day-granular-now")]


def test_a_time_only_layout_is_not_a_day_read():
    go = '''package server_test

func TestClock(t *testing.T) {
	_ = time.Now().UTC().Format("15:04:05")
}
'''
    assert findings("internal/server/clock_test.go", go) == []


def test_a_sas_expiry_offset_is_not_flagged():
    """e2e/azurite-shortcut/driver.py's three SAS expiries. A `timedelta`
    offset handed to an SDK is not formatted to a day at all, so it is not in
    this rule's scope and must not be swept in by proximity to `datetime.now`."""
    src = '''from datetime import UTC, datetime, timedelta

token = generate_blob_sas(expiry=datetime.now(UTC) + timedelta(hours=1))
'''
    assert findings("e2e/azurite-shortcut/driver.py", src) == []


def test_a_day_read_in_production_code_is_out_of_scope():
    """A day-granular read in the emulator is a product decision with its own
    tests, not a flaky assertion. The Go half scans `*_test.go` only, which is
    also what exempts the repair: internal/testsupport/clock.go reads the clock
    once on every caller's behalf, exactly as wait.go is exempt from the Go
    sleep checker."""
    src = '''package store

func Today() string {
	return time.Now().UTC().Format("2006-01-02")
}
'''
    assert findings("internal/store/activity.go", src) == []
    assert findings("internal/testsupport/clock.go", src) == []


def test_a_go_read_inside_a_comment_is_not_flagged():
    src = '''package server_test

// Was: day := time.Now().UTC().Format("2006-01-02")
/* and also time.Now().UTC().Format("2006-01-02") */
func TestAudit(t *testing.T) {}
'''
    assert findings("internal/server/audit_test.go", src) == []


def test_a_url_in_a_go_string_does_not_start_a_comment():
    """`"https://learn.microsoft.com/x"` contains `//`. The regex form of the
    comment strip reads the second half of every documentation URL in this tree
    as a comment, which is the mistake check_comment_drift.py records paying
    for -- so the strip is hand-walked over Go's four lexical states, and this
    is the test that holds it."""
    src = '''package server_test

func TestAudit(t *testing.T) {
	url := "https://learn.microsoft.com/x"
	day := time.Now().UTC().Format("2006-01-02")
	_, _ = url, day
}
'''
    assert kinds("internal/server/audit_test.go", src) == [
        ("TestAudit", "day-granular-now")]


def test_a_fake_clock_read_is_not_flagged():
    """Most of this tree advances an INJECTED clock (`st.Clock`), which is
    deterministic and the correct shape. Flagging it would make the checker
    something people argue with rather than fix, so the alias clause resolves
    names per file: only a name bound to `time.Now()` counts."""
    src = '''package server_test

func TestAudit(t *testing.T) {
	now := st.Clock.Now()
	day := now.Format("2006-01-02")
	_ = day
}
'''
    assert findings("internal/server/audit_test.go", src) == []


def test_a_non_clock_value_formatted_to_a_day_is_not_flagged():
    src = '''from datetime import UTC, datetime


def test_published():
    published = parse(response["createdAt"])
    assert published.strftime("%Y-%m-%d")
'''
    assert findings("python/tests/test_published.py", src) == []


def test_a_bare_now_call_is_not_flagged():
    """Unlike `time.sleep`, `now` is a name a great many objects legitimately
    have -- a fixture, a fake clock, a response wrapper -- so a bare `now()`
    with no `datetime` receiver is deliberately not matched."""
    src = '''def test_audit(clock):
    assert now().strftime("%Y-%m-%d")
'''
    assert findings("python/tests/test_audit.py", src) == []


# --------------------------------------------------------------------------
# The ledger, in both directions.
# --------------------------------------------------------------------------

def test_a_stale_ledger_entry_fails(tmp_path, monkeypatch, capsys):
    """The direction a 'flag what is new' reader would never think to check. A
    stale allowance is how a ban quietly stops applying, and unlike an
    unrecorded read it is green and silent."""
    ledger = tmp_path / "clock.json"
    ledger.write_text(json.dumps({"accepted": [{
        "file": "internal/server/gone_test.go", "symbol": "TestGone",
        "kind": "day-granular-now", "bucket": "verdict-day-independent",
        "why": "x"}]}), encoding="utf-8")
    monkeypatch.setattr(c, "LEDGER", ledger)
    assert c.main(["check_clock_test_flakiness.py", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "no longer flagged" in out


def test_the_ledger_key_names_the_kind():
    """ASSERTED ON THE KEY'S SHAPE, not inferred from behaviour, and
    mutation-testing is what forced that. This checker defines ONE kind, so
    `file:symbol` and `file:symbol:kind` partition today's findings
    identically -- dropping the kind from the key left this whole suite green.
    Which is exactly the position check_python_test_flakiness.py was in when it
    shipped its kind-blind key, and it stayed harmless there until the ledger
    had two kinds in it, by which point 42 of 44 recorded symbols were exempt
    from the bans the ledger existed to enforce. The only assertion that can
    see it before a second kind exists is this one."""
    assert c.ledger_key({"file": "a_test.go", "symbol": "TestX",
                         "kind": "day-granular-now"}) == \
        "a_test.go:TestX:day-granular-now"


def test_an_entry_with_no_kind_is_refused_by_name():
    """Not silently accepted as every kind. check_python_test_flakiness.py
    records the key being spelled inline in three places with the kind missing
    from two, which exempted 42 of 44 recorded symbols from the bans the ledger
    existed to enforce. This checker has ONE kind today, so a kind-less key
    would be harmless right now -- which is exactly when it gets written."""
    with pytest.raises(KeyError, match="missing kind"):
        c.ledger_key({"file": "a_test.go", "symbol": "TestX"})
    with pytest.raises(KeyError, match="missing symbol"):
        c.ledger_key({"file": "a_test.go", "kind": "day-granular-now"})


def test_every_ledger_entry_carries_a_bucket_and_a_reason():
    """An exemption with no reason is an omission wearing a decision's
    clothes."""
    data = json.loads((REPO / "docs" / "clock-test-flakiness.json")
                      .read_text(encoding="utf-8"))
    assert data["accepted"], "an empty ledger would make the both-directions check vacuous"
    for entry in data["accepted"]:
        assert entry.get("bucket"), entry
        assert len(entry.get("why", "")) > 40, entry


def test_the_ledger_records_no_repaired_site():
    """The two sites the sweep REPAIRED must not appear. They are absent
    because they no longer read the clock themselves -- both call a named
    straddle-aware helper -- and an entry for either would turn a closed gap
    into an accepted one, which is the shape docs/10-testing.md names as the
    worse half of a ledger."""
    data = json.loads((REPO / "docs" / "clock-test-flakiness.json")
                      .read_text(encoding="utf-8"))
    keys = {c.ledger_key(e) for e in data["accepted"]}
    for repaired in (
        "internal/server/activityevents_test.go:TestActivityEventsRecordsRealOperations:day-granular-now",
        "internal/server/activityevents_test.go:TestActivityEventsPagination:day-granular-now",
        "internal/server/labels_test.go:TestLabelChangesAuditWithDocumentedSchema:day-granular-now",
        "e2e/az-rest/driver.py:driver:day-granular-now",
    ):
        assert repaired not in keys, f"{repaired} is repaired, not accepted"


# --------------------------------------------------------------------------
# Separators, the one defect that escaped the Go sibling's own test suite.
# --------------------------------------------------------------------------

def test_the_ledger_key_is_separator_independent():
    """THE GO SIBLING'S WORST RECORDED BUG, driven here so it cannot recur
    silently. `str(path.relative_to(ROOT))` spells a nested path with the
    HOST's separator, so on Windows nothing matched and BOTH directions of the
    both-directions check fired at once -- every real site read as unrecorded
    and every real entry as stale -- taking two CI legs red while every POSIX
    leg stayed green. Every test in that checker's suite passed on a POSIX
    runner whether or not the defect was present, so a suite written to drive
    it in both directions could not see a defect that broke both directions.
    Reverting `as_posix()` to `str()` fails this on darwin."""
    win = pathlib.PureWindowsPath(r"C:\repo\internal\server\audit_test.go")
    assert c.relkey(win, pathlib.PureWindowsPath(r"C:\repo")) == \
        "internal/server/audit_test.go"
    posix = pathlib.PurePosixPath("/repo/internal/server/audit_test.go")
    assert c.relkey(posix, pathlib.PurePosixPath("/repo")) == \
        "internal/server/audit_test.go"


def test_findings_on_a_windows_path_use_posix_keys():
    """The same normalisation where it is actually consumed. `relkey` being
    right is not enough if `findings_for` spells the key some other way --
    which is how the Go sibling's defect survived a correct helper."""
    src = '''package server_test

func TestAudit(t *testing.T) {
	_ = time.Now().UTC().Format("2006-01-02")
}
'''
    win = pathlib.PureWindowsPath(str(REPO).replace("/", "\\")) \
        if "\\" in str(REPO) else REPO
    got = c.findings_for(pathlib.PurePath(REPO) / "internal" / "server" / "a_test.go",
                         source=src)
    assert got[0]["file"] == "internal/server/a_test.go"
    assert "\\" not in got[0]["file"]
    assert win is not None


# --------------------------------------------------------------------------
# Vacuity guards: a sweep that inspected nothing must not report success.
# --------------------------------------------------------------------------

def test_a_renamed_scan_root_fails(monkeypatch, capsys):
    """All-or-nothing vacuity is not enough and this sweep is not
    all-or-nothing: `rglob` on a missing path yields nothing and raises
    nothing, so a renamed root subtracts its whole share in silence while the
    remaining roots keep the file count comfortably non-zero. `e2e` holds one
    of the two measured defects."""
    monkeypatch.setattr(c, "SCAN_ROOTS", (*c.SCAN_ROOTS, "e2e-renamed-away"))
    assert c.main(["check_clock_test_flakiness.py", "--strict"]) == 1
    assert "do not exist" in capsys.readouterr().out


def test_an_unparseable_python_file_is_reported_not_skipped():
    """A file this interpreter cannot parse was NOT inspected, and an
    uninspected file reads exactly like a clean one."""
    got = findings("e2e/thing/driver.py", "def broken(:\n")
    assert len(got) == 1
    assert "not inspected" in got[0]["snippet"]


# --------------------------------------------------------------------------
# The real repository, as the closing control. A passing synthetic suite can
# drift away from the tree it claims to describe.
# --------------------------------------------------------------------------

def test_the_real_tree_passes_strict():
    assert c.main(["check_clock_test_flakiness.py", "--strict"]) == 0


def test_the_real_tree_is_walked_at_all():
    files = c.scanned_files()
    assert len(files) > 300, f"walked {len(files)} files; the sweep is broken"
    rel = {c.relkey(p) for p in files}
    assert "internal/server/activityevents_test.go" in rel
    assert "e2e/az-rest/driver.py" in rel
    # The repair's home is NOT walked, by construction rather than by luck.
    assert "internal/testsupport/clock.go" not in rel


def test_the_repaired_go_site_no_longer_reads_the_clock():
    """Against the file on disk, not a transcription. This is what would fail
    if the repair were reverted: the fixed test reports clean today, so losing
    the straddle handling is an unrecorded finding at exit 1."""
    path = REPO / "internal" / "server" / "activityevents_test.go"
    got = {f["symbol"] for f in c.findings_for(path)}
    assert "TestActivityEventsRecordsRealOperations" not in got
    assert "TestActivityEventsPagination" not in got
    # ...and the inert sibling in the same file still is one, so the file is
    # not passing merely because nothing in it is being read.
    assert got == {"TestActivityEventsRequestValidation"}


def test_the_repaired_python_site_no_longer_reads_the_clock_in_driver():
    path = REPO / "e2e" / "az-rest" / "driver.py"
    got = {f["symbol"] for f in c.findings_for(path)}
    assert got == {"utc_day"}, got


def test_report_mode_prints_both_directions_and_exits_zero(capsys):
    assert c.main(["check_clock_test_flakiness.py"]) == 0
    out = capsys.readouterr().out
    assert "accepted" in out
    assert "day-granular clock read" in out
