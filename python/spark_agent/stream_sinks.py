"""Durable streaming sinks via Sail collect + a batch write.

Sail's `writeStream.format("delta"|"parquet"|"memory").start()` fails inside
the engine (`DeltaWriteNode`, listing-table, missing memory format).
`foreachBatch` is not a workaround: Sail rejects `start()` with
`missing argument: Python UDF output type`, and that pickle would run on the
server anyway, not in this process.

What *does* work, measured 2026-08-14: a streaming `rate` plan executed as a
query (`limit(n).collect()`, `take`, `toLocalIterator`) returns real rows to
the Connect client. This module wraps `DataStreamWriter.start()` for the
named durable formats, pulls one bounded batch that way, and lands it with
the batch writer Sail already has (or a temp view, for `memory`).

The notebook API is preserved. The query object is a stand-in: `isActive`
stays true until `stop()`, there is no checkpoint, and a continuous query
lands one micro-batch rather than running until stopped. Announced on
stderr. `console`, `kafka`, Eventstream options, `foreachBatch`, and
`outputMode("complete")` fall through.

Gated by `install()` on a Connect session, same as CDF / JSON `multiLine`.
"""
from __future__ import annotations

import sys

PULL_LIMIT = 10
FLOW_COLUMNS = ("_marker", "_retracted")
DURABLE_FORMATS = ("delta", "parquet")
ANNOUNCE = "[delta_ops] streaming sink via Sail collect + batch write"


class StreamSinkError(RuntimeError):
    """The wrap recognised the sink but could not honour it faithfully."""


def _lower(value) -> str:
    return "" if value is None else str(value).strip().lower()


def _as_dict(options) -> dict:
    if not options:
        return {}
    if isinstance(options, dict):
        return {str(k): v for k, v in options.items()}
    try:
        return {str(k): options[k] for k in options}
    except Exception:
        return {str(k): v for k, v in dict(options).items()}


def _has_foreach(proto) -> bool:
    if proto is None:
        return False
    if hasattr(proto, "HasField"):
        try:
            return bool(
                proto.HasField("foreach_batch") or proto.HasField("foreach_writer")
            )
        except (ValueError, AttributeError):
            pass
    for name in ("foreach_batch", "foreach_writer"):
        field = getattr(proto, name, None)
        if getattr(field, "python_function", None):
            return True
    return False


def snapshot(writer, path=None, format=None, outputMode=None, partitionBy=None,
             queryName=None, options=None) -> dict:
    """Normalize a Connect writer (or a test double) into a sink request."""
    proto = getattr(writer, "_write_proto", None)
    proto_opts = _as_dict(getattr(proto, "options", None) if proto is not None else None)
    start_opts = _as_dict(options)
    merged = {**proto_opts, **{k: v for k, v in start_opts.items()}}
    lower = {str(k).lower(): v for k, v in merged.items()}
    fmt = format or (getattr(proto, "format", None) if proto is not None else None)
    dest = path or lower.get("path") or (getattr(proto, "path", None) if proto is not None else None)
    name = (
        queryName
        or (getattr(proto, "query_name", None) if proto is not None else None)
        or lower.get("queryname")
    )
    mode = (
        outputMode
        or (getattr(proto, "output_mode", None) if proto is not None else None)
        or lower.get("outputmode")
        or "append"
    )
    parts = partitionBy
    if parts is None and proto is not None:
        parts = getattr(proto, "partitioning_column_names", None)
    if isinstance(parts, str):
        parts = [parts]
    return {
        "format": _lower(fmt),
        "path": dest or "",
        "query_name": (name or "").strip(),
        "output_mode": _lower(mode) or "append",
        "partition_by": [str(c) for c in (parts or [])],
        "options": merged,
        "foreach": _has_foreach(proto),
    }


def should_intercept(state: dict) -> bool:
    """True only for append delta/parquet (with a path) or memory (with a name).

    Kafka, Eventstream options, foreachBatch/foreach, console, and complete
    output mode stay on the engine — wrapping those would guess.
    """
    if not state or state.get("foreach"):
        return False
    opts = {str(k).lower() for k in (state.get("options") or {})}
    if any(k.startswith("eventstream.") for k in opts):
        return False
    mode = _lower(state.get("output_mode")) or "append"
    if mode != "append":
        return False
    fmt = _lower(state.get("format"))
    if fmt in DURABLE_FORMATS:
        return bool(state.get("path"))
    if fmt == "memory":
        return bool(state.get("query_name"))
    return False


def row_as_dict(row) -> dict:
    if hasattr(row, "asDict"):
        data = row.asDict(recursive=True)
    elif isinstance(row, dict):
        data = dict(row)
    else:
        data = dict(row)
    for col in FLOW_COLUMNS:
        data.pop(col, None)
    return data


def pull_rows(stream_df, n=PULL_LIMIT) -> list:
    """Execute the streaming plan as a bounded query; return dicts without flow cols."""
    if stream_df is None:
        raise StreamSinkError("no streaming DataFrame to collect")
    rows = stream_df.limit(n).collect()
    return [row_as_dict(row) for row in rows]


def land_batch(spark, rows, state):
    """Write pulled rows with the batch API. No-op on an empty pull."""
    if not rows:
        return
    frame = spark.createDataFrame(rows)
    fmt = state["format"]
    if fmt == "memory":
        frame.createOrReplaceTempView(state["query_name"])
        return
    writer = frame.write.format(fmt).mode("append")
    parts = state.get("partition_by") or []
    if parts:
        writer = writer.partitionBy(*parts)
    writer.save(state["path"])


class PulledStreamingQuery:
    """Stand-in for Spark's StreamingQuery after a one-shot collect + write."""

    def __init__(self, name=None):
        self._active = True
        self.name = name or None
        self.id = "emu-pulled-stream"
        self.runId = "emu-pulled-run"
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
        print(f"{ANNOUNCE} (LocalRelation / one micro-batch)", flush=True)


def run_pulled_sink(stream_df, spark, state, file=sys.stderr) -> PulledStreamingQuery:
    print(f"{ANNOUNCE} ({state['format']}; one micro-batch, no checkpoint)",
          file=file, flush=True)
    rows = pull_rows(stream_df)
    land_batch(spark, rows, state)
    return PulledStreamingQuery(name=state.get("query_name") or None)


def _orig_write_stream(orig, df):
    if isinstance(orig, property):
        return orig.fget(df)
    return orig(df)


def patch_write_stream(writer_cls, df_cls, spark):
    """Wrap Connect `DataFrame.writeStream` / `DataStreamWriter.start`."""
    if getattr(writer_cls, "_emu_stream_sink_patched", False):
        return
    orig_start = writer_cls.start
    orig_ws = df_cls.writeStream

    def writeStream(self):
        writer = _orig_write_stream(orig_ws, self)
        writer._emu_stream_df = self
        return writer

    def start(self, path=None, format=None, outputMode=None, partitionBy=None,
              queryName=None, **options):
        state = snapshot(
            self, path=path, format=format, outputMode=outputMode,
            partitionBy=partitionBy, queryName=queryName, options=options,
        )
        if not should_intercept(state):
            return orig_start(
                self, path=path, format=format, outputMode=outputMode,
                partitionBy=partitionBy, queryName=queryName, **options,
            )
        stream_df = getattr(self, "_emu_stream_df", None)
        session = getattr(self, "_session", None) or spark
        return run_pulled_sink(stream_df, session, state)

    df_cls.writeStream = property(writeStream)
    writer_cls.start = start
    writer_cls._emu_stream_sink_patched = True


def install(spark) -> bool:
    """Wrap Connect `writeStream.start` for durable sinks. No-op otherwise."""
    try:
        from pyspark.sql.connect.dataframe import DataFrame as ConnectDF
        from pyspark.sql.connect.readwriter import DataFrameReader
        from pyspark.sql.connect.streaming.readwriter import DataStreamWriter
    except ImportError:
        return False
    reader = getattr(spark, "read", None)
    if reader is None or not isinstance(reader, DataFrameReader):
        return False
    patch_write_stream(DataStreamWriter, ConnectDF, spark)
    return True
