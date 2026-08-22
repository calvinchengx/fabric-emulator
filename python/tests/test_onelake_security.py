"""The engine half of OneLake security: what the agent does with a policy.

Unit-level on purpose. The interesting cases are the refusals — a table the
policy never mentions, a view that will not build — and those are awkward to
provoke through a live Spark session while being exactly what a security
control must get right.
"""
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))
import onelake_security as ols  # noqa: E402


class FakeSpark:
    """Records SQL instead of running it."""

    def __init__(self, fail_on=()):
        self.ran = []
        self.fail_on = fail_on

    def sql(self, q):
        self.ran.append(q)
        if any(f in q for f in self.fail_on):
            raise RuntimeError("boom")
        return None


def response(payload):
    class R:
        def __enter__(self_inner):
            return self_inner

        def __exit__(self_inner, *a):
            return False

        def read(self_inner):
            return json.dumps(payload).encode()

    return lambda req: R()


def test_fetch_access_keys_by_table_name():
    got = ols.fetch_access("http://f", "ws", "item", "alice", "tok", opener=response({
        "value": [
            {"path": "Tables/sales", "rows": "SELECT * FROM sales WHERE r = 1", "access": ["Read"]},
            {"path": "Tables/dbo/customers", "columns": ["id"], "access": ["Read"]},
            {"path": "Files/raw", "access": ["Read"]},
        ]}))
    # Files entries are not tables and must not become catalog names.
    assert set(got) == {"sales", "customers"}
    assert got["sales"]["rows"].startswith("SELECT")
    assert got["customers"]["columns"] == ["id"]


def test_a_whole_half_grant_is_recorded_as_a_wildcard():
    got = ols.fetch_access("http://f", "ws", "item", "alice", "tok", opener=response({
        "value": [{"path": "Tables", "access": ["Read"]}]}))
    assert "*" in got and got["*"]["rows"] == ""


def test_no_policy_is_an_empty_map_not_an_error():
    assert ols.fetch_access("http://f", "ws", "i", "a", "t", opener=response({"value": []})) == {}


def test_a_table_the_policy_omits_is_removed():
    spark = FakeSpark()
    ols.apply(spark, {"sales": {"rows": "", "columns": []}}, ["sales", "secrets"])
    dropped = [q for q in spark.ran if "secrets" in q]
    assert dropped, "an ungranted table was left readable"
    assert all("DROP" in q for q in dropped), dropped
    # The granted, unrestricted table is untouched: replacing it with
    # `SELECT * FROM sales` would be a no-op that could only introduce bugs.
    assert not [q for q in spark.ran if "sales" in q]


def test_a_row_filter_becomes_the_relation():
    spark = FakeSpark()
    ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1", "columns": []}}, ["sales"])
    view = [q for q in spark.ran if "CREATE OR REPLACE TEMP VIEW" in q]
    assert len(view) == 1, spark.ran
    assert "WHERE r = 1" in view[0]


def test_columns_narrow_the_projection_and_compose_with_rows():
    assert ols.secure_view_sql("t", {"rows": "", "columns": ["a", "b"]}) == "SELECT a, b FROM t"
    both = ols.secure_view_sql("t", {"rows": "SELECT * FROM t WHERE x", "columns": ["a"]})
    assert both == "SELECT a FROM (SELECT * FROM t WHERE x)"


def test_a_wildcard_grant_covers_tables_the_policy_did_not_name():
    spark = FakeSpark()
    ols.apply(spark, {"*": {"rows": "", "columns": []}}, ["sales", "regions"])
    assert not [q for q in spark.ran if "DROP" in q], spark.ran


def test_a_view_that_will_not_build_fails_closed():
    # If the filter cannot be applied, the unfiltered table must NOT survive.
    spark = FakeSpark(fail_on=("CREATE OR REPLACE TEMP VIEW",))
    ols.apply(spark, {"sales": {"rows": "SELECT nonsense", "columns": []}}, ["sales"])
    assert [q for q in spark.ran if "DROP" in q and "sales" in q], spark.ran


def test_logging_names_what_happened():
    lines = []
    spark = FakeSpark()
    ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1", "columns": []}},
              ["sales", "secrets"], log=lines.append)
    assert any("narrowed" in x for x in lines) and any("withheld" in x for x in lines), lines


# --- the qualified-name sweep -------------------------------------------------
#
# MEASURED, NOT IMAGINED. `e2e/onelake-security-bypass` put a viewer's filter on
# `sales` and then asked for `default.sales`: 3 rows of 3 and both columns,
# where the view gave 2 rows and one column. A temp view shadows the UNQUALIFIED
# name only, and `catalog.register()` deliberately registers every table into
# `default` as well, so that convenience registration was the way around the
# filter. These pin the sweep that closes it.

class CatalogSpark(FakeSpark):
    """A fake that can answer SHOW DATABASES / SHOW TABLES, and forgets a DROP.

    The base fake returns None for everything, which makes the sweep no-op
    silently — fine for the tests that predate it, useless for these. Here the
    catalog is real enough to be swept and to be checked afterwards.
    """

    def __init__(self, catalog=None, temp=(), **kw):
        super().__init__(**kw)
        # schema -> [table names]
        self.catalog = {k: list(v) for k, v in (catalog or {}).items()}
        self.temp: set[str] = set(temp)

    def sql(self, q):
        self.ran.append(q)
        if any(f in q for f in self.fail_on):
            raise RuntimeError("boom")
        if q == "SHOW DATABASES":
            return _Rows([(db,) for db in self.catalog])
        if q.startswith("SHOW TABLES IN "):
            db = q[len("SHOW TABLES IN "):].strip("`")
            return _Rows([(db, t, t in self.temp) for t in self.catalog.get(db, [])])
        if q.startswith("DROP TABLE IF EXISTS `"):
            db, table = q[len("DROP TABLE IF EXISTS `"):].rstrip("`").split("`.`")
            if table in self.catalog.get(db, []):
                self.catalog[db].remove(table)
        return None


class _Rows:
    def __init__(self, rows):
        self._rows = rows

    def collect(self):
        return self._rows


def test_a_narrowed_table_loses_every_qualified_registration():
    spark = CatalogSpark(catalog={"default": ["sales"], "lake_a": ["sales"]})
    ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1",
                                "columns": []}}, ["sales"])
    assert spark.catalog == {"default": [], "lake_a": []}, (
        "a qualified name still resolves to the unfiltered table")
    # And the view — the only remaining way to name it — was built FIRST, while
    # the real table still existed for it to select from.
    order = [q for q in spark.ran
             if "CREATE OR REPLACE TEMP VIEW" in q or q.startswith("DROP TABLE IF EXISTS `")]
    assert "CREATE OR REPLACE TEMP VIEW" in order[0], order


def test_the_sweep_does_not_delete_the_filter_it_just_installed():
    # The view is registered as a temporary table in the same session. Sweeping
    # it as though it were a qualified registration would remove the
    # enforcement and leave the name unbound — worse than either outcome.
    spark = CatalogSpark(catalog={"default": ["sales"]}, temp=("sales",))
    ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1",
                                "columns": []}}, ["sales"])
    assert spark.catalog == {"default": ["sales"]}, spark.ran


def test_a_denied_table_is_swept_from_every_schema_too():
    spark = CatalogSpark(catalog={"default": ["secret"], "lake_a": ["secret"]})
    ols.apply(spark, {}, ["secret"])
    assert spark.catalog == {"default": [], "lake_a": []}, spark.ran


def test_an_unrestricted_table_keeps_its_qualified_names():
    # Nothing is being enforced on it, so removing a spelling would be pure
    # breakage.
    spark = CatalogSpark(catalog={"default": ["sales"], "lake_a": ["sales"]})
    ols.apply(spark, {"sales": {"rows": "", "columns": []}}, ["sales"])
    assert spark.catalog == {"default": ["sales"], "lake_a": ["sales"]}


def test_a_schema_that_cannot_be_listed_does_not_stop_the_others():
    spark = CatalogSpark(catalog={"broken": ["sales"], "lake_a": ["sales"]},
                         fail_on=("SHOW TABLES IN `broken`",))
    ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1",
                                "columns": []}}, ["sales"])
    assert spark.catalog["lake_a"] == [], spark.ran


def test_an_engine_with_no_schemas_is_not_an_error():
    # Plain FakeSpark returns None for SHOW DATABASES, which is what an engine
    # without the statement looks like. The view must still be installed.
    spark = FakeSpark()
    ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1",
                                "columns": []}}, ["sales"])
    assert [q for q in spark.ran if "CREATE OR REPLACE TEMP VIEW" in q]


# --- the refusal on a shared catalog ------------------------------------------

def test_a_shared_catalog_refuses_rather_than_reshaping_it():
    # Editing a shared catalog to secure ONE session takes the table away from
    # every other session on the engine. Refusing costs availability; doing it
    # anyway costs everyone else their data.
    spark = CatalogSpark(catalog={"default": ["sales"]})
    try:
        ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1",
                                    "columns": []}}, ["sales"],
                  catalog_private=False)
    except ols.CannotEnforce as exc:
        assert "sales" in str(exc)
    else:
        raise AssertionError("a shared catalog was reshaped")
    assert not [q for q in spark.ran if "DROP" in q], (
        "it refused, but only after damaging the shared catalog")


def test_a_shared_catalog_with_nothing_to_secure_is_fine():
    # The refusal is about enforcement it cannot deliver, not about the route.
    # A session where every table is unrestricted has nothing to enforce.
    spark = CatalogSpark(catalog={"default": ["sales"]})
    ols.apply(spark, {"*": {"rows": "", "columns": []}}, ["sales"],
              catalog_private=False)
    assert spark.catalog == {"default": ["sales"]}


def test_a_denied_table_also_triggers_the_refusal():
    # Withholding is enforcement too: dropping it from a shared catalog would
    # withhold it from everyone.
    spark = CatalogSpark(catalog={"default": ["secret"]})
    try:
        ols.apply(spark, {}, ["secret"], catalog_private=False)
    except ols.CannotEnforce:
        pass
    else:
        raise AssertionError("a denied table was dropped from a shared catalog")


# --- re-applying to an already-secured session --------------------------------
#
# apply() runs per STATEMENT. The sweep removes the table the view was built
# from, so the second statement's rebuild would read the view instead — and
# with CLS in play the row filter may name a column the view no longer has,
# turning "narrowed" into "withheld" on statement two.

def test_the_second_application_rebuilds_from_the_table_not_the_view():
    spark = CatalogSpark(catalog={"default": ["sales"]})
    entry = {"sales": {"rows": "SELECT * FROM sales WHERE r = 1", "columns": ["a"]}}
    locations = {"sales": "abfss://lake/Tables/sales"}

    ols.apply(spark, entry, ["sales"], location_of=locations.get)
    assert spark.catalog == {"default": []}, "the first pass did not sweep"

    # Statement two: the name now resolves only to the temp view.
    spark.temp.add("sales")
    spark.catalog["default"].append("sales")
    spark.ran.clear()
    ols.apply(spark, entry, ["sales"], location_of=locations.get)

    recreated = [q for q in spark.ran if q.startswith("CREATE TABLE IF NOT EXISTS")]
    assert recreated and "abfss://lake/Tables/sales" in recreated[0], spark.ran
    # Unqualified: the current database is the lakehouse schema, and that is
    # where the filter's own unqualified SQL will look for the table.
    assert recreated[0].startswith("CREATE TABLE IF NOT EXISTS `sales`"), recreated[0]
    # ...and the view is rebuilt AFTER the table is back.
    order = [i for i, q in enumerate(spark.ran)
             if q.startswith("CREATE TABLE IF NOT EXISTS")
             or "CREATE OR REPLACE TEMP VIEW" in q]
    assert spark.ran[order[0]].startswith("CREATE TABLE"), spark.ran


def test_without_a_known_location_the_view_is_left_alone():
    # Nothing to restore from. Rebuilding blindly would be worse than keeping
    # the filter that is already installed.
    spark = CatalogSpark(catalog={"default": ["sales"]}, temp=("sales",))
    lines = []
    ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1",
                                "columns": []}}, ["sales"],
              location_of=lambda _n: None, log=lines.append)
    assert not [q for q in spark.ran if q.startswith("CREATE TABLE")], spark.ran
    # And it SAYS so. A silent skip here is what made the livy failure read as
    # a mystery rather than a missing location.
    assert any("no recorded location" in x for x in lines), lines


def test_a_restore_that_fails_says_so_and_leaves_no_table_behind():
    # DROP VIEW succeeds, CREATE TABLE fails. The old view is gone and the
    # table never came back, so the name resolves to nothing — which is what
    # makes the rebuild below fail on a real engine and withhold the table.
    # The fake cannot model name resolution, so this asserts the trace: the
    # restore was attempted, announced, and left nothing readable.
    spark = CatalogSpark(catalog={"default": ["sales"]}, temp=("sales",),
                         fail_on=("CREATE TABLE IF NOT EXISTS",))
    lines = []
    ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1",
                                "columns": []}}, ["sales"],
              location_of=lambda _n: "abfss://lake/Tables/sales", log=lines.append)
    assert any("could not be restored" in x for x in lines), lines
    assert "DROP VIEW IF EXISTS sales" in spark.ran, spark.ran
    assert not [q for q in spark.ran if q.startswith("CREATE TABLE") and "boom" not in q
                and q in spark.ran[spark.ran.index("DROP VIEW IF EXISTS sales") + 2:]], \
        "a table was registered after the restore reported failure"
