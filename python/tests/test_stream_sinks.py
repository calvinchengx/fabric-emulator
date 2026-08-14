"""Durable streaming sinks are pulled from Sail and batch-written, not left to fail.

The engine-matrix probes start `writeStream.format(delta|parquet).option(path)`
(and memory + queryName) and assert rows are readable. Sail cannot land those
sinks. foreachBatch never fires. These tests pin the named wrap — one bounded
collect, then a batch write — and pin everything else falling through.
"""
import io
import sys
import types
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import stream_sinks as ss  # noqa: E402


class FakeRow:
    def __init__(self, data):
        self._data = dict(data)

    def asDict(self, recursive=True):
        return dict(self._data)


class StreamDF:
    def __init__(self, rows):
        self.rows = list(rows)
        self.limits = []

    def limit(self, n):
        self.limits.append(n)
        return types.SimpleNamespace(collect=lambda: list(self.rows[:n]))


class WriteChain:
    def __init__(self, sink):
        self.sink = sink
        self.fmt = None
        self.saved_mode = None
        self.parts = None
        self.path = None

    def format(self, fmt):
        self.fmt = fmt
        return self

    def mode(self, mode):
        self.saved_mode = mode
        return self

    def partitionBy(self, *cols):
        self.parts = list(cols)
        return self

    def save(self, path):
        self.path = path
        self.sink.saves.append(self)


class BatchDF:
    def __init__(self, spark, rows):
        self.spark = spark
        self.rows = rows
        self.view = None

    @property
    def write(self):
        return WriteChain(self.spark)

    def createOrReplaceTempView(self, name):  # noqa: N802 — pyspark's name
        self.view = name
        self.spark.views[name] = self.rows


class FakeSpark:
    def __init__(self):
        self.frames = []
        self.saves = []
        self.views = {}
        self.read = types.SimpleNamespace()

    def createDataFrame(self, data, schema=None):  # noqa: N802 — pyspark's name
        self.frames.append(data)
        return BatchDF(self, data)


def _state(**kw):
    base = {
        "format": "delta",
        "path": "/tmp/s_delta",
        "query_name": "",
        "output_mode": "append",
        "partition_by": [],
        "options": {"path": "/tmp/s_delta", "checkpointLocation": "/tmp/s_delta_ck"},
        "foreach": False,
    }
    base.update(kw)
    return base


# --- intercept decision -------------------------------------------------------

def test_should_intercept_delta_and_parquet_with_a_path():
    assert ss.should_intercept(_state(format="delta"))
    assert ss.should_intercept(_state(format="parquet", path="/tmp/s_pq",
                                      options={"path": "/tmp/s_pq"}))
    assert ss.should_intercept(_state(format="DELTA", path="/tmp/t"))


def test_should_intercept_memory_with_a_query_name():
    assert ss.should_intercept(_state(
        format="memory", path="", query_name="probe_mem", options={}))


def test_should_not_intercept_without_a_destination():
    assert not ss.should_intercept(_state(format="delta", path="", options={}))
    assert not ss.should_intercept(_state(
        format="parquet", path="", options={"checkpointLocation": "/ck"}))
    assert not ss.should_intercept(_state(
        format="memory", path="", query_name="", options={}))


def test_console_kafka_and_csv_fall_through():
    for fmt in ("console", "kafka", "csv", "orc", ""):
        assert not ss.should_intercept(_state(format=fmt)), fmt


def test_eventstream_options_fall_through_even_on_delta():
    assert not ss.should_intercept(_state(options={
        "path": "/tmp/t",
        "eventstream.itemid": "abc",
        "eventstream.datasourceid": "def",
    }))


def test_foreach_batch_falls_through():
    assert not ss.should_intercept(_state(foreach=True))


def test_complete_output_mode_falls_through():
    assert not ss.should_intercept(_state(output_mode="complete"))
    assert not ss.should_intercept(_state(output_mode="update"))


def test_append_is_the_default_output_mode():
    assert ss.should_intercept(_state(output_mode=""))
    assert ss.should_intercept(_state(output_mode="append"))


# --- snapshot from a Connect-shaped writer ------------------------------------

def test_snapshot_reads_proto_format_path_and_query_name():
    writer = types.SimpleNamespace(_write_proto=types.SimpleNamespace(
        format="parquet",
        options={"path": "/tmp/s_pq", "checkpointLocation": "/ck"},
        path="",
        query_name="",
        output_mode="append",
        partitioning_column_names=[],
    ))
    state = ss.snapshot(writer)
    assert state["format"] == "parquet"
    assert state["path"] == "/tmp/s_pq"
    assert ss.should_intercept(state)


def test_snapshot_prefers_start_kwargs_over_proto():
    writer = types.SimpleNamespace(_write_proto=types.SimpleNamespace(
        format="parquet",
        options={"path": "/from-option"},
        path="",
        query_name="old",
        output_mode="append",
        partitioning_column_names=[],
    ))
    state = ss.snapshot(writer, path="/from-start", format="delta", queryName="q")
    assert state["format"] == "delta"
    assert state["path"] == "/from-start"
    assert state["query_name"] == "q"


def test_snapshot_detects_foreach_batch_via_hasfield():
    proto = types.SimpleNamespace(
        format="delta",
        options={"path": "/tmp/t"},
        path="",
        query_name="",
        output_mode="append",
        partitioning_column_names=[],
        HasField=lambda name: name == "foreach_batch",
    )
    state = ss.snapshot(types.SimpleNamespace(_write_proto=proto))
    assert state["foreach"] is True
    assert not ss.should_intercept(state)


def test_snapshot_detects_foreach_via_python_function():
    proto = types.SimpleNamespace(
        format="delta",
        options={"path": "/tmp/t"},
        path="",
        query_name="",
        output_mode="",
        partitioning_column_names=[],
        foreach_batch=types.SimpleNamespace(python_function=b"pickled"),
        foreach_writer=None,
    )
    assert ss.snapshot(types.SimpleNamespace(_write_proto=proto))["foreach"] is True


def test_snapshot_hasfield_valueerror_falls_back_to_attributes():
    proto = types.SimpleNamespace(
        format="delta",
        options={"path": "/tmp/t"},
        path="",
        query_name="",
        output_mode="append",
        partitioning_column_names=[],
        HasField=lambda name: (_ for _ in ()).throw(ValueError("no such field")),
        foreach_batch=None,
        foreach_writer=None,
    )
    assert ss.snapshot(types.SimpleNamespace(_write_proto=proto))["foreach"] is False


# --- pull: Sail rows, not invented ones ---------------------------------------

def test_pull_rows_uses_limit_and_returns_collected_dicts():
    stream = StreamDF([FakeRow({"timestamp": "t0", "value": 0}),
                       FakeRow({"timestamp": "t1", "value": 1})])
    got = ss.pull_rows(stream, n=10)
    assert stream.limits == [10]
    assert got == [{"timestamp": "t0", "value": 0},
                   {"timestamp": "t1", "value": 1}]


def test_pull_rows_strips_flow_event_columns():
    stream = StreamDF([FakeRow({
        "timestamp": "t0", "value": 0, "_marker": 1, "_retracted": False,
    })])
    assert ss.pull_rows(stream) == [{"timestamp": "t0", "value": 0}]


def test_pull_rows_accepts_plain_dicts():
    stream = StreamDF([{"value": 7}])
    assert ss.pull_rows(stream) == [{"value": 7}]


def test_pull_without_a_stream_is_an_error():
    with pytest.raises(ss.StreamSinkError, match="no streaming DataFrame"):
        ss.pull_rows(None)


# --- land: delta and parquet both, and memory ---------------------------------

def test_land_delta_appends_collected_rows(tmp_path):
    spark = FakeSpark()
    rows = [{"timestamp": "t0", "value": 0}]
    ss.land_batch(spark, rows, _state(format="delta", path=str(tmp_path / "t")))
    assert spark.frames == [rows]
    assert len(spark.saves) == 1
    save = spark.saves[0]
    assert save.fmt == "delta" and save.saved_mode == "append"
    assert save.path == str(tmp_path / "t")
    assert save.parts is None


def test_land_parquet_appends_collected_rows(tmp_path):
    spark = FakeSpark()
    rows = [{"timestamp": "t0", "value": 1}, {"timestamp": "t1", "value": 2}]
    ss.land_batch(spark, rows, _state(format="parquet", path=str(tmp_path / "p")))
    save = spark.saves[0]
    assert save.fmt == "parquet" and save.saved_mode == "append"
    assert save.path == str(tmp_path / "p")
    assert spark.frames[0] == rows


def test_land_honours_partition_by_on_both_file_sinks(tmp_path):
    for fmt in ("delta", "parquet"):
        spark = FakeSpark()
        ss.land_batch(spark, [{"k": 1, "v": 2}], _state(
            format=fmt, path=str(tmp_path / fmt), partition_by=["k"]))
        assert spark.saves[0].parts == ["k"]
        assert spark.saves[0].fmt == fmt


def test_land_memory_registers_a_temp_view():
    spark = FakeSpark()
    rows = [{"value": 3}]
    ss.land_batch(spark, rows, _state(
        format="memory", path="", query_name="probe_mem", options={}))
    assert spark.saves == []
    assert spark.views["probe_mem"] == rows


def test_empty_pull_does_not_write():
    spark = FakeSpark()
    ss.land_batch(spark, [], _state())
    assert spark.frames == [] and spark.saves == [] and spark.views == {}


# --- run: announce, pull, land, stand-in query --------------------------------

def test_run_pulled_sink_delta_announces_and_returns_an_active_query():
    spark = FakeSpark()
    stream = StreamDF([FakeRow({"value": 0})])
    err = io.StringIO()
    query = ss.run_pulled_sink(stream, spark, _state(format="delta"), file=err)
    assert "Sail collect + batch write (delta" in err.getvalue()
    assert "no checkpoint" in err.getvalue()
    assert query.isActive is True
    query.stop()
    assert query.isActive is False
    assert spark.saves[0].fmt == "delta"


def test_run_pulled_sink_parquet_lands_the_collected_rows():
    spark = FakeSpark()
    stream = StreamDF([FakeRow({"value": 9})])
    query = ss.run_pulled_sink(
        stream, spark, _state(format="parquet", path="/tmp/s_pq"),
        file=io.StringIO())
    assert spark.frames == [[{"value": 9}]]
    assert spark.saves[0].fmt == "parquet"
    assert query.awaitTermination() is True
    assert query.processAllAvailable() is None
    query.explain()


def test_probe_shaped_delta_and_parquet_both_land_rate_rows(tmp_path):
    """The engine-matrix sink probes: rate stream, path + checkpoint, append."""
    rows = [FakeRow({"timestamp": "2026-08-14", "value": i}) for i in range(3)]
    for fmt, name in (("delta", "s_delta"), ("parquet", "s_pq")):
        spark = FakeSpark()
        path = str(tmp_path / name)
        state = _state(
            format=fmt, path=path,
            options={"path": path, "checkpointLocation": path + "_ck"},
        )
        query = ss.run_pulled_sink(StreamDF(rows), spark, state, file=io.StringIO())
        try:
            assert spark.saves[0].fmt == fmt
            assert spark.saves[0].path == path
            assert [r["value"] for r in spark.frames[0]] == [0, 1, 2]
            assert "_marker" not in spark.frames[0][0]
        finally:
            query.stop()


# --- patch: intercept start, leave the rest on the engine ---------------------

def _fake_connect_writer(monkeypatch, DataStreamWriter, ConnectDF, DataFrameReader=None):
    streaming = types.ModuleType("pyspark.sql.connect.streaming.readwriter")
    streaming.DataStreamWriter = DataStreamWriter
    rw = types.ModuleType("pyspark.sql.connect.readwriter")
    rw.DataFrameReader = DataFrameReader or type("DataFrameReader", (), {})
    dfmod = types.ModuleType("pyspark.sql.connect.dataframe")
    dfmod.DataFrame = ConnectDF
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect", types.ModuleType("pyspark.sql.connect"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.readwriter", rw)
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.dataframe", dfmod)
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming",
                        types.ModuleType("pyspark.sql.connect.streaming"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming.readwriter", streaming)


def test_patch_intercepts_delta_and_parquet_start_and_skips_the_engine(monkeypatch):
    engine_starts = []

    class DataStreamWriter:
        def __init__(self):
            self._write_proto = types.SimpleNamespace(
                format="",
                options={},
                path="",
                query_name="",
                output_mode="append",
                partitioning_column_names=[],
            )
            self._session = None

        def start(self, path=None, format=None, outputMode=None, partitionBy=None,
                  queryName=None, **options):
            engine_starts.append(format)
            return "engine-query"

    class ConnectDF(StreamDF):
        def __init__(self):
            super().__init__([FakeRow({"value": 1})])

        @property
        def writeStream(self):
            return DataStreamWriter()

    spark = FakeSpark()
    ss.patch_write_stream(DataStreamWriter, ConnectDF, spark)
    for fmt, path in (("delta", "/tmp/s_delta"), ("parquet", "/tmp/s_pq")):
        q = ConnectDF().writeStream.start(path=path, format=fmt)
        assert q.isActive is True
        assert engine_starts == []
        assert spark.saves[-1].fmt == fmt
        assert spark.saves[-1].path == path
        q.stop()


def test_patch_leaves_console_and_kafka_on_the_engine(monkeypatch):
    class DataStreamWriter:
        def __init__(self):
            self._write_proto = types.SimpleNamespace(
                format="console",
                options={},
                path="",
                query_name="",
                output_mode="append",
                partitioning_column_names=[],
            )
            self._session = None

        def start(self, path=None, format=None, **kw):
            return "engine-query"

    class ConnectDF:
        @property
        def writeStream(self):
            return DataStreamWriter()

        def limit(self, n):
            raise AssertionError("must not pull when falling through")

    spark = FakeSpark()
    ss.patch_write_stream(DataStreamWriter, ConnectDF, spark)
    assert ConnectDF().writeStream.start() == "engine-query"
    w = ConnectDF().writeStream
    w._write_proto.format = "kafka"
    assert w.start() == "engine-query"
    assert spark.saves == []


def test_patch_is_idempotent():
    class DataStreamWriter:
        def start(self, *a, **k):
            return "engine"

    class ConnectDF:
        @property
        def writeStream(self):
            return DataStreamWriter()

    ss.patch_write_stream(DataStreamWriter, ConnectDF, FakeSpark())
    first = DataStreamWriter.start
    ss.patch_write_stream(DataStreamWriter, ConnectDF, FakeSpark())
    assert DataStreamWriter.start is first


def test_install_is_a_noop_without_a_connect_reader():
    spark = FakeSpark()
    assert ss.install(spark) is False


def test_install_patches_a_connect_writer(monkeypatch):
    class DataFrameReader:
        pass

    class DataStreamWriter:
        def __init__(self):
            self._write_proto = types.SimpleNamespace(
                format="delta",
                options={"path": "/tmp/t"},
                path="",
                query_name="",
                output_mode="append",
                partitioning_column_names=[],
            )
            self._session = None

        def start(self, path=None, format=None, **kw):
            return "engine"

    class ConnectDF(StreamDF):
        def __init__(self):
            super().__init__([FakeRow({"value": 4})])

        @property
        def writeStream(self):
            return DataStreamWriter()

    _fake_connect_writer(monkeypatch, DataStreamWriter, ConnectDF, DataFrameReader)
    spark = FakeSpark()
    spark.read = DataFrameReader()
    spark._session = spark
    assert ss.install(spark) is True
    q = ConnectDF().writeStream.start()
    assert q.isActive is True
    assert spark.saves[0].fmt == "delta"


def test_delta_ops_install_installs_stream_sinks(monkeypatch):
    import delta_ops as d

    seen = []
    monkeypatch.setattr(ss, "install", lambda s: seen.append(s))
    # json_multiline.install also runs; leave it.
    spark = types.SimpleNamespace(sql=lambda q: q, read=None)
    d.install(spark, storage_options={})
    assert seen == [spark]


def test_pulled_query_stop_is_the_only_thing_that_clears_isactive():
    q = ss.PulledStreamingQuery(name="probe_mem")
    assert q.name == "probe_mem" and q.isActive
    q.awaitTermination(1)
    assert q.isActive
    q.stop()
    assert not q.isActive


def test_as_dict_accepts_a_mapping_without_items():
    class Map:
        def __init__(self):
            self._d = {"path": "/tmp/t"}

        def __iter__(self):
            return iter(self._d)

        def __getitem__(self, k):
            return self._d[k]

    writer = types.SimpleNamespace(_write_proto=types.SimpleNamespace(
        format="delta",
        options=Map(),
        path="",
        query_name="",
        output_mode="append",
        partitioning_column_names="date",
    ))
    state = ss.snapshot(writer)
    assert state["path"] == "/tmp/t"
    assert state["partition_by"] == ["date"]


def test_snapshot_without_proto_uses_start_kwargs():
    state = ss.snapshot(types.SimpleNamespace(), path="/tmp/t", format="delta")
    assert state["foreach"] is False
    assert ss.should_intercept(state)


def test_as_dict_falls_back_when_keys_are_not_lookupable():
    writer = types.SimpleNamespace(_write_proto=types.SimpleNamespace(
        format="delta",
        options=[("path", "/tmp/pairs")],
        path="",
        query_name="",
        output_mode="append",
        partitioning_column_names=[],
    ))
    assert ss.snapshot(writer)["path"] == "/tmp/pairs"


def test_row_as_dict_accepts_a_sequence_of_pairs():
    assert ss.row_as_dict([("value", 1), ("_marker", True)]) == {"value": 1}


def test_patch_accepts_writeStream_as_a_method():
    class DataStreamWriter:
        def __init__(self):
            self._write_proto = types.SimpleNamespace(
                format="console", options={}, path="", query_name="",
                output_mode="append", partitioning_column_names=[],
            )
            self._session = None

        def start(self, path=None, format=None, **kw):
            return "engine-query"

    class ConnectDF:
        def writeStream(self):
            return DataStreamWriter()

    ss.patch_write_stream(DataStreamWriter, ConnectDF, FakeSpark())
    assert ConnectDF().writeStream.start() == "engine-query"


def test_install_returns_false_when_pyspark_connect_is_missing(monkeypatch):
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming.readwriter", None)
    assert ss.install(FakeSpark()) is False
