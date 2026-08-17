"""A typed result set must reach the client, not drop the socket.

THE BUG THIS PINS. `run_sql` built its rows with `list(r)` and handed them
straight to the HTTP writer's `json.dumps`. Any result carrying a DATE —
`SELECT current_date()`, or a `to_date()` cast over a bronze column — raised

    TypeError: Object of type date is not JSON serializable

inside the handler, so the reply was never written. The caller saw only
`RemoteDisconnected: Remote end closed connection without response`, which
reads as a network fault: reported from the field as first-suspected OOM, then
a timeout, when the agent was at 176 MiB of 15.7 GiB and `run_sql` had already
succeeded. A dbt model returning a bare date killed the agent, and dates are
not exotic in a warehouse.

The recovery path mattered too: the uint64 branch built rows the same way, so
a DML whose result carried a typed value would have failed identically. Both
paths are pinned below.

These stubs import no Spark; the session arrives in `g`, as in
test_spark_agent_sql_envelope.py.
"""
import datetime
import decimal
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import sqlrun as agent  # noqa: E402

UINT64_MSG = (
    "[UNSUPPORTED_DATA_TYPE_FOR_ARROW_CONVERSION] "
    "uint64 is not supported in conversion to Arrow."
)


class FakeSchema:
    fields = ("v",)

    @staticmethod
    def jsonValue():
        return {"type": "struct", "fields": [
            {"name": "v", "type": "date", "nullable": True, "metadata": {}}]}


class FakeDF:
    """Returns one row of whatever values the test supplies."""

    def __init__(self, rows):
        self._rows = rows
        self.schema = FakeSchema

    def collect(self):
        return self._rows


class FakeSpark:
    def __init__(self, df):
        self._df = df

    def sql(self, _code):
        return self._df


def envelope(rows):
    out = agent.run_sql("SELECT v", {"spark": FakeSpark(FakeDF(rows))})
    assert out["status"] == "ok", out
    return out["data"]["application/json"]["data"]


def test_a_date_round_trips_and_the_envelope_encodes():
    rows = envelope([[datetime.date(2026, 8, 18)]])
    assert rows == [["2026-08-18"]]
    # The real failure was at encode time, so encoding is the assertion.
    json.dumps(rows)


def test_datetime_and_time_round_trip():
    rows = envelope([[datetime.datetime(2026, 8, 18, 13, 45, 6)],
                     [datetime.time(1, 2, 3)]])
    assert rows == [["2026-08-18T13:45:06"], ["01:02:03"]]
    json.dumps(rows)


def test_decimal_keeps_its_precision_as_a_string():
    # float() would round 0.1+0.2-style values; a warehouse decimal that
    # silently loses precision is worse than an error.
    rows = envelope([[decimal.Decimal("12345678901234567890.123456789")]])
    assert rows == [["12345678901234567890.123456789"]]
    json.dumps(rows)


def test_binary_becomes_base64():
    rows = envelope([[b"\x00\xff\x10"]])
    assert rows == [["AP8Q"]]
    json.dumps(rows)


def test_primitives_and_null_are_untouched():
    rows = envelope([[1], [1.5], ["s"], [True], [None]])
    assert rows == [[1], [1.5], ["s"], [True], [None]]
    json.dumps(rows)


def test_nested_struct_array_and_map_are_converted_through():
    d = datetime.date(2026, 1, 2)
    rows = envelope([
        [[d, d]],                      # ARRAY<date>
        [(d, decimal.Decimal("1.5"))],  # STRUCT — a pyspark Row is a tuple
        [{"k": d}],                     # MAP<string,date>
    ])
    assert rows == [
        [["2026-01-02", "2026-01-02"]],
        [["2026-01-02", "1.5"]],
        [{"k": "2026-01-02"}],
    ]
    json.dumps(rows)


def test_an_unknown_object_stringifies_rather_than_dropping_the_socket():
    class Opaque:
        def __str__(self):
            return "opaque-value"

    rows = envelope([[Opaque()]])
    assert rows == [["opaque-value"]]
    json.dumps(rows)


# --- the uint64 recovery path builds rows too ---------------------------------

class FakeArrowTable:
    def __init__(self, rows):
        self._rows = rows

    def to_pylist(self):
        return self._rows


class FakeUint64DF:
    """collect() fails the way DataFusion's uint64 row count does; the
    recovery reads the cached relation through toArrow()."""

    def __init__(self, arrow_rows):
        self.schema = FakeSchema
        self._arrow = FakeArrowTable(arrow_rows)

    def collect(self):
        raise ValueError(UINT64_MSG)

    def toArrow(self):
        return self._arrow


def test_the_uint64_recovery_converts_typed_values_too():
    df = FakeUint64DF([{"v": datetime.date(2026, 8, 18)}])
    out = agent.run_sql("INSERT INTO t VALUES (1)", {"spark": FakeSpark(df)})
    assert out["status"] == "ok", out
    rows = out["data"]["application/json"]["data"]
    assert rows == [["2026-08-18"]]
    json.dumps(rows)


def test_no_spark_session_still_reports_an_error():
    out = agent.run_sql("SELECT 1", {})
    assert out["status"] == "error"
    assert out["ename"] == "NoSparkSession"


class EmptySchema:
    fields = ()  # DDL has no output columns

    @staticmethod
    def jsonValue():  # pragma: no cover - never reached for an empty schema
        raise AssertionError("jsonValue must not be read for a DDL result")


class FakeDDLDF:
    schema = EmptySchema

    @staticmethod
    def collect():  # pragma: no cover - the empty-schema branch returns first
        raise AssertionError("collect must not run for a DDL result")


def test_ddl_returns_an_empty_envelope_without_reading_rows():
    # Closes a gap that predates this change: the empty-schema branch had no
    # test, so nothing pinned that DDL returns an empty envelope rather than
    # falling through to collect().
    out = agent.run_sql("CREATE TABLE t (a int)", {"spark": FakeSpark(FakeDDLDF)})
    assert out == {"status": "ok", "execution_count": 0, "data": {}}
