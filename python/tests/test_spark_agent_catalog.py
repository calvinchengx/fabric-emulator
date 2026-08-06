"""Catalog state must be per-Livy-session, not per-agent-process.

THE BUG THIS FIXES. The agent holds one SparkSession and serves concurrent
requests from a ThreadingHTTPServer. `register_tables` moved the current
database with `setCurrentDatabase(schema)` — process-wide — so two notebooks
bound to different lakehouses raced, and the loser's unqualified
`spark.table("customers")` resolved against the OTHER lakehouse. Not an error:
rows, from the wrong place. Reachable today through plain concurrent Livy
sessions, with no notebook DAG involved.

The fake engine below models the one property under test — how an unqualified
name resolves — for the two engine shapes that matter: one that implements
`newSession()` (JVM Spark) and one that implements neither `newSession()` nor
`setCurrentDatabase` (Sail, over Spark Connect). A real engine is not needed to
prove a name resolves to the wrong location, and requiring one is why this code
had no tests at all.
"""
import sys
import threading
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import catalog  # noqa: E402


class FakeCatalogAPI:
    def __init__(self, session):
        self._session = session

    def setCurrentDatabase(self, db):  # noqa: N802 — mirrors PySpark's name
        if not self._session.supports_current_database:
            raise RuntimeError("USE/setCurrentDatabase not supported by this engine")
        if db not in self._session.server.schemas:
            raise RuntimeError(f"no such database: {db}")
        self._session.current_database = db


class FakeServer:
    """Engine-side state: schemas and tables, shared by every session on it."""

    def __init__(self):
        self.schemas = set()
        self.tables = {}  # (db, name) -> location


class FakeSpark:
    """One client session against a FakeServer.

    `current_database` is per-session state, which is exactly the thing the
    real bug got wrong by keeping it per-process.
    """

    def __init__(self, server=None, supports_new_session=True,
                 supports_current_database=True, new_session_returns_none=False,
                 fail_creates=()):
        self.server = server or FakeServer()
        self.supports_new_session = supports_new_session
        self.supports_current_database = supports_current_database
        self.new_session_returns_none = new_session_returns_none
        self.fail_creates = set(fail_creates)
        self.current_database = "default"
        self.catalog = FakeCatalogAPI(self)

    def newSession(self):  # noqa: N802 — mirrors PySpark's name
        if self.new_session_returns_none:
            return None
        if not self.supports_new_session:
            raise RuntimeError("newSession not supported by this engine")
        return FakeSpark(
            server=self.server,
            supports_new_session=self.supports_new_session,
            supports_current_database=self.supports_current_database,
            fail_creates=self.fail_creates,
        )

    def sql(self, stmt):
        text = " ".join(stmt.split())
        if text.startswith("CREATE SCHEMA IF NOT EXISTS"):
            name = text.split("`")[1]
            if name in self.fail_creates:
                raise RuntimeError(f"cannot create schema {name}")
            self.server.schemas.add(name)
            return self
        if text.startswith("CREATE TABLE IF NOT EXISTS"):
            parts = text.split("`")
            db, name = parts[1], parts[3]
            if name in self.fail_creates:
                raise RuntimeError(f"cannot create table {name}")
            location = text.split("LOCATION '")[1].rstrip("'")
            self.server.tables.setdefault((db, name), location)
            return self
        raise AssertionError(f"unexpected SQL in test: {text}")

    def resolve(self, name):
        """Where an UNQUALIFIED name resolves for this session, or None."""
        return self.server.tables.get((self.current_database, name))


def lakehouse(name, table, location):
    return {"schema": name, "tables": [{"name": table, "location": location}]}


def bind(root, session, lake, claims, table="customers", location=None):
    """Open a session against the root engine and register a lakehouse in it."""
    spark, isolated = catalog.isolate(root)
    result = catalog.register(
        spark, session, lake,
        [{"name": table, "location": location or f"abfss://{lake}/Tables/{table}"}],
        claims=claims, isolated=isolated)
    return spark, result


# --- the headline: two lakehouses, two sessions, no leakage -------------------

def test_two_sessions_on_different_lakehouses_each_resolve_their_own():
    root = FakeSpark()
    claims = catalog.Claims()
    a, _ = bind(root, "sess-a", "lake_a", claims)
    b, _ = bind(root, "sess-b", "lake_b", claims)

    # Each session's unqualified name resolves to ITS OWN lakehouse. Before the
    # fix, both answered with whichever session registered last.
    assert a.resolve("customers") == "abfss://lake_a/Tables/customers"
    assert b.resolve("customers") == "abfss://lake_b/Tables/customers"


def test_binding_a_second_session_does_not_move_the_first_ones_current_database():
    root = FakeSpark()
    claims = catalog.Claims()
    a, _ = bind(root, "sess-a", "lake_a", claims)
    assert a.current_database == "lake_a"
    bind(root, "sess-b", "lake_b", claims)
    assert a.current_database == "lake_a"


def test_the_root_session_is_never_moved_by_a_bind():
    root = FakeSpark()
    bind(root, "sess-a", "lake_a", catalog.Claims())
    assert root.current_database == "default"


def test_concurrent_binds_each_keep_their_own_resolution():
    # The real trigger: two Livy sessions binding at the same moment on the
    # agent's ThreadingHTTPServer.
    root = FakeSpark()
    claims = catalog.Claims()
    sessions, errors = {}, []

    def worker(n):
        try:
            spark, _ = bind(root, f"sess-{n}", f"lake_{n}", claims)
            sessions[n] = spark
        except Exception as exc:  # noqa: BLE001 - surfaced by the assert below
            errors.append(exc)

    threads = [threading.Thread(target=worker, args=(n,)) for n in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert not errors, errors
    for n, spark in sessions.items():
        assert spark.resolve("customers") == f"abfss://lake_{n}/Tables/customers"


# --- isolate() ---------------------------------------------------------------

def test_isolate_returns_a_private_session_when_the_engine_can():
    root = FakeSpark()
    session, isolated = catalog.isolate(root)
    assert isolated is True
    assert session is not root
    assert session.server is root.server  # same engine, split session state


def test_isolate_degrades_when_new_session_raises():
    root = FakeSpark(supports_new_session=False)
    session, isolated = catalog.isolate(root)
    assert (session, isolated) == (root, False)


def test_isolate_degrades_when_new_session_returns_none():
    root = FakeSpark(new_session_returns_none=True)
    session, isolated = catalog.isolate(root)
    assert (session, isolated) == (root, False)


# --- the degraded path: detect, report, and do not perform -------------------

def test_a_degraded_engine_reports_the_current_database_conflict():
    # JVM-shaped: setCurrentDatabase works, newSession does not. The second
    # bind would silently repoint the first session's unqualified names.
    root = FakeSpark(supports_new_session=False)
    claims = catalog.Claims()
    _, first = bind(root, "sess-a", "lake_a", claims)
    _, second = bind(root, "sess-b", "lake_b", claims)

    assert first["unqualified"] == "current-database"
    assert "conflicts" not in first
    assert second["unqualified"] == "conflict-unresolved"
    assert any("lake_a" in c and "sess-a" in c for c in second["conflicts"]), second
    # And the conflict was REPORTED, not performed: session A still resolves.
    assert root.current_database == "lake_a"


def test_a_degraded_engine_reports_the_default_mirror_conflict():
    # Sail-shaped: neither newSession nor setCurrentDatabase. Unqualified
    # resolution goes through `default`, which is process-wide.
    root = FakeSpark(supports_new_session=False, supports_current_database=False)
    claims = catalog.Claims()
    _, first = bind(root, "sess-a", "lake_a", claims)
    _, second = bind(root, "sess-b", "lake_b", claims)

    assert first["unqualified"] == "mirrored-into-default (1)"
    assert second["unqualified"] == "mirrored-into-default (0)"
    assert any("customers" in c and "lake_a" in c for c in second["conflicts"]), second
    # `default`.`customers` still points at the FIRST claimant, and the second
    # session was told to use the qualified name rather than handed these rows.
    assert root.server.tables[("default", "customers")] == "abfss://lake_a/Tables/customers"


def test_two_sessions_on_the_SAME_lakehouse_are_not_a_conflict():
    # Same location under one name is the legitimate case — two notebooks
    # attached to one lakehouse — and must not be reported.
    root = FakeSpark(supports_new_session=False, supports_current_database=False)
    claims = catalog.Claims()
    bind(root, "sess-a", "lake_a", claims)
    _, second = bind(root, "sess-b", "lake_a", claims)
    assert "conflicts" not in second, second


def test_rebinding_the_same_session_is_not_a_conflict():
    root = FakeSpark(supports_new_session=False)
    claims = catalog.Claims()
    bind(root, "sess-a", "lake_a", claims)
    _, again = bind(root, "sess-a", "lake_b", claims)
    assert "conflicts" not in again, again


def test_an_isolated_session_never_consults_the_claims_registry():
    root = FakeSpark()
    claims = catalog.Claims()
    bind(root, "sess-a", "lake_a", claims)
    bind(root, "sess-b", "lake_b", claims)
    assert claims.unqualified == {} and claims.bound == {}


# --- Claims ------------------------------------------------------------------

def test_claim_reports_the_owner_only_on_a_different_location():
    c = catalog.Claims()
    assert c.claim("t", "loc1", "s1") is None
    assert c.claim("t", "loc1", "s2") is None       # same data, no conflict
    assert c.claim("t", "loc2", "s2") == ("loc1", "s1")


def test_take_current_reports_a_session_that_still_needs_another_schema():
    c = catalog.Claims()
    assert c.take_current("a", "s1") is None
    assert c.take_current("a", "s2") is None        # same schema, no conflict
    # s2 moving to `b` would break s1, which is still bound to `a` — and s1 is
    # NOT the most recent setter, which is why the registry holds every session.
    assert c.take_current("b", "s2") == ("a", "s1")
    # Refused, so s2's claim on `a` stands rather than being quietly rewritten.
    assert c.bound == {"s1": "a", "s2": "a"}


def test_take_current_lets_the_only_session_rebind_freely():
    c = catalog.Claims()
    assert c.take_current("a", "s1") is None
    assert c.take_current("b", "s1") is None
    assert c.bound == {"s1": "b"}


# --- registration mechanics --------------------------------------------------

def test_no_spark_session_is_an_error_not_a_crash():
    out = catalog.register(None, "s", "lake", [])
    assert out["registered"] == 0 and "no spark session" in out["error"]


def test_a_schema_that_cannot_be_created_aborts_with_the_reason():
    root = FakeSpark(fail_creates={"lake_a"})
    out = catalog.register(root, "s", "lake_a", [{"name": "t", "location": "loc"}])
    assert out["registered"] == 0 and "could not create schema" in out["error"]


def test_a_table_that_cannot_be_created_is_skipped_not_fatal():
    root = FakeSpark(fail_creates={"broken"})
    out = catalog.register(root, "s", "lake_a", [
        {"name": "broken", "location": "loc1"},
        {"name": "fine", "location": "loc2"},
    ])
    assert out["tables"] == ["fine"]
    assert any("broken" in s for s in out["skipped"])


def test_entries_missing_a_name_or_location_are_ignored():
    root = FakeSpark()
    out = catalog.register(root, "s", "lake_a", [
        {"name": "t"}, {"location": "loc"}, {},
        {"name": "good", "location": "loc"},
    ])
    assert out["tables"] == ["good"]


def test_schema_folders_are_created_at_their_onelake_location():
    root = FakeSpark()
    catalog.register(root, "s", "lake_a", [], schemas=[
        {"name": "bronze", "location": "abfss://lake_a/Tables/bronze"},
        {"name": "nameless"},          # ignored: no location
        {"location": "no-name"},       # ignored: no name
    ])
    assert "bronze" in root.server.schemas
    assert "nameless" not in root.server.schemas


def test_a_schema_qualified_table_is_not_mirrored_into_default():
    # On Fabric a schema-enabled lakehouse resolves `bronze.x`, not a bare `x`.
    root = FakeSpark(supports_new_session=False, supports_current_database=False)
    out = catalog.register(root, "s", "lake_a", [
        {"name": "x", "location": "loc", "schema": "bronze"},
    ], claims=catalog.Claims(), isolated=False)
    assert out["unqualified"] == "mirrored-into-default (0)"
    assert ("default", "x") not in root.server.tables


def test_the_result_reports_whether_the_session_was_isolated():
    root = FakeSpark()
    spark, isolated = catalog.isolate(root)
    assert catalog.register(spark, "s", "lake_a", [], isolated=isolated)["isolated"] is True
    degraded = FakeSpark(supports_new_session=False)
    spark2, isolated2 = catalog.isolate(degraded)
    assert catalog.register(spark2, "s", "lake_a", [], isolated=isolated2)["isolated"] is False


@pytest.mark.parametrize("supports_current_database", [True, False])
def test_registration_succeeds_on_both_engine_shapes(supports_current_database):
    root = FakeSpark(supports_current_database=supports_current_database)
    spark, isolated = catalog.isolate(root)
    out = catalog.register(spark, "s", "lake_a",
                           [{"name": "t", "location": "abfss://lake_a/Tables/t"}],
                           claims=catalog.Claims(), isolated=isolated)
    assert out["registered"] == 1
    assert spark.resolve("t") == "abfss://lake_a/Tables/t"
