"""The freshness bound on contract 3's hand-maintained floor.

Contract 3's comparison is monotone: an upgrade keeps passing, so the contract
will never go red on its own if Microsoft raises the runtime floor. There is no
machine-readable oracle to vendor the way contracts 2 and 8 have one, so the
claim is bounded in time instead — and a time bound that is never exercised is
just a comment with a date in it.

These pin the boundary rather than the happy path: the day it stops being fresh
must be a failure, and provenance that is missing or malformed must be too.
"""
import datetime as dt
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_runtime_floor_freshness as c  # noqa: E402

TODAY = dt.date(2026, 9, 1)


def _rt(**over):
    base = {"python": "3.11", "source": "https://learn.microsoft.com/x",
            "read": "2026-08-18"}
    base.update(over)
    return {"1.3": base}


def test_a_recent_reading_passes():
    assert c.problems(_rt(), TODAY) == []


def test_the_day_after_the_limit_fails():
    """The boundary itself, because an off-by-one here means the bound never
    fires or fires a day early forever."""
    read = TODAY - dt.timedelta(days=c.MAX_AGE_DAYS + 1)
    found = c.problems(_rt(read=read.isoformat()), TODAY)
    assert found and "last read" in found[0]


def test_exactly_at_the_limit_still_passes():
    read = TODAY - dt.timedelta(days=c.MAX_AGE_DAYS)
    assert c.problems(_rt(read=read.isoformat()), TODAY) == []


def test_the_failure_names_the_page_and_the_floor_to_re_check():
    """A stale-date failure is useless without saying what to go and read."""
    read = TODAY - dt.timedelta(days=400)
    msg = c.problems(_rt(read=read.isoformat()), TODAY)[0]
    assert "learn.microsoft.com" in msg
    assert "3.11" in msg


def test_a_missing_source_fails():
    assert any("no `source`" in p for p in c.problems(_rt(source=""), TODAY))


def test_a_malformed_date_fails_rather_than_being_treated_as_fresh():
    """`read: "soon"` must not sail through — the parse would raise, and a
    check that raises on bad data is not the same as one that reports it."""
    found = c.problems(_rt(read="soon"), TODAY)
    assert found and "not an ISO date" in found[0]


def test_no_runtimes_at_all_is_a_failure():
    assert c.problems({}, TODAY)


def test_the_repositorys_own_file_parses_and_is_currently_fresh():
    """Against the real file, not a fixture — this is the one that will start
    failing when the reading ages out, which is the point."""
    runtimes = c.entries(c.RUNTIMES.read_text(encoding="utf-8"))
    assert "1.3" in runtimes
    assert c.problems(runtimes, dt.date.today()) == []


def test_main_reports_and_returns_zero_on_the_real_tree(capsys):
    assert c.main() == 0
    assert "citing a page and a reading date" in capsys.readouterr().out


def test_main_returns_one_when_a_reading_has_aged_out(monkeypatch, capsys):
    monkeypatch.setattr(c, "MAX_AGE_DAYS", -1)
    assert c.main() == 1
    assert "last read" in capsys.readouterr().err


def test_a_missing_runtimes_file_is_reported_not_raised(monkeypatch, tmp_path, capsys):
    """The file could be renamed or dropped. That must be a stated failure, not
    a traceback — a check that crashes reads as broken tooling rather than as
    the finding it is."""
    monkeypatch.setattr(c, "RUNTIMES", tmp_path / "gone.json")
    assert c.main() == 1
    assert "is missing" in capsys.readouterr().err
