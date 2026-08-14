"""Fabric Eventstream options → Kafka records, on JVM Spark and on Sail.

Microsoft's notebook API is:

    spark.readStream.format("kafka").options(**{
        "eventstream.itemid": item_id,
        "eventstream.datasourceid": datasource_id,
    }).load()

    df_raw.writeStream.foreachBatch(show_df).outputMode("append").start()

The closed-source Fabric adapter uses the notebook Entra token to resolve
those IDs to a Kafka endpoint. User code has no bootstrap servers. The schema
is Kafka's (`key`, `value`, `topic`, `partition`, `offset`, `timestamp`) —
never `rate` (`timestamp`, `value`). Mapping Eventstream onto `rate` would
paint the engine matrix green with the wrong columns; this module does not.

Two engines, one notebook snippet:

- **JVM** (`SPARK_REMOTE` unset): rewrite `eventstream.*` to
  `kafka.bootstrap.servers` + `subscribe` on the real OSS Kafka source.
- **Sail** (Connect): the engine has no Kafka source and rejects
  `foreachBatch` at `start()`. The wrap consumes through the emulator
  (`GET …/sources/{ds}/events`), materialises a LocalRelation with the Kafka
  schema, and runs `foreachBatch` in this process. One micro-batch, announced,
  no checkpoint — the same class of wrap as CDF / JSON `multiLine`.

Native `format("kafka")` with `kafka.bootstrap.servers` + `subscribe` is
honoured on Sail too: the agent consumes the topic (kafka-python) and
`createDataFrame`s the Kafka-schema rows into Sail. Subsequent
`select`/`filter`/SQL run on the engine. One micro-batch, announced, no
checkpoint — the same class of wrap as CDF / JSON `multiLine`. JVM keeps
the jar (this module does not rewrite native options on a classic session).

`stream_sinks.py` still does not intercept a kafka *sink* or `foreachBatch`
on an engine stream. Mapping Kafka onto `rate` stays forbidden.
"""
from __future__ import annotations

import base64
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import UTC, datetime


class EventstreamError(RuntimeError):
    """The wrap recognised Eventstream options but could not honour them."""


class KafkaSourceError(RuntimeError):
    """OSS format('kafka') on Sail was recognised but cannot be honoured."""


ITEM_KEYS = ("eventstream.itemid", "eventstream.itemId")
DATASOURCE_KEYS = ("eventstream.datasourceid", "eventstream.datasourceId")
ANNOUNCE = "[eventstream] kafka source via emulator consume (LocalRelation, one micro-batch)"
NATIVE_ANNOUNCE = (
    "[kafka] OSS format(kafka) via driver consume "
    "(LocalRelation → Sail, one micro-batch)"
)
FOREACH_ANNOUNCE = "[eventstream] foreachBatch in the agent (Sail cannot pickle UDFs)"
NATIVE_OPTION_KEYS = (
    "kafka.bootstrap.servers",
    "subscribe",
    "subscribepattern",
    "assign",
)

_classic_installed = False
_connect_installed = False


def _opt(options, names):
    lower = {str(k).lower(): v for k, v in (options or {}).items()}
    for name in names:
        value = lower.get(name.lower())
        if value is not None and str(value).strip():
            return str(value).strip()
    return ""


def eventstream_ids(options) -> tuple[str, str]:
    return _opt(options, ITEM_KEYS), _opt(options, DATASOURCE_KEYS)


def should_rewrite(fmt, options) -> bool:
    """True when this is a kafka read carrying at least one Eventstream id."""
    if (fmt or "").strip().lower() != "kafka":
        return False
    item, ds = eventstream_ids(options)
    return bool(item or ds)


def kafka_options(bootstrap: str, topic: str, extra=None) -> dict:
    """OSS Kafka source options. startingOffsets=earliest so a notebook that
    starts after a Custom produce still sees the records (Fabric's first
    read of a stream behaves the same way for the sample)."""
    out = {
        "kafka.bootstrap.servers": bootstrap,
        "subscribe": topic,
        "startingOffsets": "earliest",
    }
    for key, value in (extra or {}).items():
        if str(key).lower().startswith("eventstream."):
            continue
        if key in out:
            continue
        out[key] = value
    return out


def _fabric_token(env=None):
    env = os.environ if env is None else env
    url = env.get("ENTRA_TOKEN_URL")
    if not url:
        raise EventstreamError(
            "eventstream kafka adapter needs ENTRA_TOKEN_URL to mint a Fabric token"
        )
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": env["ENTRA_CLIENT_ID"],
        "client_secret": env["ENTRA_CLIENT_SECRET"],
        "scope": env.get("ENTRA_FABRIC_SCOPE", "https://api.fabric.microsoft.com/.default"),
    }).encode()
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    req = urllib.request.Request(url, data=form)
    with urllib.request.urlopen(req, timeout=30, context=ctx) as response:
        return json.loads(response.read())["access_token"]


def _fabric_request(path, env=None, query=None):
    """Entra-gated GET. Unknown IDs and a missing broker fail loudly."""
    env = os.environ if env is None else env
    base = (env.get("FABRIC_API_URL") or "http://api.fabric.microsoft.com").rstrip("/")
    url = base + path
    if query:
        url += "?" + urllib.parse.urlencode(query)
    token = _fabric_token(env)
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(req, timeout=60, context=ctx) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as err:
        detail = err.read().decode("utf-8", "replace")
        try:
            parsed = json.loads(detail)
            detail = parsed.get("message") or parsed.get("errorCode") or detail
        except json.JSONDecodeError:
            pass
        raise EventstreamError(
            f"Eventstream {path} failed ({err.code}): {detail}"
        ) from err


def resolve_source(item_id, datasource_id, env=None):
    """Entra-gated lookup. Unknown IDs and a missing broker fail loudly."""
    item_id = (item_id or "").strip()
    datasource_id = (datasource_id or "").strip()
    if not item_id or not datasource_id:
        raise EventstreamError(
            "both eventstream.itemid and eventstream.datasourceid are required"
        )
    body = _fabric_request(
        f"/v1/eventstreams/{item_id}/sources/{datasource_id}", env=env,
    )
    bootstrap = body.get("bootstrapServers")
    topic = body.get("topic")
    if not bootstrap or not topic:
        raise EventstreamError(
            f"Eventstream resolve returned no bootstrap/topic: {body!r}"
        )
    return {"bootstrapServers": bootstrap, "topic": topic}


def consume_events(item_id, datasource_id, env=None, max_records=100, timeout_ms=8000):
    """Pull Kafka-shaped records through the control plane (Sail has no client)."""
    item_id = (item_id or "").strip()
    datasource_id = (datasource_id or "").strip()
    if not item_id or not datasource_id:
        raise EventstreamError(
            "both eventstream.itemid and eventstream.datasourceid are required"
        )
    body = _fabric_request(
        f"/v1/eventstreams/{item_id}/sources/{datasource_id}/events",
        env=env,
        query={"max": str(max_records), "timeoutMs": str(timeout_ms)},
    )
    recs = body.get("records")
    if recs is None:
        raise EventstreamError(
            f"Eventstream consume returned no records field: {body!r}"
        )
    return recs


def rewrite_load(fmt, options) -> dict | None:
    """Return Kafka options to apply, or None to leave the reader alone.

    Raises EventstreamError when the wrap recognised the API but cannot
    honour it (partial options, unknown IDs, no broker).
    """
    options = {str(k): v for k, v in (options or {}).items()}
    if not should_rewrite(fmt, options):
        return None
    item, ds = eventstream_ids(options)
    if not item or not ds:
        raise EventstreamError(
            "both eventstream.itemid and eventstream.datasourceid are required"
        )
    source = resolve_source(item, ds)
    return kafka_options(source["bootstrapServers"], source["topic"], options)


def native_subscribe(options) -> tuple[str, list[str]]:
    bootstrap = _opt(options, ("kafka.bootstrap.servers",))
    subscribe = _opt(options, ("subscribe",))
    topics = [t.strip() for t in subscribe.split(",") if t.strip()] if subscribe else []
    return bootstrap, topics


def should_consume_native(fmt, options) -> bool:
    """True when this is OSS format('kafka') carrying bootstrap/subscribe/assign."""
    if (fmt or "").strip().lower() != "kafka":
        return False
    if should_rewrite(fmt, options):
        return False
    lower = {str(k).lower() for k in (options or {})}
    return any(key in lower for key in NATIVE_OPTION_KEYS)


def consume_plain_kafka(options, poll=None):
    """Pull Kafka records for OSS bootstrap + subscribe. Never rate rows."""
    options = {str(k): v for k, v in (options or {}).items()}
    if _opt(options, ("subscribePattern", "subscribepattern")):
        raise KafkaSourceError(
            "subscribePattern is not in this wrap (use subscribe)"
        )
    if _opt(options, ("assign",)):
        raise KafkaSourceError("assign is not in this wrap (use subscribe)")
    proto = _opt(options, ("kafka.security.protocol",))
    if proto and proto.upper() not in ("PLAINTEXT",):
        raise KafkaSourceError(
            f"kafka.security.protocol={proto!r} is not in this wrap (PLAINTEXT only)"
        )
    headers = _opt(options, ("includeHeaders", "includeheaders"))
    if headers and str(headers).strip().lower() in ("true", "1", "yes"):
        raise KafkaSourceError("includeHeaders is not in this wrap")
    bootstrap, topics = native_subscribe(options)
    if not bootstrap:
        raise KafkaSourceError("option 'kafka.bootstrap.servers' is required")
    if not topics:
        raise KafkaSourceError(
            "option 'subscribe' is required "
            "(subscribePattern / assign are not in this wrap)"
        )
    starting = _opt(options, ("startingOffsets", "startingoffsets")) or "earliest"
    if starting not in ("earliest", "latest"):
        raise KafkaSourceError(
            f"startingOffsets {starting!r} is not in this wrap (use earliest or latest)"
        )
    timeout = int(
        _opt(options, ("kafkaConsumer.pollTimeoutMs",
                       "kafkaconsumer.polltimeoutms")) or 8000
    )
    max_records = int(
        _opt(options, ("maxOffsetsPerTrigger", "maxoffsetspertrigger")) or 10000
    )
    return (poll or _kafka_poll)(bootstrap, topics, starting, timeout, max_records)


def _kafka_poll(bootstrap, topics, starting, timeout_ms, max_records):
    """kafka-python consumer → list of Kafka-shaped dicts (bytes intact)."""
    try:
        from kafka import KafkaConsumer, TopicPartition
    except ImportError as exc:
        raise KafkaSourceError(
            "OSS format('kafka') on Sail needs kafka-python in the spark-agent image"
        ) from exc
    servers = [s.strip() for s in bootstrap.split(",") if s.strip()]
    consumer = KafkaConsumer(
        bootstrap_servers=servers,
        enable_auto_commit=False,
        consumer_timeout_ms=timeout_ms,
        request_timeout_ms=max(int(timeout_ms) + 5000, 15000),
        api_version_auto_timeout_ms=min(int(timeout_ms), 10000),
    )
    try:
        tps = []
        deadline = time.monotonic() + max(int(timeout_ms), 1) / 1000.0
        missing = list(topics)
        while missing and time.monotonic() < deadline:
            still = []
            for topic in missing:
                parts = consumer.partitions_for_topic(topic)
                if not parts:
                    still.append(topic)
                    continue
                tps.extend(TopicPartition(topic, p) for p in sorted(parts))
            missing = still
            if missing:
                time.sleep(0.2)
        if missing:
            raise KafkaSourceError(
                f"kafka topic metadata not found for {missing} at {bootstrap}"
            )
        if not tps:
            return []
        consumer.assign(tps)
        if starting == "latest":
            consumer.seek_to_end(*tps)
            return []
        consumer.seek_to_beginning(*tps)
        rows = []
        for msg in consumer:
            rows.append({
                "key": msg.key,
                "value": msg.value,
                "topic": msg.topic,
                "partition": msg.partition,
                "offset": msg.offset,
                "timestamp": msg.timestamp,
                "timestampType": int(getattr(msg, "timestamp_type", 0) or 0),
            })
            if len(rows) >= int(max_records):
                break
        return rows
    finally:
        consumer.close()


def connect_kafka_df(spark, fmt, options):
    """Materialise a Kafka-schema LocalRelation, or None to leave the reader."""
    options = {str(k): v for k, v in (options or {}).items()}
    if should_rewrite(fmt, options):
        item, ds = eventstream_ids(options)
        if not item or not ds:
            raise EventstreamError(
                "both eventstream.itemid and eventstream.datasourceid are required"
            )
        recs = consume_events(item, ds)
        print(ANNOUNCE, file=sys.stderr, flush=True)
        return materialize_kafka_df(spark, recs)
    if should_consume_native(fmt, options):
        recs = consume_plain_kafka(options)
        print(NATIVE_ANNOUNCE, file=sys.stderr, flush=True)
        return materialize_kafka_df(spark, recs)
    return None


def _as_bytes(value):
    if value is None:
        return None
    if isinstance(value, (bytes, bytearray)):
        return bytes(value)
    if isinstance(value, str):
        if value == "":
            return b""
        try:
            return base64.b64decode(value)
        except (ValueError, TypeError):
            return value.encode("utf-8")
    return bytes(value)


def _as_timestamp(value):
    if value is None or value == "":
        return None
    if isinstance(value, datetime):
        return value
    if isinstance(value, (int, float)):
        # kafka-python timestamps are milliseconds since epoch.
        millis = float(value)
        if millis > 1e12:
            millis = millis / 1000.0
        return datetime.fromtimestamp(millis, tz=UTC).replace(tzinfo=None)
    text = str(value).replace("Z", "+00:00")
    try:
        ts = datetime.fromisoformat(text)
    except ValueError:
        return datetime.fromtimestamp(0, tz=UTC).replace(tzinfo=None)
    if ts.tzinfo is not None:
        ts = ts.astimezone(UTC).replace(tzinfo=None)
    return ts


def records_to_rows(records) -> list[tuple]:
    """Tuples matching Spark's kafka source schema (never rate)."""
    rows = []
    for rec in records or []:
        rec = rec or {}
        rows.append((
            _as_bytes(rec.get("key")),
            _as_bytes(rec.get("value")),
            rec.get("topic") or "",
            int(rec.get("partition") or 0),
            int(rec.get("offset") or 0),
            _as_timestamp(rec.get("timestamp")),
            int(rec.get("timestampType") or 0),
        ))
    return rows


def kafka_schema():
    from pyspark.sql.types import (
        BinaryType,
        IntegerType,
        LongType,
        StringType,
        StructField,
        StructType,
        TimestampType,
    )
    return StructType([
        StructField("key", BinaryType(), True),
        StructField("value", BinaryType(), True),
        StructField("topic", StringType(), False),
        StructField("partition", IntegerType(), False),
        StructField("offset", LongType(), False),
        StructField("timestamp", TimestampType(), True),
        StructField("timestampType", IntegerType(), False),
    ])


def materialize_kafka_df(spark, records):
    """LocalRelation with Kafka columns. `.explain()` is not a streaming plan."""
    df = spark.createDataFrame(records_to_rows(records), kafka_schema())
    df._emu_eventstream = True
    return df


def should_run_local_foreach(stream_df, writer) -> bool:
    """True only for an Eventstream wrap + foreachBatch. Other streams stay on the engine."""
    if writer is None or getattr(writer, "_emu_foreach_batch", None) is None:
        return False
    return bool(getattr(stream_df, "_emu_eventstream", False))


class OneShotStreamingQuery:
    """Stand-in after a local foreachBatch. availableNow: already finished."""

    def __init__(self, name=None):
        self._active = False
        self.name = name or None
        self.id = "emu-eventstream"
        self.runId = "emu-eventstream-run"
        self.lastProgress = None
        self.recentProgress = []

    @property
    def isActive(self):
        return self._active

    def stop(self):
        self._active = False

    def awaitTermination(self, timeout=None):
        return True

    def processAllAvailable(self):
        return None

    def explain(self, extended=False):
        print(FOREACH_ANNOUNCE, flush=True)


def _install_classic():
    """Wrap classic DataStreamReader so Eventstream options never reach OSS Kafka."""
    global _classic_installed
    if os.environ.get("SPARK_REMOTE"):
        return False
    if _classic_installed:
        return True
    from pyspark.sql.streaming import DataStreamReader

    orig_format = DataStreamReader.format
    orig_option = DataStreamReader.option
    orig_load = DataStreamReader.load

    def format(self, source):  # noqa: A001 — Spark's name
        self._emu_format = source
        return orig_format(self, source)

    def option(self, key, value):
        held = getattr(self, "_emu_opts", None)
        if held is None:
            held = {}
            self._emu_opts = held
        held[str(key)] = value
        if str(key).lower().startswith("eventstream."):
            return self
        return orig_option(self, key, value)

    def options(self, **kwargs):
        for key, value in kwargs.items():
            option(self, key, value)
        return self

    def load(self, path=None, format=None, schema=None, **kwargs):  # noqa: A002
        held = dict(getattr(self, "_emu_opts", {}) or {})
        held.update({str(k): v for k, v in kwargs.items()})
        fmt = format or getattr(self, "_emu_format", None)
        rewritten = rewrite_load(fmt, held)
        if rewritten is not None:
            kwargs = {k: v for k, v in kwargs.items()
                      if not str(k).lower().startswith("eventstream.")}
            for key, value in rewritten.items():
                orig_option(self, key, value)
        return orig_load(self, path=path, format=format, schema=schema, **kwargs)

    DataStreamReader.format = format
    DataStreamReader.option = option
    DataStreamReader.options = options
    DataStreamReader.load = load
    _classic_installed = True
    return True


def _install_connect(spark):
    """Wrap Connect read / readStream / foreachBatch for Kafka on Sail."""
    global _connect_installed
    if _connect_installed:
        return True
    try:
        from pyspark.sql.connect.dataframe import DataFrame as ConnectDF
        from pyspark.sql.connect.readwriter import DataFrameReader
        from pyspark.sql.connect.streaming.readwriter import (
            DataStreamReader,
            DataStreamWriter,
        )
    except ImportError:
        return False
    if spark is not None:
        reader = getattr(spark, "read", None)
        if reader is None or not isinstance(reader, DataFrameReader):
            return False

    orig_load = DataStreamReader.load
    orig_batch_load = DataFrameReader.load
    orig_foreach = DataStreamWriter.foreachBatch
    orig_start = DataStreamWriter.start
    orig_ws = ConnectDF.writeStream

    def _held(reader, format, kwargs):  # noqa: A002
        if format is not None:
            reader.format(format)
        held = dict(getattr(reader, "_options", {}) or {})
        held.update({str(k): v for k, v in kwargs.items()})
        fmt = format or getattr(reader, "_format", None)
        return fmt, held

    def load(self, path=None, format=None, schema=None, **kwargs):  # noqa: A002
        fmt, held = _held(self, format, kwargs)
        df = connect_kafka_df(spark, fmt, held)
        if df is not None:
            return df
        return orig_load(self, path=path, format=format, schema=schema, **kwargs)

    def batch_load(self, path=None, format=None, schema=None, **kwargs):  # noqa: A002
        fmt, held = _held(self, format, kwargs)
        df = connect_kafka_df(spark, fmt, held)
        if df is not None:
            return df
        return orig_batch_load(self, path=path, format=format, schema=schema, **kwargs)

    def foreachBatch(self, func):  # noqa: N802 — Spark's name
        self._emu_foreach_batch = func
        stream_df = getattr(self, "_emu_stream_df", None)
        if getattr(stream_df, "_emu_eventstream", False):
            return self
        return orig_foreach(self, func)

    def writeStream(self):  # noqa: N802
        writer = orig_ws.fget(self) if isinstance(orig_ws, property) else orig_ws(self)
        writer._emu_stream_df = self
        return writer

    def start(self, path=None, format=None, outputMode=None, partitionBy=None,
              queryName=None, **options):
        stream_df = getattr(self, "_emu_stream_df", None)
        if should_run_local_foreach(stream_df, self):
            print(FOREACH_ANNOUNCE, file=sys.stderr, flush=True)
            self._emu_foreach_batch(stream_df, 0)
            return OneShotStreamingQuery(name=queryName)
        return orig_start(
            self, path=path, format=format, outputMode=outputMode,
            partitionBy=partitionBy, queryName=queryName, **options,
        )

    DataStreamReader.load = load
    DataFrameReader.load = batch_load
    DataStreamWriter.foreachBatch = foreachBatch
    DataStreamWriter.start = start
    ConnectDF.writeStream = property(writeStream)
    DataStreamReader._emu_eventstream_patched = True
    _connect_installed = True
    return True


def install(spark=None):  # noqa: ARG001 — session used to detect Connect
    """Install the Eventstream wrap for whichever engine this session is."""
    try:
        classic = _install_classic()
    except ImportError:
        classic = False
    try:
        connect = _install_connect(spark)
    except ImportError:
        connect = False
    return classic or connect
