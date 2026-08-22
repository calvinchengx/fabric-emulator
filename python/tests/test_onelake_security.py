"""The engine half of OneLake security: what the agent does with a policy.

Unit-level on purpose. The interesting cases are the refusals — a table the
policy never mentions, a view that will not build — and those are awkward to
provoke through a live Spark session while being exactly what a security
control must get right.
"""
import json
import io
import sys
from pathlib import Path

import pytest

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
