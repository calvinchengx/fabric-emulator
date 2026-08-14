"""Eventstream kafka wrap rewrites Fabric options to OSS Kafka, never to rate."""
import base64
import io
import json
import sys
import types
from datetime import UTC, datetime
from pathlib import Path
from urllib.error import HTTPError

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import eventstream_kafka as ek  # noqa: E402


def test_partial_options_fail_loud():
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        ek.rewrite_load("kafka", {"eventstream.itemid": "abc"})
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        ek.rewrite_load("kafka", {"eventstream.datasourceid": "def"})


def test_non_kafka_and_native_kafka_are_untouched():
    assert ek.rewrite_load("rate", {"eventstream.itemid": "abc",
                                    "eventstream.datasourceid": "def"}) is None
    assert ek.rewrite_load("kafka", {"kafka.bootstrap.servers": "k:9092",
                                     "subscribe": "t"}) is None
    assert ek.should_rewrite("delta", {"eventstream.itemid": "abc"}) is False


def test_kafka_options_are_oss_kafka_not_rate():
    opts = ek.kafka_options("kafka:9092", "item.ds", {
        "eventstream.itemid": "item",
        "failOnDataLoss": "false",
    })
    assert opts["kafka.bootstrap.servers"] == "kafka:9092"
    assert opts["subscribe"] == "item.ds"
    assert opts["startingOffsets"] == "earliest"
    assert opts["failOnDataLoss"] == "false"
    assert "eventstream.itemid" not in opts
    assert "rowsPerSecond" not in opts
    assert set(opts) >= {"kafka.bootstrap.servers", "subscribe", "startingOffsets"}


def test_resolve_unknown_id_fails_loud(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_token", lambda env=None: "tok")

    def boom(req, timeout=60, context=None):
        body = json.dumps({
            "errorCode": "EventstreamSourceNotFound",
            "message": "The Eventstream datasource id is not available.",
        }).encode()
        raise HTTPError(req.full_url, 404, "Not Found", hdrs={}, fp=io.BytesIO(body))

    monkeypatch.setattr(ek.urllib.request, "urlopen", boom)
    with pytest.raises(ek.EventstreamError, match=r"not available|404"):
        ek.resolve_source("item", "ds", env={"FABRIC_API_URL": "http://fabric"})


def test_consume_unknown_id_fails_loud(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_token", lambda env=None: "tok")

    def boom(req, timeout=60, context=None):
        body = json.dumps({
            "errorCode": "ItemNotFound",
            "message": "The Eventstream item is not available.",
        }).encode()
        raise HTTPError(req.full_url, 404, "Not Found", hdrs={}, fp=io.BytesIO(body))

    monkeypatch.setattr(ek.urllib.request, "urlopen", boom)
    with pytest.raises(ek.EventstreamError, match=r"not available|404"):
        ek.consume_events("item", "ds", env={"FABRIC_API_URL": "http://fabric"})


def test_consume_events_hits_the_events_path(monkeypatch):
    seen = {}

    class FakeResp:
        def read(self):
            return json.dumps({
                "topic": "item.ds",
                "records": [{
                    "key": "a2k=",
                    "value": "eyJuIjoxfQ==",
                    "topic": "item.ds",
                    "partition": 0,
                    "offset": 0,
                    "timestamp": "1970-01-01T00:00:00Z",
                    "timestampType": 0,
                }],
            }).encode()

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

    def fake_urlopen(req, timeout=60, context=None):
        seen["url"] = req.full_url
        return FakeResp()

    monkeypatch.setattr(ek, "_fabric_token", lambda env=None: "tok")
    monkeypatch.setattr(ek.urllib.request, "urlopen", fake_urlopen)
    recs = ek.consume_events("aaaa", "bbbb", env={"FABRIC_API_URL": "http://fabric"})
    assert "/v1/eventstreams/aaaa/sources/bbbb/events" in seen["url"]
    assert "max=100" in seen["url"]
    assert recs[0]["topic"] == "item.ds"


def test_records_to_rows_are_kafka_not_rate():
    rows = ek.records_to_rows([{
        "key": base64.b64encode(b"k0").decode(),
        "value": base64.b64encode(b'{"n":0}').decode(),
        "topic": "item.ds",
        "partition": 0,
        "offset": 3,
        "timestamp": "1970-01-01T00:00:00Z",
        "timestampType": 0,
    }])
    assert len(rows) == 1
    key, value, topic, partition, offset, _ts, ts_type = rows[0]
    assert key == b"k0"
    assert value == b'{"n":0}'
    assert topic == "item.ds"
    assert partition == 0
    assert offset == 3
    assert ts_type == 0
    assert "rate" not in topic


def test_local_foreach_only_for_eventstream_df():
    class Writer:
        _emu_foreach_batch = lambda df, i: None  # noqa: E731

    marked = type("DF", (), {"_emu_eventstream": True})()
    plain = type("DF", (), {})()
    assert ek.should_run_local_foreach(marked, Writer()) is True
    assert ek.should_run_local_foreach(plain, Writer()) is False
    assert ek.should_run_local_foreach(marked, type("W", (), {})()) is False


def test_oneshot_query_is_already_done():
    q = ek.OneShotStreamingQuery(name="clicks")
    assert q.awaitTermination(60) is True
    assert q.isActive is False
    q.stop()
    assert q.isActive is False


def test_rewrite_load_sets_bootstrap_and_subscribe(monkeypatch):
    monkeypatch.setattr(ek, "resolve_source", lambda item, ds, env=None: {
        "bootstrapServers": "kafka:9092",
        "topic": f"{item}.{ds}",
    })
    opts = ek.rewrite_load("kafka", {
        "eventstream.itemid": "aaaa",
        "eventstream.datasourceid": "bbbb",
    })
    assert opts["kafka.bootstrap.servers"] == "kafka:9092"
    assert opts["subscribe"] == "aaaa.bbbb"
    assert "rate" not in str(opts).lower()


def test_fabric_token_uses_fabric_audience(monkeypatch):
    seen = {}

    class FakeResp:
        def read(self):
            return json.dumps({"access_token": "tok", "expires_in": 3600}).encode()

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

    def fake_urlopen(req, timeout=60, context=None):
        seen["url"] = req.full_url
        seen["body"] = req.data.decode()
        return FakeResp()

    monkeypatch.setattr(ek.urllib.request, "urlopen", fake_urlopen)
    token = ek._fabric_token({
        "ENTRA_TOKEN_URL": "http://entra/token",
        "ENTRA_CLIENT_ID": "c",
        "ENTRA_CLIENT_SECRET": "s",
    })
    assert token == "tok"
    assert "api.fabric.microsoft.com" in seen["body"]


def test_fabric_token_requires_entra_url():
    with pytest.raises(ek.EventstreamError, match="ENTRA_TOKEN_URL"):
        ek._fabric_token({})


def test_resolve_source_requires_both_ids():
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        ek.resolve_source("", "ds")


def test_resolve_source_returns_bootstrap_and_topic(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_request", lambda *a, **k: {
        "bootstrapServers": "kafka:9092",
        "topic": "item.ds",
    })
    src = ek.resolve_source("item", "ds")
    assert src == {"bootstrapServers": "kafka:9092", "topic": "item.ds"}


def test_resolve_source_requires_bootstrap_and_topic(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_request", lambda *a, **k: {"topic": "t"})
    with pytest.raises(ek.EventstreamError, match="no bootstrap"):
        ek.resolve_source("item", "ds")


def test_consume_events_requires_both_ids():
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        ek.consume_events("item", "")


def test_consume_events_requires_records_field(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_request", lambda *a, **k: {"topic": "t"})
    with pytest.raises(ek.EventstreamError, match="no records field"):
        ek.consume_events("item", "ds")


def test_http_error_body_need_not_be_json(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_token", lambda env=None: "tok")

    def boom(req, timeout=60, context=None):
        raise HTTPError(req.full_url, 502, "Bad Gateway", hdrs={},
                        fp=io.BytesIO(b"not-json"))

    monkeypatch.setattr(ek.urllib.request, "urlopen", boom)
    with pytest.raises(ek.EventstreamError, match=r"not-json|502"):
        ek.resolve_source("item", "ds", env={"FABRIC_API_URL": "http://fabric"})


def test_kafka_options_do_not_overwrite_core_keys():
    opts = ek.kafka_options("kafka:9092", "item.ds", {"subscribe": "other"})
    assert opts["subscribe"] == "item.ds"


def test_eventstream_ids_accept_camel_case():
    assert ek.eventstream_ids({
        "eventstream.itemId": "A",
        "eventstream.datasourceId": "B",
    }) == ("A", "B")


def test_as_bytes_and_timestamp_shapes():
    rows = ek.records_to_rows([
        None,
        {"key": None, "value": b"raw", "topic": "t", "timestamp": None},
        {"key": bytearray(b"k"), "value": "", "topic": "t",
         "timestamp": datetime(1970, 1, 1, tzinfo=UTC)},
        {"key": "abc", "value": 1, "topic": "t", "timestamp": "nope"},
        {"key": b"x", "value": b"y", "topic": "t",
         "timestamp": datetime(1970, 1, 1)},
    ])
    assert rows[0][0] is None
    assert rows[1][1] == b"raw"
    assert rows[2][0] == b"k"
    assert rows[2][1] == b""
    assert rows[2][5].tzinfo is UTC
    assert rows[3][0] == b"abc"
    assert rows[3][1] == bytes(1)
    assert rows[4][5] == datetime(1970, 1, 1)


def test_records_to_rows_empty():
    assert ek.records_to_rows(None) == []
    assert ek.records_to_rows([]) == []


def test_as_timestamp_accepts_kafka_epoch_millis():
    ts = ek._as_timestamp(1_700_000_000_000)
    assert ts.year == 2023
    assert ek._as_timestamp(None) is None
    assert ek._as_timestamp(datetime(2020, 1, 1)) == datetime(2020, 1, 1)


def test_native_kafka_is_not_an_eventstream_rewrite():
    assert ek.should_consume_native("kafka", {
        "kafka.bootstrap.servers": "k:9092", "subscribe": "t",
    }) is True
    assert ek.should_consume_native("kafka", {
        "eventstream.itemid": "a", "eventstream.datasourceid": "b",
    }) is False
    assert ek.should_consume_native("rate", {
        "kafka.bootstrap.servers": "k:9092", "subscribe": "t",
    }) is False
    assert ek.should_rewrite("kafka", {
        "kafka.bootstrap.servers": "k:9092", "subscribe": "t",
    }) is False


def test_consume_plain_kafka_fails_loud_on_unsupported_options():
    with pytest.raises(ek.KafkaSourceError, match="bootstrap"):
        ek.consume_plain_kafka({"subscribe": "t"})
    with pytest.raises(ek.KafkaSourceError, match="subscribe"):
        ek.consume_plain_kafka({"kafka.bootstrap.servers": "k:9092"})
    with pytest.raises(ek.KafkaSourceError, match="subscribePattern"):
        ek.consume_plain_kafka({
            "kafka.bootstrap.servers": "k:9092",
            "subscribePattern": "clicks-.*",
        })
    with pytest.raises(ek.KafkaSourceError, match="assign"):
        ek.consume_plain_kafka({
            "kafka.bootstrap.servers": "k:9092",
            "assign": '{"t":[0]}',
        })
    with pytest.raises(ek.KafkaSourceError, match="PLAINTEXT"):
        ek.consume_plain_kafka({
            "kafka.bootstrap.servers": "k:9092",
            "subscribe": "t",
            "kafka.security.protocol": "SASL_SSL",
        })
    with pytest.raises(ek.KafkaSourceError, match="includeHeaders"):
        ek.consume_plain_kafka({
            "kafka.bootstrap.servers": "k:9092",
            "subscribe": "t",
            "includeHeaders": "true",
        })
    with pytest.raises(ek.KafkaSourceError, match="startingOffsets"):
        ek.consume_plain_kafka({
            "kafka.bootstrap.servers": "k:9092",
            "subscribe": "t",
            "startingOffsets": '{"t":{"0":0}}',
        })


def test_consume_plain_kafka_passes_bytes_through_poll():
    seen = {}

    def poll(bootstrap, topics, starting, timeout_ms, max_records):
        seen["args"] = (bootstrap, topics, starting, timeout_ms, max_records)
        return [{"key": b"k", "value": b"hello", "topic": topics[0]}]

    recs = ek.consume_plain_kafka({
        "kafka.bootstrap.servers": "kafka:9092",
        "subscribe": "plain, other",
        "startingOffsets": "earliest",
        "maxOffsetsPerTrigger": "7",
        "kafkaConsumer.pollTimeoutMs": "1234",
    }, poll=poll)
    assert seen["args"] == ("kafka:9092", ["plain", "other"], "earliest", 1234, 7)
    assert recs[0]["value"] == b"hello"
    rows = ek.records_to_rows(recs)
    assert rows[0][0] == b"k"
    assert rows[0][1] == b"hello"
    assert rows[0][2] == "plain"


def test_kafka_poll_maps_consumer_bytes(monkeypatch):
    class Msg:
        key = b"k"
        value = b"hello-engine-matrix"
        topic = "t"
        partition = 0
        offset = 3
        timestamp = 1_700_000_000_000
        timestamp_type = 0

    assigned = {}

    class Consumer:
        def __init__(self, **kw):
            pass

        def partitions_for_topic(self, topic):
            return {0}

        def assign(self, tps):
            assigned["tps"] = tps

        def seek_to_beginning(self, *tps):
            assigned["seek"] = "beginning"

        def seek_to_end(self, *tps):
            assigned["seek"] = "end"

        def __iter__(self):
            return iter([Msg()])

        def close(self):
            assigned["closed"] = True

    class TP:
        def __init__(self, topic, p):
            self.topic, self.partition = topic, p

    kafka_mod = types.ModuleType("kafka")
    kafka_mod.KafkaConsumer = Consumer
    kafka_mod.TopicPartition = TP
    monkeypatch.setitem(sys.modules, "kafka", kafka_mod)
    rows = ek._kafka_poll("k:9092", ["t"], "earliest", 500, 10)
    assert rows[0]["value"] == b"hello-engine-matrix"
    assert assigned["seek"] == "beginning" and assigned["closed"] is True
    rows = ek._kafka_poll("k:9092", ["t"], "latest", 500, 10)
    assert rows == []
    assert assigned["seek"] == "end"


def test_connect_kafka_df_native_uses_the_spark_session(monkeypatch, capsys):
    """Bytes go through spark.createDataFrame — that is the Sail LocalRelation."""
    monkeypatch.setattr(
        ek, "consume_plain_kafka",
        lambda opts: [{"key": b"k", "value": b"payload", "topic": "t"}],
    )
    monkeypatch.setattr(ek, "kafka_schema", lambda: "kafka-schema")
    created = []

    class Spark:
        def createDataFrame(self, rows, schema):
            created.append((rows, schema))
            return types.SimpleNamespace(rows=rows, schema=schema)

    spark = Spark()
    df = ek.connect_kafka_df(spark, "kafka", {
        "kafka.bootstrap.servers": "k:9092", "subscribe": "t",
    })
    assert created and created[0][1] == "kafka-schema"
    assert created[0][0][0][1] == b"payload"
    assert df._emu_eventstream is True
    assert "driver consume" in capsys.readouterr().err
    assert ek.connect_kafka_df(spark, "rate", {}) is None


def test_oneshot_query_explain_and_process(capsys):
    q = ek.OneShotStreamingQuery()
    q.processAllAvailable()
    q.explain()
    assert ek.FOREACH_ANNOUNCE in capsys.readouterr().out


def test_kafka_schema_is_kafka_not_rate(monkeypatch):
    types_mod = types.ModuleType("pyspark.sql.types")

    class Field:
        def __init__(self, name, typ, nullable):
            self.name, self.dataType, self.nullable = name, typ, nullable

    class Struct:
        def __init__(self, fields):
            self.fields = fields

    types_mod.BinaryType = lambda: "binary"
    types_mod.IntegerType = lambda: "int"
    types_mod.LongType = lambda: "long"
    types_mod.StringType = lambda: "string"
    types_mod.TimestampType = lambda: "timestamp"
    types_mod.StructField = Field
    types_mod.StructType = Struct
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.types", types_mod)
    schema = ek.kafka_schema()
    names = [f.name for f in schema.fields]
    assert names == ["key", "value", "topic", "partition", "offset",
                     "timestamp", "timestampType"]
    assert "rate" not in names


def test_materialize_marks_the_dataframe(monkeypatch):
    monkeypatch.setattr(ek, "kafka_schema", lambda: "schema")

    class Spark:
        def createDataFrame(self, rows, schema):
            return types.SimpleNamespace(rows=rows, schema=schema)

    df = ek.materialize_kafka_df(Spark(), [{"value": b"x", "topic": "t"}])
    assert df._emu_eventstream is True
    assert df.schema == "schema"


@pytest.fixture
def reset_install():
    ek._classic_installed = False
    ek._connect_installed = False
    yield
    ek._classic_installed = False
    ek._connect_installed = False


def test_classic_install_skips_when_remote(monkeypatch, reset_install):
    monkeypatch.setenv("SPARK_REMOTE", "sc://sail:50051")
    assert ek._install_classic() is False


def test_classic_install_rewrites_eventstream_options(monkeypatch, reset_install):
    monkeypatch.delenv("SPARK_REMOTE", raising=False)
    applied = []

    class DataStreamReader:
        def format(self, source):
            return self

        def option(self, key, value):
            applied.append((key, value))
            return self

        def options(self, **kwargs):
            return self

        def load(self, path=None, format=None, schema=None, **kwargs):
            return "df"

    streaming = types.ModuleType("pyspark.sql.streaming")
    streaming.DataStreamReader = DataStreamReader
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.streaming", streaming)
    monkeypatch.setattr(ek, "rewrite_load", lambda fmt, opts: {
        "kafka.bootstrap.servers": "kafka:9092",
        "subscribe": "t",
        "startingOffsets": "earliest",
    } if ek.should_rewrite(fmt, opts) else None)

    spark = types.SimpleNamespace(read=None)
    assert ek.install(spark) is True
    assert ek._install_classic() is True

    r = DataStreamReader()
    r.format("kafka")
    r.option("eventstream.itemid", "aaaa")
    r.option("eventstream.datasourceid", "bbbb")
    r.option("failOnDataLoss", "false")
    r.options(foo="bar")
    assert r.load() == "df"
    keys = [k for k, _ in applied]
    assert "kafka.bootstrap.servers" in keys
    assert "subscribe" in keys
    assert "failOnDataLoss" in keys
    assert "foo" in keys
    assert "eventstream.itemid" not in keys

    native = DataStreamReader()
    native.format("kafka")
    native.option("subscribe", "plain")
    assert native.load() == "df"


def test_connect_install_runs_foreach_locally(monkeypatch, reset_install, capsys):
    monkeypatch.setenv("SPARK_REMOTE", "sc://sail:50051")
    engine = {"load": 0, "foreach": 0, "start": 0}

    class DataFrameReader:
        def __init__(self):
            self._format = None
            self._options = {}

        def format(self, source):
            self._format = source
            return self

        def load(self, path=None, format=None, schema=None, **kwargs):
            engine["batch"] = engine.get("batch", 0) + 1
            return "engine-batch"

    class DataStreamReader:
        def __init__(self):
            self._format = None
            self._options = {}
            self._client = object()

        def format(self, source):
            self._format = source
            return self

        def load(self, path=None, format=None, schema=None, **kwargs):
            engine["load"] += 1
            return "engine-df"

    class DataStreamWriter:
        def foreachBatch(self, func):
            engine["foreach"] += 1
            return self

        def start(self, path=None, format=None, outputMode=None, partitionBy=None,
                  queryName=None, **options):
            engine["start"] += 1
            return "engine-query"

    class ConnectDF:
        def __init__(self, marked=False):
            self._emu_eventstream = marked

        @property
        def writeStream(self):
            return DataStreamWriter()

    streaming = types.ModuleType("pyspark.sql.connect.streaming.readwriter")
    streaming.DataStreamReader = DataStreamReader
    streaming.DataStreamWriter = DataStreamWriter
    rw = types.ModuleType("pyspark.sql.connect.readwriter")
    rw.DataFrameReader = DataFrameReader
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

    marked = types.SimpleNamespace(_emu_eventstream=True)
    monkeypatch.setattr(ek, "consume_events", lambda *a, **k: [{"topic": "t"}])
    monkeypatch.setattr(ek, "materialize_kafka_df", lambda spark, recs: marked)

    spark = types.SimpleNamespace(read=DataFrameReader())
    assert ek.install(spark) is True
    assert ek._install_connect(spark) is True

    r = DataStreamReader()
    r.format("kafka")
    r._options = {"eventstream.itemid": "a", "eventstream.datasourceid": "b"}
    df = r.load()
    assert df is marked
    assert engine["load"] == 0
    err = capsys.readouterr().err
    assert "LocalRelation" in err

    r_kw = DataStreamReader()
    assert r_kw.load(format="kafka", **{
        "eventstream.itemid": "a", "eventstream.datasourceid": "b",
    }) is marked

    r2 = DataStreamReader()
    r2._format = "kafka"
    r2._options = {"eventstream.itemid": "only"}
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        r2.load()

    r3 = DataStreamReader()
    r3._format = "rate"
    assert r3.load() == "engine-df"
    assert engine["load"] == 1

    seen = []
    w = DataStreamWriter()
    w._emu_stream_df = marked
    assert w.foreachBatch(lambda batch, i: seen.append((batch, i))) is w
    assert engine["foreach"] == 0
    q = w.start(queryName="clicks")
    assert seen == [(marked, 0)]
    assert q.isActive is False
    assert q.name == "clicks"
    assert engine["start"] == 0

    bound = ConnectDF(marked=True)
    ws = bound.writeStream
    assert ws._emu_stream_df is bound

    plain = DataStreamWriter()
    plain._emu_stream_df = ConnectDF()
    plain.foreachBatch(lambda *a: None)
    assert engine["foreach"] == 1
    assert plain.start() == "engine-query"
    assert engine["start"] == 1

    monkeypatch.setattr(
        ek, "consume_plain_kafka",
        lambda opts: [{"value": b"hello", "topic": opts["subscribe"]}],
    )
    n = DataStreamReader()
    n.format("kafka")
    n._options = {"kafka.bootstrap.servers": "kafka:9092", "subscribe": "plain"}
    assert n.load() is marked
    assert engine["load"] == 1
    assert "driver consume" in capsys.readouterr().err

    b = DataFrameReader()
    b.format("kafka")
    b._options = {"kafka.bootstrap.servers": "k:9092", "subscribe": "plain"}
    assert b.load() is marked
    assert engine.get("batch", 0) == 0


def test_connect_install_skips_non_connect_reader(monkeypatch, reset_install):
    monkeypatch.setenv("SPARK_REMOTE", "sc://x")

    class DataFrameReader:
        pass

    streaming = types.ModuleType("pyspark.sql.connect.streaming.readwriter")
    streaming.DataStreamReader = type("DataStreamReader", (), {})
    streaming.DataStreamWriter = type("DataStreamWriter", (), {})
    rw = types.ModuleType("pyspark.sql.connect.readwriter")
    rw.DataFrameReader = DataFrameReader
    dfmod = types.ModuleType("pyspark.sql.connect.dataframe")
    dfmod.DataFrame = type("DataFrame", (), {})
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect", types.ModuleType("pyspark.sql.connect"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.readwriter", rw)
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.dataframe", dfmod)
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming",
                        types.ModuleType("pyspark.sql.connect.streaming"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming.readwriter", streaming)
    spark = types.SimpleNamespace(read=object())
    assert ek._install_connect(spark) is False
    monkeypatch.setenv("SPARK_REMOTE", "sc://x")
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming.readwriter", None)
    spark = types.SimpleNamespace(read=object())
    assert ek._install_connect(spark) is False
    assert ek.install(spark) is False
