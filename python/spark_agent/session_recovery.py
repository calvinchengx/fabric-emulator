"""Recognise an engine session this agent can no longer use, and rebind after it.

WHY THIS EXISTS (issue #312). The agent builds its `SparkSession` once, at
import, and holds it for the life of the process — that is the design, one
long-lived session behind many Livy namespaces. If the ENGINE drops that
session, nothing re-runs `getOrCreate()`, so every statement from then on fails
with

    IllegalArgumentException: invalid argument: session <uuid> is not running

and keeps failing until the container is restarted. The failure is permanent for
a reason that has nothing to do with the statement being run, which is why it
reads as "the agent broke" rather than "the engine went away".

WHAT THIS DELIBERATELY DOES NOT DO: re-run the statement. A cell that wrote data
and then met the dead session on a later line would write twice on a silent
retry, and the agent cannot tell the two halves apart — it hands `exec()` a block
of arbitrary user code, not a plan it can inspect for side effects. So recovery
makes the NEXT statement work and says plainly that this one did not, which is
also what a real Livy session does when its Spark application dies.

AND IT CANNOT BE SILENT, because rebinding is not free: temp views, cached
DataFrames and any session-scoped conf lived in the engine session that went
away. A "transparent" reconnect hands the user a notebook that has quietly
forgotten its temp views — the same failure wearing a friendlier face, and
harder to diagnose than the error it replaced. The caller reports what was lost.
"""
import re

# Substrings, lowercased, that mean THIS CLIENT'S SESSION is gone rather than
# the statement being wrong. Kept as markers rather than exception types because
# the two engines this image is pointed at disagree about the type: Sail raises
# pyspark's IllegalArgumentException with the message above, while classic
# Spark Connect raises SparkConnectGrpcException carrying an INVALID_HANDLE
# error class. Matching the type alone would recover on one engine and not the
# other, with nothing to say which.
# Each entry is a tuple of substrings that must ALL appear. "is not running" is
# on its own far too loose to trigger a rebuild — "the container is not
# running", "the Docker daemon is not running" and any engine-side message about
# something else that stopped would all match, and every one of them would cost
# every open notebook its temp views. Sail's actual text is
# "invalid argument: session <uuid> is not running", so pair it with "session".
LOST_SESSION_MARKERS = (
    ("session", "is not running"),
    ("invalid_handle.session_closed",),
    ("invalid_handle.session_not_found",),
    ("session_not_found",),
    ("session is closed",),
)

# There is deliberately NO deny-list beside this. I wrote one first — to keep
# `INVALID_HANDLE.SESSION_ALREADY_EXISTS` (the engine refusing to CREATE a
# session) from triggering a rebuild that would meet it again — and then
# mutation-tested it: deleting the whole guard failed no test, because none of
# the markers above matches that message anyway. A branch that cannot change an
# outcome is not defence, it is something for a later reader to maintain and
# believe in. The test asserting ALREADY_EXISTS is not treated as a lost session
# stays, and now guards the marker list itself against being widened to
# something as loose as "session".

def is_lost_session_text(text):
    """Does this error TEXT mean the engine no longer has this client's session?

    Text rather than an exception, because the agent has two statement paths —
    Python (`run_code`) and SQL (`run_sql`) — and both convert the exception to
    an error envelope before anyone can look at it. Reading the rendered error
    covers both; asking for the exception object would have covered whichever
    one I happened to edit.

    Reads the message, not the type — see LOST_SESSION_MARKERS. Anything not
    recognisably a lost session must answer False: recovering from an ordinary
    user error would drop every namespace's temp views for a typo.
    """
    text = (text or "").lower()
    return any(all(part in text for part in markers) for markers in LOST_SESSION_MARKERS)


def is_lost_session(exc):
    """`is_lost_session_text` for an exception object."""
    return is_lost_session_text(f"{type(exc).__name__}: {exc}")


def envelope_is_lost_session(result):
    """Same question, asked of a statement result envelope.

    Both statement paths return `{"status": "error", "evalue": ..., "traceback":
    [...]}`. The marker can be in either field: `evalue` is only the LAST
    traceback line, and a lost session raised inside library code puts the
    message further up.
    """
    if not isinstance(result, dict) or result.get("status") != "error":
        return False
    parts = [str(result.get("evalue") or "")]
    tb = result.get("traceback") or []
    if isinstance(tb, (list, tuple)):
        parts.extend(str(line) for line in tb)
    return is_lost_session_text("\n".join(parts))


# A table that WAS there and now is not. Distinct from LOST_SESSION_MARKERS
# because the engine does not report this as a lost session at all -- see
# forgotten_table_in below.
FORGOTTEN_TABLE_MARKERS = (
    ("table_or_view_not_found",),
    ("table or view not found",),
    ("table not found",),
)

# The name inside those messages, and there is more than one shape. MEASURED,
# not recalled -- an earlier version of this matched one pattern against a
# half-remembered message and silently extracted the word "The":
#
#   sail   AnalysisException: Table not found: [TABLE_OR_VIEW_NOT_FOUND]
#          Table or view not found: events
#   spark  [TABLE_OR_VIEW_NOT_FOUND] The table or view `events` cannot be found.
#
# So CANDIDATES are collected rather than a single name parsed. Every plausible
# identifier in the message is offered to `was_registered`, and the registry
# decides. That is safe precisely because the registry is the real test: a
# message full of ordinary words yields no registered name and nothing fires.
_NAME_AFTER_NOT_FOUND = re.compile(r"not\s+found\s*:\s*[`'\"]?([\w.]+)", re.IGNORECASE)
_QUOTED_NAME = re.compile(r"[`'\"]([\w.]+)[`'\"]")


def _candidate_names(text):
    """Every identifier in `text` that could be the table, most likely first."""
    seen, out = set(), []
    for pattern in (_NAME_AFTER_NOT_FOUND, _QUOTED_NAME):
        for name in pattern.findall(text):
            key = name.lower()
            if key not in seen:
                seen.add(key)
                out.append(name)
    return out


def forgotten_table_in(result, was_registered):
    """The name of a table this agent registered that the engine has forgotten.

    WHY THIS EXISTS, AND WHY IT IS NOT AN ERROR-TEXT MATCH ALONE. Sail's
    credential refresh is a process restart (`docker/sail/launcher.py`): the
    Storage bearer only enters sail through startup env, so re-minting it needs
    a new process, and object_store reads that env exactly once -- docs/20
    records this as "the one thing the Sail side cannot do". The restart
    discards the engine's session state, and sail then re-creates the session
    under the SAME id on the next statement, so nothing ever reports the session
    as not running and NONE of LOST_SESSION_MARKERS fires. The client is simply
    told its table does not exist.

    That is indistinguishable from a typo, which is exactly the failure this
    module exists to prevent: an unattributed loss is worse than a loud one. So
    the text match is only half the test. The other half is `was_registered` --
    the agent asking its OWN records whether it put that table in the catalog.
    A typo answers no and is left completely alone; a table the agent registered
    and the engine has forgotten answers yes, and only that pair means the
    engine went away underneath this session.

    Returns the table name, or None.
    """
    if not isinstance(result, dict) or result.get("status") != "error":
        return None
    parts = [str(result.get("evalue") or "")]
    tb = result.get("traceback") or []
    if isinstance(tb, (list, tuple)):
        parts.extend(str(line) for line in tb)
    text = "\n".join(parts)
    low = text.lower()
    if not any(all(part in low for part in markers)
               for markers in FORGOTTEN_TABLE_MARKERS):
        return None
    for name in _candidate_names(text):
        try:
            if was_registered(name):
                return name
        except Exception:  # noqa: BLE001 - a registry that cannot answer is not evidence
            return None
    return None


def table_exists_somewhere(name, *, recorded_location, declared_tables, in_storage):
    """Is `name` a table that EXISTS, which the engine has nonetheless lost?

    The oracle half of `forgotten_table_in`, and the half that keeps a typo a
    typo. It lives here rather than in agent.py because agent.py needs a Spark
    session to import and is therefore omitted from the coverage gate — this
    decides whether a user is told "the engine restarted" or left reading
    "table not found" as their own mistake, which is far too load-bearing to be
    reachable only through an end-to-end run. Same reasoning that put catalog
    registration and jvm conf in their own modules.

    THREE ORACLES, cheapest first, any one sufficient:

      1. `recorded_location(name)` — delta_ops has a location for it, recorded
         or derived earlier in this process;
      2. `declared_tables()` — the control plane named it in a /register
         payload;
      3. `in_storage(name)` — THE LAKEHOUSE ITSELF has it.

    THE THIRD IS THE ONE THAT MATTERS, and leaving it out made a first draft of
    this miss the exact case it was written for. A notebook's
    `saveAsTable("events")` on a fresh lakehouse is in NEITHER of the first
    two: `register()` enumerated the lakehouse before the write happened, and a
    DataFrameWriter call is not a statement the agent's `sql` wrapper ever
    sees. So it answered "no" for precisely the table whose disappearance
    prompted the fix.

    Asking storage is also the semantically right question. "The table is in
    the lakehouse and the engine cannot see it" IS the forgotten-session
    condition, stated directly rather than inferred from bookkeeping. A typo is
    not in the lakehouse either, so it still answers no.

    Every oracle is allowed to fail. A registry that raises is not evidence
    either way, and treating an unreadable one as "absent" would turn a
    credential problem into a silent typo verdict.
    """
    bare = (name or "").replace("`", "").strip().lower().rsplit(".", 1)[-1]
    if not bare:
        return False
    try:
        if recorded_location(bare):
            return True
    except Exception:  # noqa: BLE001 - a registry that cannot answer is not evidence
        pass
    try:
        for declared in declared_tables():
            if str(declared or "").strip().lower() == bare:
                return True
    except Exception:  # noqa: BLE001
        pass
    try:
        return bool(in_storage(bare))
    except Exception:  # noqa: BLE001 - unreadable storage is not evidence
        return False


def forgotten_table_note(name, reregistered):
    """What to tell the user after re-establishing a forgotten session.

    NAMES WHAT WAS NOT RECOVERED, deliberately. Catalog registrations can be
    rebuilt from the lakehouse; temp views, cached DataFrames and session-scoped
    conf lived in the engine process that went away and cannot be. Reporting
    only the half that worked would hand back a notebook that looks whole and is
    not -- the "friendlier face" this module's header refuses.
    """
    head = (f"[recovered] the engine restarted to refresh its Storage credential, "
            f"which discards session state, so {name!r} was missing from the "
            f"catalog rather than from the lakehouse.")
    if reregistered:
        head += f" {reregistered} table(s) have been re-registered."
    return (head + " Temp views, cached DataFrames and session-scoped conf did NOT "
            "survive and must be recreated. This statement did not run; re-run it.")


def rebind(namespaces, new_spark, isolate, attach_sc=None, on_route=None):
    """Point every live namespace at `new_spark`, returning the sessions rebound.

    `on_route(session, route)` is called with each rebound session's NEW
    isolation route. A recovered session can land on a different route than it
    started on -- the engine it reconnects to is not required to be the engine
    it left -- and OneLake security reads that route to decide whether editing
    the catalog is safe. A stale route recorded as the private one would let a
    shared-catalog session be reshaped, which is the leaking direction, so the
    fresh value is pushed out rather than left to expire.

    The per-Livy-session objects are derived from the shared one (`newSession`),
    so when the shared session dies they all die with it — rebinding only the
    module-level handle would leave every existing notebook holding the corpse.

    User variables are left alone. A plain Python value is still valid across an
    engine restart; only the Spark-derived bindings are not, and clearing the
    namespace wholesale would throw away work the engine never held.
    """
    rebound = []
    for session, g in namespaces.items():
        if "spark" not in g:
            continue
        session_spark, route = isolate(new_spark)
        if on_route is not None:
            on_route(session, route)
        g["spark"] = session_spark
        if attach_sc is not None:
            try:
                g["sc"] = attach_sc(session_spark)
            except Exception:  # noqa: BLE001 — an engine without sparkContext keeps the old facade
                pass
        rebound.append(session)
    return rebound


def note(rebound):
    """The line the user sees. Names the loss, because rebinding is not free."""
    return ("agent: the engine dropped this Spark session; rebuilt it and "
            f"rebound {len(rebound)} notebook session(s). Temp views, cached "
            "DataFrames and session-scoped conf from before the drop are gone. "
            "Re-run this cell.")


def annotate(result, note):
    """Append the recovery note to a statement's error envelope, in place.

    Both fields, because clients read different ones: the Livy layer surfaces
    `evalue` in the job's error, while a notebook shows the traceback.
    """
    if not note or not isinstance(result, dict):
        return result
    result["evalue"] = f"{result.get('evalue', '')}\n{note}".strip()
    result["traceback"] = [*list(result.get("traceback") or []), note]
    return result
