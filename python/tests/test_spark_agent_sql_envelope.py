"""`run_sql` must return a DML's row count, not absorb it as an empty result.

THE BUG THIS PINS. DataFusion reports INSERT/MERGE row counts as uint64, which
the Arrow conversion to the Connect client rejects (Spark has no unsigned
types). The agent caught that failure and returned an empty envelope, so a
client could not tell "wrote 3 rows" from "wrote nothing" — parity.md carried
`DML row-count envelopes` as 🟡 for exactly this.

The fix reads the statement's cached result relation through `toArrow()`, which
skips the Spark-type conversion that rejects uint64. Measured against live Sail
before this test existed: the DML executes at `spark.sql()`, so re-reading the
result does NOT re-run it — a 3-row INSERT recovered this way leaves the table
at 3 rows. The route-level witness is `e2e/livy` (kind=sql INSERT asserting the
envelope AND the count-back); these stubs pin the branch logic so a refactor
cannot quietly restore the absorption.

`run_sql` lives in sqlrun.py, split out of agent.py the way catalog.py was:
agent.py builds its SparkSession at import (and dials it — measured, a bogus
SPARK_REMOTE hangs the import), so the function had to move to become testable.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import sqlrun as agent  # noqa: E402

UINT64_MSG = (
    "[UNSUPPORTED_DATA_TYPE_FOR_ARROW_CONVERSION] "
    "uint64 is not supported in conversion to Arrow."
)


class FakeSchema:
    fields = ("count",)  # non-empty: the DML result relation has one column

    @staticmethod
    def jsonValue():
        return {"type": "struct", "fields": [
            {"name": "count", "type": "long", "nullable": False, "metadata": {}}]}


class FakeArrowTable:
    @staticmethod
    def to_pylist():
        return [{"count": 3}]


class UintDF:
    """collect() dies the way pyspark-client dies on a uint64 payload."""
    schema = FakeSchema()

    def collect(self):
        raise Exception(UINT64_MSG)

    def toArrow(self):
        return FakeArrowTable()


class UintDFNoArrow(UintDF):
    def toArrow(self):
        raise RuntimeError("toArrow unavailable")


class BrokenDF:
    """A genuinely failing statement — must stay an error, not an envelope."""
    schema = FakeSchema()

    def collect(self):
        raise Exception("Table 'nope' not found")


def _g(df):
    class FakeSpark:
        @staticmethod
        def sql(code):
            return df
    return {"spark": FakeSpark()}


def test_the_count_is_recovered_not_absorbed():
    out = agent.run_sql("INSERT INTO t VALUES (1),(2),(3)", _g(UintDF()))
    assert out["status"] == "ok"
    payload = out["data"]["application/json"]
    assert payload["data"] == [[3]], "the count the engine reported must survive"
    assert payload["schema"]["fields"][0]["name"] == "count"


def test_absorption_is_the_fallback_of_last_resort():
    out = agent.run_sql("INSERT INTO t VALUES (1)", _g(UintDFNoArrow()))
    # The write has landed either way; an empty envelope is the old, honest
    # floor when even the arrow read fails.
    assert out == {"status": "ok", "execution_count": 0, "data": {}}


def test_real_sql_errors_still_surface():
    out = agent.run_sql("SELECT * FROM nope", _g(BrokenDF()))
    assert out["status"] == "error"
    assert "nope" in out["evalue"]
