"""Per-Livy-session catalog registration, isolated so sessions cannot collide.

WHY THIS IS ITS OWN MODULE. The agent holds ONE long-lived SparkSession and
serves concurrent requests from a ThreadingHTTPServer, so anything this code
sets on that session is process-wide. Two notebooks bound to different
lakehouses would both call `setCurrentDatabase`, and the loser's unqualified
`spark.table("customers")` silently resolved against the OTHER lakehouse — a
wrong answer, not an error, in the one place a caller cannot see it.

That is the same defect class as the `__nb_exit__` prelude race documented in
internal/api/notebookdrive.go: one shared object, per-session meaning, last
writer wins. It is reachable today through plain concurrent Livy sessions and
needs no notebook DAG to trigger.

The fix is a real per-session SparkSession (`newSession()`), which gives each
Livy session its own current database and temp views over the same engine.
Where an engine does not implement it, isolation is impossible and the
fallback DETECTS the collision and says so, because degraded-and-loud beats
silently-wrong. Split out of agent.py so it is importable without starting
Spark: agent.py calls `getOrCreate()` at import, so nothing there can be unit
tested.
"""
import os
import traceback

# How a session was isolated, and — the part that matters to OneLake security —
# whether it got a catalog of its OWN.
#
# Measured (e2e/onelake-security-bypass): a `builder.create()` session on Sail
# starts with an EMPTY catalog and sees nothing the parent registered, so
# reshaping it touches nobody else. `newSession()` is the opposite by design:
# the JVM docs give it its own temp views and current database over the SAME
# catalog, so a DROP there is a DROP for every session on that engine.
#
# That difference decides whether securing a session by editing its catalog is
# enforcement or vandalism, so it is recorded rather than inferred at the point
# of use. The JVM entry is the conservative reading: it has not been measured
# here, and being wrong about it costs a refusal, never a leak.
ROUTE_NEW_SESSION = "newSession"
ROUTE_CONNECT = "connect"
ROUTE_SHARED = ""

CATALOG_IS_PRIVATE = {
    ROUTE_CONNECT: True,
    ROUTE_NEW_SESSION: False,
    ROUTE_SHARED: False,
}


def isolate(root, remote=None):
    """Return (session, route) — a private SparkSession where possible.

    `route` is one of the ROUTE_* constants and is falsy exactly when isolation
    failed, so `bool(route)` is the old `isolated` flag. It is a route rather
    than a flag because the two routes differ in a way that matters downstream:
    only one of them comes with a catalog of its own (see CATALOG_IS_PRIVATE).

    TWO ROUTES, because the engines differ and only one of them was ever taken.

    `newSession()` is the JVM route: same engine and connection, own SQLConf,
    temp views and current database. On **Spark Connect it does not exist at
    all** — it raises `JVM_ATTRIBUTE_NOT_SUPPORTED`, so on Sail this function
    used to degrade on every single call and nothing was ever isolated. The
    comment above about collision detection described the fallback as the
    exception; on the default engine it was the rule.

    That was not merely untidy. A OneLake security row filter is installed as a
    per-user temp view, and on a shared session one user's filter narrowed
    another user's data (docs/54, stage 5). Measured, then measured again:
    `e2e/sail-session-isolation` shows `builder.create()` isolates on Sail while
    `getOrCreate()` leaks.

    So Connect gets its own route: a NEW client session against the same
    server. `create()` and not `getOrCreate()` — the latter hands back the
    session we already have, which is the leak.

    Broad excepts on purpose: this runs against three engines (JVM Spark, Sail
    over Spark Connect, and whatever a consumer overlays), and an engine that
    raises anything at all must degrade rather than fail the session bind.
    """
    try:
        session = root.newSession()
        if session is not None:
            return session, ROUTE_NEW_SESSION
    except Exception:  # noqa: BLE001 — Connect has no newSession; try below
        pass

    remote = remote or os.environ.get("SPARK_REMOTE")
    if remote:
        try:
            from pyspark.sql import SparkSession

            session = SparkSession.builder.remote(remote).create()
            if session is not None:
                return session, ROUTE_CONNECT
        except Exception:  # noqa: BLE001 — degrade, as above
            pass
    return root, ROUTE_SHARED


class Claims:
    """Who owns which unqualified name, when sessions cannot be isolated.

    Only consulted on the degraded path. `unqualified` maps a bare table name
    to the (location, session) that first claimed it; `bound` maps each session
    to the schema its unqualified names need the current database to be.

    `bound` holds EVERY session, not just the last setter. Tracking only the
    most recent one loses the session before it: A binds `lake_a`, B binds
    `lake_a` (no conflict, same schema), B rebinds to `lake_b` — and A, still
    depending on `lake_a`, is displaced by a move the registry called clean.
    """

    def __init__(self):
        self.unqualified = {}
        self.bound = {}

    def claim(self, name, location, session):
        """Record a claim; return the conflicting owner, or None.

        A repeat claim by the same session, or by any session pointing at the
        SAME location, is not a conflict — two sessions bound to one lakehouse
        legitimately resolve the same name to the same data.
        """
        owner = self.unqualified.get(name)
        if owner is None:
            self.unqualified[name] = (location, session)
            return None
        if owner[0] == location:
            return None
        return owner

    def take_current(self, schema, session):
        """Record that `session` needs the current database to be `schema`.

        Returns the (schema, session) of any OTHER session that needs a
        different one — moving the shared current database would break it — and
        records nothing in that case, so the incumbent keeps its claim.
        """
        for other, other_schema in self.bound.items():
            if other != session and other_schema != schema:
                return (other_schema, other)
        self.bound[session] = schema
        return None


def _remember_location(name, location, schema=None):
    """Tell delta_ops where a table we just registered lives.

    Tolerant of delta_ops being absent: it is only imported on the Sail/Connect
    path, and on the JVM overlay Spark resolves names natively so there is
    nothing to record. A missing module here must not fail a registration that
    otherwise succeeded.
    """
    try:
        import delta_ops
    except ImportError:
        return
    delta_ops.remember(name, location, schema)


def _remember_schema_location(name, location):
    """Tell delta_ops where a schema lives. Same tolerance as tables above."""
    try:
        import delta_ops
    except ImportError:
        return
    delta_ops.remember_schema(name, location)


def _last_line():
    return traceback.format_exc().splitlines()[-1]


def register(spark, session, schema, tables, schemas=None, claims=None, isolated=True):
    """Declare a lakehouse's Delta tables in this session's Spark catalog.

    On real Fabric a Lakehouse's `Tables/` already ARE catalog tables — attach a
    notebook and `SELECT * FROM silver_customers` resolves, because Fabric keeps
    a metastore in step with the folder. Nothing in this stack holds a
    metastore: Sail is handed object storage and nothing else. So the emulator
    enumerates the folder when a session opens and calls this, and a client that
    addresses a table by NAME rather than by abfs path works the way it does on
    Fabric.

    `schemas` carries a schema-enabled lakehouse's schema folders
    (Tables/<schema>/<table>). Each is created WITH its OneLake location, and
    that is the whole point: a schema created bare lives in the engine's own
    warehouse, so a later `saveAsTable("bronze.x")` succeeds, reports rows
    written, and leaves nothing in the lakehouse. Registering the schema at its
    real location makes schema-qualified writes land where Fabric would put
    them. Entries in `tables` may carry a "schema" of their own for the same
    layout; those without one belong to the lakehouse-name schema.

    Idempotent (CREATE ... IF NOT EXISTS), because a session may be re-opened
    against a lakehouse whose tables are already registered.

    `isolated` says whether `spark` is this Livy session's own. When it is not,
    every route to unqualified resolution is process-wide, so each one is
    checked against `claims` and a cross-lakehouse conflict is REPORTED rather
    than performed — the caller learns the qualified name is required instead
    of reading another lakehouse's rows.
    """
    if spark is None:
        return {"registered": 0, "error": "no spark session in this REPL namespace"}
    claims = claims if claims is not None else Claims()
    registered, failed, conflicts = [], [], []
    try:
        spark.sql(f"CREATE SCHEMA IF NOT EXISTS `{schema}`")
    except Exception:
        return {"registered": 0, "error": f"could not create schema {schema}: {_last_line()}"}
    for s in schemas or []:
        sname, sloc = s.get("name"), s.get("location")
        if not sname or not sloc:
            continue
        try:
            spark.sql(f"CREATE SCHEMA IF NOT EXISTS `{sname}` LOCATION '{sloc}'")
            # Recorded for delta_ops: a CTAS naming this schema must land at
            # this location, and Sail's own CTAS placement ignores it.
            _remember_schema_location(sname, sloc)
        except Exception:
            failed.append(f"schema {sname}: {_last_line()}")
    for t in tables:
        name, loc = t.get("name"), t.get("location")
        tschema = t.get("schema") or schema
        if not name or not loc:
            continue
        try:
            spark.sql(f"CREATE TABLE IF NOT EXISTS `{tschema}`.`{name}` "
                      f"USING delta LOCATION '{loc}'")
            registered.append(name)
            # Record where it lives so a statement naming this table can be
            # resolved without asking the engine. Sail cannot answer
            # DESCRIBE DETAIL at all, and we already know the answer.
            _remember_location(name, loc, tschema)
        except Exception:
            # A folder under Tables/ that is not a readable Delta table is
            # skipped, not fatal — the same tolerance warehouse.Reflect applies.
            failed.append(f"{name}: {_last_line()}")

    unqualified = _make_unqualified_resolve(
        spark, session, schema, tables, registered, failed, conflicts, claims, isolated)

    out = {"registered": len(registered), "tables": registered,
           "unqualified": unqualified, "isolated": isolated}
    if failed:
        out["skipped"] = failed
    if conflicts:
        out["conflicts"] = conflicts
    return out


def _make_unqualified_resolve(spark, session, schema, tables, registered,
                              failed, conflicts, claims, isolated):
    """Make UNQUALIFIED names resolve, as they do in a lakehouse-attached notebook.

    Two routes, because engines differ:

      1. move the session's current database — what Spark does for `USE`;
      2. failing that, register the same tables in `default` too.

    Sail rejects `USE <schema>` and setCurrentDatabase outright, so route 2 is
    the one that actually fires there. It is a duplicate registration of the
    same LOCATION, not a copy of data — both names point at one Delta table.

    Both routes are per-SESSION state when `spark` is this session's own, and
    process-wide state when it is not. On the degraded path each is therefore
    guarded: a conflicting claim is reported and NOT taken, so a session never
    silently acquires another lakehouse's rows under a bare name.
    """
    if _set_current_database(spark, schema):
        # Route 1 works on this engine. Only NOW is a current-database conflict
        # meaningful: on an engine that rejects the call there is nothing to
        # conflict over, and claiming one would send Sail down a dead end
        # instead of the mirror route that actually serves it.
        if isolated:
            return "current-database"
        displaced = claims.take_current(schema, session)
        if displaced is None:
            return "current-database"
        # Put it back. The move has already happened, and leaving it is
        # precisely the silent breakage this guard exists to prevent — the
        # incumbent keeps a working session, this one is told to qualify.
        _set_current_database(spark, displaced[0])
        conflicts.append(
            f"current database is shared and session {displaced[1]!r} needs it "
            f"set to {displaced[0]!r}: unqualified names here would resolve to "
            f"the wrong lakehouse — use `{schema}`.<table>")
        return "conflict-unresolved"

    mirrored = []
    for name in registered:
        # Schema-qualified tables stay qualified: on Fabric a schema-enabled
        # lakehouse resolves `bronze.x`, not a bare `x`.
        loc = next((t["location"] for t in tables
                    if t.get("name") == name and not t.get("schema")), None)
        if not loc:
            continue
        if not isolated:
            owner = claims.claim(name, loc, session)
            if owner is not None:
                conflicts.append(
                    f"{name}: `default`.`{name}` already points at {owner[0]} "
                    f"(session {owner[1]!r}) — unqualified access would read the "
                    f"wrong lakehouse, use `{schema}`.`{name}`")
                continue
        try:
            spark.sql(f"CREATE TABLE IF NOT EXISTS `default`.`{name}` "
                      f"USING delta LOCATION '{loc}'")
            mirrored.append(name)
            _remember_location(name, loc, "default")
        except Exception:
            failed.append(f"default.{name}: {_last_line()}")
    return f"mirrored-into-default ({len(mirrored)})"


def _set_current_database(spark, schema):
    """Move the current database, reporting whether the engine allowed it.

    Sail rejects both `USE <schema>` and setCurrentDatabase outright, so this
    is a capability probe as much as an action.
    """
    try:
        spark.catalog.setCurrentDatabase(schema)
    except Exception:  # noqa: BLE001 — engines differ; the caller has a route 2
        return False
    return True
