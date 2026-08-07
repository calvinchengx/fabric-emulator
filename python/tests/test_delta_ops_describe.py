"""DESCRIBE routed to delta-rs: what matches, and what it answers.

Both statements are measured ❌ on Sail in docs/engine-matrix.md, and they fail
in the two ways hardest to notice: `DESCRIBE DETAIL` is absent from the grammar
outright, while `DESCRIBE` on a registered Delta table returns the right SCHEMA
and **zero rows**, raising nothing.

Execution here runs against a real Delta table on the local filesystem — the
metadata is genuinely read out of a `_delta_log`, not synthesized — with a
minimal stand-in for `spark.createDataFrame`, which is the only Spark API these
two touch.
"""
import sys
from pathlib import Path

import pyarrow as pa
import pytest
from deltalake import write_deltalake

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import delta_ops


class FakeSpark:
    """Only `createDataFrame` is used, so only it is provided."""

    def createDataFrame(self, rows, columns):
        return {"rows": rows, "columns": columns}


@pytest.fixture
def table(tmp_path):
    """A real Delta table with a partition column, written by delta-rs."""
    path = str(tmp_path / "accounts")
    write_deltalake(path, pa.table({
        "id": pa.array([1, 2], pa.int64()),
        "name": pa.array(["Acme", "Globex"]),
        "region": pa.array(["emea", "amer"]),
    }), partition_by=["region"])
    return path


def _resolve(_name):
    raise AssertionError("a delta.`path` target must not need the catalog")


def test_describe_detail_matches_and_reads_the_log(table):
    kind, params = delta_ops.match(f"DESCRIBE DETAIL delta.`{table}`")
    assert kind == "describe_detail"

    got = delta_ops.describe_detail(FakeSpark(), params, _resolve, storage_options={})
    row = dict(zip(got["columns"], got["rows"][0], strict=True))
    assert row["format"] == "delta"
    assert row["location"] == table
    assert row["version"] == 0
    assert row["numFiles"] >= 1
    # Read out of the log, not assumed: the table really is partitioned.
    assert row["partitionColumns"] == ["region"]
    # `id` is the Delta table's own uuid — present rather than blank.
    assert len(row["id"]) > 10


def test_describe_table_lists_real_columns_with_spark_type_names(table):
    kind, params = delta_ops.match(f"DESCRIBE TABLE delta.`{table}`")
    assert kind == "describe_table"

    got = delta_ops.describe_table(FakeSpark(), params, _resolve, storage_options={})
    assert got["columns"] == ["col_name", "data_type", "comment"]
    by_name = {r[0]: r for r in got["rows"]}
    assert set(by_name) == {"id", "name", "region"}
    # Delta says `long`; every DESCRIBE a notebook has ever read says `bigint`.
    assert by_name["id"][1] == "bigint"
    assert by_name["name"][1] == "string"
    # The partition column is marked, which is what Spark's own DESCRIBE does.
    assert by_name["region"][2] == "partition"


def test_bare_desc_and_describe_both_match(table):
    for sql in (f"DESC delta.`{table}`", f"DESCRIBE delta.`{table}`",
                f"describe table delta.`{table}` ;"):
        kind, _ = delta_ops.match(sql)
        assert kind == "describe_table", sql


def test_describe_of_an_unlocatable_name_falls_through_to_the_engine():
    # The seam's rule: anything we cannot locate is the engine's business. A
    # DESCRIBE of a temp view, a function, or an engine-managed table must NOT
    # be intercepted — a shim that guessed would be worse than the gap.
    delta_ops.forget_all()
    assert delta_ops.match("DESCRIBE some_temp_view") is None
    assert delta_ops.match("DESCRIBE FUNCTION upper") is None


def test_describe_of_a_registered_name_is_intercepted():
    delta_ops.forget_all()
    delta_ops.remember("accounts", "az://ws/item/Tables/accounts", schema="silver")
    kind, params = delta_ops.match("DESCRIBE TABLE silver.accounts")
    assert kind == "describe_table"
    assert params["target"] == "silver.accounts"


def test_the_plain_grammar_cannot_swallow_a_detail_statement(table):
    # Answering a DETAIL request with a column list would be plausible output to
    # the wrong question. What prevents it is the GRAMMAR, not matcher order:
    # the plain form anchors at end-of-statement, so `DESCRIBE DETAIL x` leaves
    # `x` unconsumed and cannot match. Asserted directly, because an earlier
    # version of this test claimed order was the protection — mutation testing
    # reordered the matchers and every test still passed.
    assert delta_ops._DESCRIBE_TABLE.match(f"DESCRIBE DETAIL delta.`{table}`") is None
    assert delta_ops._DESCRIBE_DETAIL.match(f"DESCRIBE DETAIL delta.`{table}`") is not None
    kind, _ = delta_ops.match(f"DESCRIBE DETAIL delta.`{table}`")
    assert kind == "describe_detail"


def test_spark_type_names():
    assert delta_ops._spark_type('PrimitiveType("long")') == "bigint"
    assert delta_ops._spark_type('PrimitiveType("timestampNtz")') == "timestamp_ntz"
    # Anything structured passes through as delta-rs renders it rather than
    # being guessed at — a wrong nested type is worse than an unfamiliar one.
    assert delta_ops._spark_type("StructType(...)") == "StructType(...)"
