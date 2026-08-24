"""A dropped engine session is recognised, and rebinding reaches every namespace.

agent.py calls getOrCreate() at import, so the logic lives in
session_recovery.py — the same split as catalog.py and jvmconf.py. Issue #312:
the agent held one session for the process lifetime and nothing re-ran
getOrCreate(), so once the engine dropped it every later statement failed with
`session <uuid> is not running` until the container restarted.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import session_recovery  # noqa: E402


class FakeSpark:
    """Just enough to be told apart from the session that died."""

    def __init__(self, tag):
        self.tag = tag


def isolate(shared):
    return FakeSpark(f"isolated-of-{shared.tag}"), True


# --- recognising the failure -------------------------------------------------

def test_sails_dead_session_message_is_recognised():
    # The exact text from the issue, which is what Sail raises.
    err = ("pyspark.errors.exceptions.connect.IllegalArgumentException: "
           "invalid argument: session 7f3a1c22-0a1e-4f7e-9f0b-4a2f1d9a0c11 is not running")
    assert session_recovery.is_lost_session_text(err)


def test_spark_connects_error_classes_are_recognised():
    # Classic Spark Connect words it as an error class, not a sentence. Both
    # engines are behind this one image, so both spellings have to match.
    for text in ("[INVALID_HANDLE.SESSION_CLOSED] The handle is already closed",
                 "[INVALID_HANDLE.SESSION_NOT_FOUND] session not found"):
        assert session_recovery.is_lost_session_text(text), text


def test_an_ordinary_user_error_is_not_a_lost_session():
    # THE NEGATIVE HALF, and the one that matters: recovering here would drop
    # every namespace's temp views because somebody mistyped a column.
    for text in ("AnalysisException: [UNRESOLVED_COLUMN] A column named `nmae` cannot be resolved",
                 "SyntaxError: invalid syntax",
                 "ZeroDivisionError: division by zero"):
        assert not session_recovery.is_lost_session_text(text), text


def test_session_already_exists_is_not_a_lost_session():
    # Looks like the others and is the opposite: the engine refusing to CREATE
    # a session. A rebuild meets it again immediately, so treating it as a lost
    # session turns one clear error into a loop.
    assert not session_recovery.is_lost_session_text(
        "[INVALID_HANDLE.SESSION_ALREADY_EXISTS] session already exists")


def test_empty_and_none_are_not_a_lost_session():
    assert not session_recovery.is_lost_session_text("")
    assert not session_recovery.is_lost_session_text(None)


def test_is_lost_session_reads_an_exception_object():
    class IllegalArgumentException(Exception):
        pass

    exc = IllegalArgumentException("invalid argument: session abc is not running")
    assert session_recovery.is_lost_session(exc)
    assert not session_recovery.is_lost_session(ValueError("nope"))


# --- recognising it in a statement envelope ----------------------------------

def test_the_marker_is_found_in_the_traceback_not_only_in_evalue():
    # `evalue` is only the LAST traceback line. A lost session raised inside
    # library code puts the message further up, so an evalue-only check would
    # miss exactly the case this exists for.
    result = {
        "status": "error",
        "evalue": "  File \"/usr/lib/python3/pyspark/sql/connect/client.py\", line 1",
        "traceback": [
            "Traceback (most recent call last):",
            "pyspark.errors.exceptions.connect.IllegalArgumentException: "
            "invalid argument: session abc is not running",
            "  File \"/usr/lib/python3/pyspark/sql/connect/client.py\", line 1",
        ],
    }
    assert session_recovery.envelope_is_lost_session(result)


def test_a_successful_statement_is_never_a_lost_session():
    ok = {"status": "ok", "data": {"text/plain": "session is not running"}}
    assert not session_recovery.envelope_is_lost_session(ok)
    assert not session_recovery.envelope_is_lost_session(None)


# --- rebinding ---------------------------------------------------------------

def test_every_namespace_is_rebound_not_just_the_shared_handle():
    # The per-session objects are derived from the shared one, so they die with
    # it. Rebinding only the module-level handle leaves every open notebook
    # holding the corpse — which is the bug, one level down.
    dead = FakeSpark("dead")
    namespaces = {
        "s1": {"spark": FakeSpark("isolated-of-dead"), "x": 1},
        "s2": {"spark": FakeSpark("isolated-of-dead")},
    }
    fresh = FakeSpark("fresh")
    rebound = session_recovery.rebind(namespaces, fresh, isolate)
    assert sorted(rebound) == ["s1", "s2"]
    for g in namespaces.values():
        assert g["spark"].tag == "isolated-of-fresh"
    assert dead.tag == "dead"  # untouched, just no longer referenced


def test_user_variables_survive_the_rebind():
    # A plain Python value is still valid across an engine restart; only the
    # Spark-derived bindings are not. Clearing the namespace would throw away
    # work the engine never held.
    namespaces = {"s1": {"spark": FakeSpark("old"), "df_count": 42, "name": "contoso"}}
    session_recovery.rebind(namespaces, FakeSpark("fresh"), isolate)
    assert namespaces["s1"]["df_count"] == 42
    assert namespaces["s1"]["name"] == "contoso"


def test_sc_is_rebound_when_an_attach_is_supplied():
    namespaces = {"s1": {"spark": FakeSpark("old"), "sc": "old-sc"}}
    session_recovery.rebind(namespaces, FakeSpark("fresh"), isolate,
                            attach_sc=lambda s: f"sc-of-{s.tag}")
    assert namespaces["s1"]["sc"] == "sc-of-isolated-of-fresh"


def test_a_failing_sc_attach_does_not_lose_the_session_rebind():
    # An engine without sparkContext must not cost us the spark rebind, which
    # is the part that fixes the bug.
    namespaces = {"s1": {"spark": FakeSpark("old"), "sc": "old-sc"}}

    def boom(_s):
        raise RuntimeError("no sparkContext on this engine")

    rebound = session_recovery.rebind(namespaces, FakeSpark("fresh"), isolate, attach_sc=boom)
    assert rebound == ["s1"]
    assert namespaces["s1"]["spark"].tag == "isolated-of-fresh"
    assert namespaces["s1"]["sc"] == "old-sc"


def test_a_namespace_without_spark_is_skipped():
    namespaces = {"s1": {"x": 1}}
    assert session_recovery.rebind(namespaces, FakeSpark("fresh"), isolate) == []


def test_the_note_names_what_the_rebuild_cost():
    # Silence here would hand the user a notebook that quietly forgot its temp
    # views. The note is the whole reason recovery is allowed to be automatic.
    note = session_recovery.note(["s1", "s2"])
    assert "2 notebook session(s)" in note
    assert "Temp views" in note
    assert "Re-run this cell" in note


def test_the_note_is_appended_to_both_fields_a_client_might_read():
    # The Livy layer surfaces `evalue`; a notebook shows the traceback. Writing
    # only one leaves half the clients with the bare `is not running`.
    result = {"status": "error", "evalue": "boom", "traceback": ["Traceback", "boom"]}
    session_recovery.annotate(result, "rebuilt")
    assert result["evalue"] == "boom\nrebuilt"
    assert result["traceback"][-1] == "rebuilt"


def test_annotate_is_a_no_op_when_the_rebuild_failed():
    # recover_lost_session() returns None when it could not rebuild. Reporting a
    # recovery that did not happen is worse than the failure it replaces.
    result = {"status": "error", "evalue": "boom", "traceback": ["boom"]}
    session_recovery.annotate(result, None)
    assert result["evalue"] == "boom"
    assert result["traceback"] == ["boom"]


def test_something_else_that_is_not_running_is_not_a_lost_session():
    # "is not running" alone is far too loose: these are messages about
    # something ELSE having stopped, and recovering on them would cost every
    # open notebook its temp views for an unrelated failure.
    for text in ("RuntimeError: the container is not running",
                 "docker: Error response from daemon: the Docker daemon is not running",
                 "OSError: the mount helper is not running"):
        assert not session_recovery.is_lost_session_text(text), text


def test_sails_message_still_matches_with_the_tightened_marker():
    assert session_recovery.is_lost_session_text(
        "invalid argument: session 7f3a1c22 is not running")


# ---------------------------------------------------------------------------
# A table the engine FORGOT, which is a different event wearing the same red.
#
# Sail's credential refresh restarts the engine, the restart discards session
# state, and sail re-creates the session under the same id — so none of
# LOST_SESSION_MARKERS fires and the client is just told its table is missing.
# Both messages below are MEASURED, not invented: the sail one came from the
# conformance contract-7 probe, which records it verbatim every run.

SAIL_FORGOT = ("AnalysisException: Table not found: [TABLE_OR_VIEW_NOT_FOUND] "
               "Table or view not found: events")
SPARK_FORGOT = "[TABLE_OR_VIEW_NOT_FOUND] The table or view `events` cannot be found."


def _err(evalue, traceback=()):
    return {"status": "error", "evalue": evalue, "traceback": list(traceback)}


def _registered(*names):
    lowered = {n.lower() for n in names}
    return lambda n: n.lower() in lowered


def test_a_forgotten_table_is_recognised_on_sail():
    assert session_recovery.forgotten_table_in(_err(SAIL_FORGOT), _registered("events")) == "events"


def test_a_forgotten_table_is_recognised_on_spark():
    """Different wording, same event — the name is backticked rather than
    trailing a colon, which one regex silently got wrong."""
    assert session_recovery.forgotten_table_in(_err(SPARK_FORGOT), _registered("events")) == "events"


def test_a_typo_is_left_completely_alone():
    """THE GUARD. Text alone cannot tell a forgotten table from a misspelt one,
    so the registry is the real test — and recovering from a typo would drop
    every namespace's temp views for a slip of the finger."""
    assert session_recovery.forgotten_table_in(_err(SAIL_FORGOT), _registered("orders")) is None


def test_an_unrelated_error_is_not_a_forgotten_table():
    assert session_recovery.forgotten_table_in(
        _err("ZeroDivisionError: division by zero"), _registered("events")) is None


def test_a_successful_result_is_never_a_forgotten_table():
    assert session_recovery.forgotten_table_in({"status": "ok"}, _registered("events")) is None


def test_the_marker_can_live_in_the_traceback_not_only_evalue():
    """Same reason envelope_is_lost_session reads both: evalue is only the last
    line, and a failure raised inside library code puts the message further up."""
    assert session_recovery.forgotten_table_in(
        _err("RuntimeError: statement failed", traceback=[SAIL_FORGOT]),
        _registered("events")) == "events"


def test_a_registry_that_raises_is_not_treated_as_evidence():
    def boom(_name):
        raise RuntimeError("registry unavailable")

    assert session_recovery.forgotten_table_in(_err(SAIL_FORGOT), boom) is None


def test_a_lost_session_is_not_also_reported_as_a_forgotten_table():
    """The two paths must not both fire: a genuinely lost session already has a
    recovery that rebuilds the whole engine handle."""
    lost = _err("IllegalArgumentException: invalid argument: session abc is not running")
    assert session_recovery.envelope_is_lost_session(lost)
    assert session_recovery.forgotten_table_in(lost, _registered("events")) is None


def test_the_note_names_what_did_not_survive():
    """Reporting only the half that worked hands back a notebook that looks
    whole and is not — the 'friendlier face' this module refuses."""
    note = session_recovery.forgotten_table_note("events", 3)
    assert "restarted to refresh" in note
    assert "3 table(s) have been re-registered" in note
    assert "Temp views" in note and "did NOT" in note
    assert "re-run it" in note


def test_the_note_still_names_the_cause_when_nothing_could_be_re_registered():
    note = session_recovery.forgotten_table_note("events", None)
    assert "restarted to refresh" in note
    assert "re-registered" not in note
