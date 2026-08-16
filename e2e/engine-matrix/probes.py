"""Engine capability probes, run identically against Sail and Spark JVM.

Why this exists: the parity map used to assert what Sail could not do, with no
symmetric test. That went stale — a row read "execution fails on Sail v0.6.6"
for structured streaming long after Sail gained streaming support, and it took
a hand-probe to notice. Worse, three unrelated capabilities were bundled under
one verdict, hiding that one of them partly worked.

So: one probe per capability, never bundled, run against both engines, and the
error *message* recorded rather than just pass/fail — `No table format found
for: memory` and `unsupported extension node for streaming: DeltaWriteNode`
are different facts and point at different fixes.

Each probe is a callable taking a SparkSession. Returning normally is a pass;
any exception is a fail, with its type and message recorded.
"""
import json
import os
import sys
import time
import traceback

WAREHOUSE = os.environ.get("PROBE_DIR", "/tmp/probe")


def _table_path(name):
    return f"{WAREHOUSE}/{name}"


# --------------------------------------------------------------- core Delta
def delta_write(spark):
    spark.sql("SELECT 1 AS id, 'a' AS name").write.format("delta").mode("overwrite").save(
        _table_path("t_write"))


def delta_append(spark):
    p = _table_path("t_append")
    spark.sql("SELECT 1 AS id").write.format("delta").mode("overwrite").save(p)
    spark.sql("SELECT 2 AS id").write.format("delta").mode("append").save(p)
    assert spark.read.format("delta").load(p).count() == 2


def delta_time_travel_dataframe(spark):
    p = _table_path("t_tt")
    spark.sql("SELECT 1 AS id").write.format("delta").mode("overwrite").save(p)
    spark.sql("SELECT 2 AS id").write.format("delta").mode("append").save(p)
    n = spark.read.format("delta").option("versionAsOf", 0).load(p).count()
    assert n == 1, n


def delta_time_travel_sql(spark):
    """The SQL syntax form. Sail supports the DataFrame option but this is a
    separate code path in its planner."""
    p = _table_path("t_tt_sql")
    spark.sql("SELECT 1 AS id").write.format("delta").mode("overwrite").save(p)
    spark.sql("SELECT 2 AS id").write.format("delta").mode("append").save(p)
    spark.sql(f"SELECT count(*) FROM delta.`{p}` VERSION AS OF 0").collect()


def delta_merge_registered_table(spark):
    """MERGE against a registered table whose LOCATION is a **local path**.

    Deliberately qualified: e2e/sail proves the same statement succeeds on Sail
    when the table is backed by an `az://` OneLake URL, which is the path the
    emulator actually uses. Isolated by elimination — CREATE TABLE without
    IF NOT EXISTS still fails, and so does an update-only MERGE — leaving the
    storage URL as the only difference. Reporting this as "MERGE unsupported"
    would understate the engine.
    """
    p = _table_path("t_merge_reg")
    spark.sql("SELECT 1 AS id, 'a' AS v").write.format("delta").mode("overwrite").save(p)
    spark.sql(f"CREATE TABLE IF NOT EXISTS m_reg USING delta LOCATION '{p}'")
    # The VALUES form, which e2e/sail proves works: an inline projection as the
    # MERGE source trips a resolution quirk unrelated to MERGE support itself.
    spark.sql("""
        MERGE INTO m_reg AS t
        USING (SELECT * FROM VALUES (1, 'b') AS s(id, v)) AS s
        ON t.id = s.id
        WHEN MATCHED THEN UPDATE SET t.v = s.v
        WHEN NOT MATCHED THEN INSERT *
    """)


def delta_merge_path_target(spark):
    """A path-addressed merge target — distinct from the registered-table form.

    Update-only: the VALUES row matches the existing id. The interception still
    has to parse a subquery source against a delta.`path` target; INSERT * is
    covered by the registered-table probe.
    """
    p = _table_path("t_merge_path")
    spark.sql("SELECT 1 AS id, 'a' AS v").write.format("delta").mode("overwrite").save(p)
    spark.sql(f"""
        MERGE INTO delta.`{p}` AS t
        USING (SELECT * FROM VALUES (1, 'b') AS s(id, v)) AS s
        ON t.id = s.id
        WHEN MATCHED THEN UPDATE SET t.v = s.v
    """)


def delta_optimize(spark):
    p = _table_path("t_opt")
    spark.sql("SELECT 1 AS id").write.format("delta").mode("overwrite").save(p)
    spark.sql(f"OPTIMIZE delta.`{p}`")


def delta_vacuum(spark):
    p = _table_path("t_vac")
    spark.sql("SELECT 1 AS id").write.format("delta").mode("overwrite").save(p)
    spark.sql(f"VACUUM delta.`{p}` RETAIN 168 HOURS")


def delta_change_data_feed(spark):
    p = _table_path("t_cdf")
    (spark.sql("SELECT 1 AS id").write.format("delta")
     .option("delta.enableChangeDataFeed", "true").mode("overwrite").save(p))
    spark.sql("SELECT 2 AS id").write.format("delta").mode("append").save(p)
    rows = (spark.read.format("delta").option("readChangeFeed", "true")
            .option("startingVersion", 0).load(p).collect())
    # Accepted-but-inert is a *different* outcome from rejected: a normal
    # snapshot has no _change_type column.
    assert any("_change_type" in r.asDict() for r in rows), "CDF returned a plain snapshot"


# ----------------------------------------------------------------- streaming
#
# These probes assert that rows REACH THE SINK, not merely that a query object
# reported itself active. The weaker form is what they used to do, and it made
# `console` green on an engine that never delivered a single batch — a ✅ that
# measured liveness while its label claimed a working sink.
#
# Where the sink's output is not observable from a Spark Connect client, the
# probe says so in its name rather than pretending. Two are in that category:
# `console` writes to the *server's* stdout, and the rate source can only be
# observed through some sink. Sail also reports no progress metrics
# (`lastProgress` is None, `recentProgress` is []), so there is nothing else to
# interrogate — asserting on them would fail for a missing API rather than a
# missing capability.
def _rate_stream(spark):
    return spark.readStream.format("rate").option("rowsPerSecond", 5).load()


def _await_rows(read, deadline=25):
    """Poll a reader until rows appear, returning (row_count, columns).

    Streaming sinks land data asynchronously, so a single read straight after
    `start()` proves nothing either way. Returns (0, set()) if nothing arrives
    before the deadline — the caller decides whether that is a failure.
    """
    end = time.time() + deadline
    while time.time() < end:
        try:
            df = read()
            n = df.count()
            if n > 0:
                return n, set(df.columns)
        except Exception:  # noqa: BLE001 — the target may not exist yet
            pass
        time.sleep(1)
    return 0, set()


def _assert_sink_wrote(spark, fmt, path, read):
    """Run a streaming query into `path` and assert real rows land there."""
    query = (_rate_stream(spark).writeStream.format(fmt)
             .option("path", path)
             .option("checkpointLocation", path + "_ck")
             .outputMode("append").start())
    try:
        rows, cols = _await_rows(read)
    finally:
        query.stop()
    assert rows > 0, (
        f"{fmt} sink: query ran but no rows were readable at {path} — "
        "a started query is not a working sink"
    )
    # The flow event schema (`_marker`/`_retracted`) is engine-internal. If it
    # reaches the files, the sink wrote its plumbing instead of the user's data.
    leaked = {"_marker", "_retracted"} & cols
    assert not leaked, f"{fmt} sink: flow-event columns leaked into the output: {sorted(leaked)}"


def streaming_read_rate(spark):
    """Schema only — a readStream is not observable without a sink."""
    df = _rate_stream(spark)
    assert "timestamp" in df.schema.simpleString()


def streaming_sink_console(spark):
    """Liveness only — console writes to the server's stdout, not to the client.

    Deliberately still the weak assertion, because no stronger one exists from
    here. The row is named so that the matrix does not overstate it.
    """
    query = _rate_stream(spark).writeStream.format("console").outputMode("append").start()
    time.sleep(4)
    active = query.isActive
    query.stop()
    assert active, "console sink started but was not active"


def _seed_kafka(spark, bootstrap, topic):
    """Put one Kafka-schema record on `topic` so the read probe is not vacuous.

    sail-delta has kafka-python (the wrap's client). JVM has spark-sql-kafka
    and writes with the jar. Bare Sail never sets KAFKA_BOOTSTRAP, so this
    is not called there — the probe fails on the missing source, not on a
    missing broker.
    """
    try:
        from kafka import KafkaProducer
    except ImportError:
        (spark.createDataFrame([("hello-engine-matrix",)], ["v"])
         .selectExpr("CAST(v AS BINARY) as value")
         .write.format("kafka")
         .option("kafka.bootstrap.servers", bootstrap)
         .option("topic", topic)
         .save())
        return
    producer = KafkaProducer(
        bootstrap_servers=[s.strip() for s in bootstrap.split(",") if s.strip()],
        acks="all",
    )
    try:
        producer.send(topic, b"hello-engine-matrix").get(timeout=15)
        producer.flush()
    finally:
        producer.close()


def streaming_read_kafka(spark):
    """OSS format('kafka') + bootstrap/subscribe. Kafka schema, never rate.

    Batch read so JVM native spark-sql-kafka and the Sail wrap can both
    collect(). CAST(value AS STRING) runs on the engine — that is the bytes
    reaching Sail, not a mapping onto `rate`.
    """
    bootstrap = os.environ.get("KAFKA_BOOTSTRAP", "")
    topic = os.environ.get("KAFKA_TOPIC", "engine-matrix-kafka")
    if bootstrap:
        _seed_kafka(spark, bootstrap, topic)
    df = (spark.read.format("kafka")
          .option("kafka.bootstrap.servers", bootstrap or "kafka:9092")
          .option("subscribe", topic)
          .option("startingOffsets", "earliest")
          .option("endingOffsets", "latest")
          .load())
    names = {f.name for f in df.schema.fields}
    assert {"key", "value", "topic", "partition", "offset"} <= names, names
    assert "rate" not in str(df.schema).lower()
    rows = df.selectExpr("CAST(value AS STRING) as v").collect()
    assert len(rows) > 0, "kafka source returned no rows"
    assert any("hello-engine-matrix" in (r.v or "") for r in rows), [r.v for r in rows]


def streaming_sink_memory(spark):
    """The memory sink IS observable: it registers a queryable table."""
    name = "probe_mem"
    query = (_rate_stream(spark).writeStream.format("memory")
             .queryName(name).outputMode("append").start())
    try:
        rows, _ = _await_rows(lambda: spark.table(name))
    finally:
        query.stop()
    assert rows > 0, "memory sink: query ran but spark.table() returned no rows"


def streaming_sink_parquet(spark):
    path = _table_path("s_pq")
    _assert_sink_wrote(spark, "parquet", path, lambda: spark.read.parquet(path))


def streaming_sink_delta(spark):
    path = _table_path("s_delta")
    _assert_sink_wrote(spark, "delta", path, lambda: spark.read.format("delta").load(path))


# ------------------------------------------------------- JVM-only surface
def rdd_sparkcontext(spark):
    assert spark.sparkContext.parallelize([1, 2, 3, 4]).map(lambda x: x * 2).sum() == 20


def jvm_bridge(spark):
    assert spark._jvm.java.lang.String is not None


# ----------------------------------------------------------- general Spark
def create_dataframe_local_rows(spark):
    df = spark.createDataFrame([(1, "a"), (2, "b")], ["id", "name"])
    assert df.count() == 2


def python_udf(spark):
    """Python UDF execution — Sail embeds CPython through pyo3 to run these.

    This failed for a while with `read_udfs() missing 2 required positional
    arguments`, which looked like an engine gap but was a client pin: pyspark
    4.2.0 added two parameters to `pyspark.worker.read_udfs`, and pysail 0.6.x
    called the 3-argument form. The fix was not a particular version — it was
    pinning the client to whatever the pysail release is built against, which
    `pyproject.toml` now does for both halves at once (Sail 0.7.0 + client
    4.2.0).

    The direction of the constraint was measured rather than assumed, and it is
    asymmetric: an OLD server cannot take a NEW client, but 0.7.0 takes 4.1.1
    fine. So a regression here means the client LEADS the server, not merely
    that the two differ. Check that pin before suspecting the engine.
    """
    from pyspark.sql.functions import udf
    from pyspark.sql.types import IntegerType
    double = udf(lambda x: x * 2, IntegerType())
    rows = spark.sql("SELECT 21 AS n").select(double("n").alias("d")).collect()
    assert rows[0]["d"] == 42


def sql_temp_view(spark):
    spark.sql("SELECT 1 AS id").createOrReplaceTempView("v_probe")
    assert spark.sql("SELECT count(*) AS n FROM v_probe").collect()[0]["n"] == 1


def sql_create_table_defaults_to_delta(spark):
    """A CREATE TABLE with no USING clause must produce a DELTA table.

    On Fabric the default table format is Delta, so `CREATE TABLE x LOCATION
    '...' AS SELECT ...` writes a Delta table with no `USING delta` needed.
    Sail's default is not Delta, so the same statement writes something else at
    that path and nothing downstream can read it as Delta.

    This is invisible until something depends on it, and something did.
    dbt-fabricspark's file_format_clause macro emits NO clause for exactly one
    value of file_format — `delta` — because the adapter assumes it is the
    default. So a model configured `+file_format: delta` and `+location_root:
    abfs://.../Tables` produced

        create or replace table `lake`.silver_orders
          location 'abfs://.../Tables/silver_orders' as ...

    with no USING, and the lakehouse silently never received silver while dbt
    reported success. Two rounds of debugging went into config that was being
    applied correctly the whole time.

    Neither side is wrong in isolation, which is what makes it worth a row:
    dbt is correct for Fabric, and an engine is entitled to its own default.

    Read the row carefully — it is red on BOTH ENGINES and green in the middle.
    Delta-by-default is a FABRIC property and neither Sail nor the JVM overlay
    reproduces it; the EMULATOR now does, because `delta_ops` honours an explicit
    LOCATION and writes Delta there. That makes this the one row where the
    emulator is more faithful to Fabric than the JVM overlay is.

    An earlier revision of this docstring said the row was "expected to be RED
    EVERYWHERE, and that is the finding", and concluded the examples'
    fabricspark__file_format_clause override was "the price of running
    dbt-fabricspark anywhere that is not Fabric". The first half is no longer
    true. The second is still worth testing rather than assuming: the override
    may now be unnecessary AGAINST THE EMULATOR, and it is certainly still needed
    against bare OSS Spark. Prove it with the medallion e2e before deleting it.
    """
    p = _table_path("t_default_fmt")
    # Plain CREATE TABLE, not CREATE OR REPLACE: the first version of this probe
    # used OR REPLACE and the JVM leg failed with "does not support REPLACE TABLE
    # AS SELECT", which is a statement-support fact and not the format question.
    # A probe that can fail for two reasons measures neither.
    spark.sql(f"CREATE TABLE d_default LOCATION '{p}' AS SELECT 1 AS id")
    # The portable test from a Connect client: can the Delta reader open it?
    # A non-Delta table at that path has no _delta_log and this fails.
    assert spark.read.format("delta").load(p).count() == 1


def sql_describe_registered_delta_table(spark):
    """DESCRIBE TABLE must return one row per column for a registered Delta table.

    Sail returns the right SCHEMA (col_name, data_type, comment) and ZERO ROWS,
    and raises nothing. Against a TEMP VIEW it answers correctly, so the gap is
    specific to catalog-registered Delta tables — which is exactly what the
    emulator's registerLakehouseTables creates for every lakehouse table.

    This cost a day precisely because three components each behaved reasonably:
    Sail answers with no rows and no error; the Livy agent forwards the empty
    result faithfully (the schema has 3 fields, so it is not an empty envelope);
    dbt's get_columns_in_relation reads that as "this table has no columns"; and
    the model then compiles to `select` followed by `from`. Nobody lied. The
    information that the answer was missing simply did not survive the chain.

    So the row belongs here rather than in a comment: it is the only place the
    claim gets re-checked against a live engine. And it is broader than dbt —
    ANY introspection against a registered table has this hole, so a green row
    one day is what tells us the workarounds can be removed.

    The portable route, which is what the medallion models use instead:
    `run_query("select * from t limit 0").column_names`, which carries the
    schema in the result envelope and never asks the catalog.
    """
    p = _table_path("t_describe")
    spark.sql("SELECT 1 AS id, 'a' AS v").write.format("delta").mode("overwrite").save(p)
    spark.sql(f"CREATE TABLE IF NOT EXISTS d_reg USING delta LOCATION '{p}'")
    rows = spark.sql("DESCRIBE TABLE d_reg").collect()
    names = [r[0] for r in rows]
    assert "id" in names and "v" in names, f"DESCRIBE returned {len(rows)} rows: {names}"


def sql_describe_detail_registered_delta(spark):
    """DESCRIBE DETAIL resolves a registered Delta table to its physical location.

    python/spark_agent/delta_ops.py needs exactly this: when a statement names a
    registered table rather than a path, the delta-rs interception has to find
    where the table actually lives before it can act on it.

    Recorded separately from the DESCRIBE TABLE row ᵉ because the two fail in
    opposite ways on the bare engine, and the difference is the whole lesson:
    one is silent and cost a day, the other is noisy and cost minutes. The
    emulator answers both from the Delta log once LOCATION is recorded.
    """
    p = _table_path("t_detail")
    spark.sql("SELECT 1 AS id").write.format("delta").mode("overwrite").save(p)
    spark.sql(f"CREATE TABLE IF NOT EXISTS dd_reg USING delta LOCATION '{p}'")
    rows = spark.sql("DESCRIBE DETAIL dd_reg").collect()
    assert rows and rows[0]["location"], f"DESCRIBE DETAIL returned {len(rows)} rows"


def sql_filter_on_unprojected_window_column(spark):
    """Filter on a window-function column the outer SELECT does not project.

    This is the `row_number()` deduplication shape: rank rows into `_rn` in a
    CTE, keep `_rn = 1`, project only the real columns. Standard SQL evaluates
    WHERE against the FROM relation, so `_rn` is in scope even though it is not
    projected. It cost the dbt-fabricspark medallion models a rewrite after:

        attribute ObjectName([Identifier("_rn")]) is missing from the schema:
        cannot resolve attribute

    The window function is load-bearing in this probe. The same query with a
    plain unprojected column passes on all three engines — that was the first
    version of this probe, and it was green everywhere, which is why it is
    written this way instead. The gap is specifically a window-derived
    attribute surviving into a filter above the projection that drops it.
    """
    spark.sql(
        "SELECT 1 AS order_id, 1 AS event_seq, 5 AS qty "
        "UNION ALL SELECT 1, 2, 7 "
        "UNION ALL SELECT 2, 1, 9"
    ).createOrReplaceTempView("v_win")
    rows = spark.sql(
        "WITH latest AS ("
        "  SELECT *, row_number() OVER (PARTITION BY order_id ORDER BY event_seq DESC) AS _rn"
        "  FROM v_win"
        ") "
        "SELECT order_id, qty FROM latest WHERE _rn = 1 ORDER BY order_id"
    ).collect()
    assert [(r["order_id"], r["qty"]) for r in rows] == [(1, 7), (2, 9)], rows


# ---------------------------------------------------------- reader options
#
# Two options Sail ACCEPTS AND IGNORES. Accepted-but-inert is a different
# outcome from rejected — the same distinction `delta_change_data_feed` above
# is built around — and it is the more dangerous one: nothing in the API tells
# a caller the option did not take effect, so code written against Sail passes
# locally for the wrong reason while the same code on real Fabric Spark, which
# honours both, behaves differently.
#
# The fixture is the whole difficulty. A file with ONE line makes "honoured"
# and "ignored" produce an identical row count, and the first attempt at this
# measurement reported `wholetext` as working for exactly that reason. So every
# fixture here has more than one line, and each probe asserts the PLAIN read
# first — a broken fixture must report itself as a broken fixture rather than
# as a missing capability.
#
# The fixtures are written THROUGH THE ENGINE rather than with a client-side
# open(): on the `sail` profile the probe runs in a different container from
# the engine and shares no volume with it (only `sail-delta` does, and the
# compose file says why), so a client-written file would not be there to read
# and the probe would measure a missing file.


def _write_text_through_engine(spark, name, body):
    """Write `body` verbatim as a text file using the engine's own writer."""
    p = _table_path(name)
    (spark.createDataFrame([(body,)], "value string")
     .coalesce(1).write.mode("overwrite").text(p))
    return p


def read_text_wholetext(spark):
    """`wholetext=True` must return ONE row per FILE, not one per line."""
    p = _write_text_through_engine(spark, "t_wholetext", "line-1\nline-2\nline-3")
    plain = spark.read.text(p).count()
    assert plain == 3, (
        f"fixture is not 3 lines (a plain read gave {plain}), so the option "
        "cannot be measured — this is a fixture failure, not a capability one")
    # The KEYWORD form, not .option("wholetext", True). PySpark's
    # DataFrameReader.text(path, wholetext=False, ...) calls _set_opts with
    # that default, which OVERWRITES an option set beforehand — so the
    # .option() spelling can never take effect here and a probe written that
    # way reports every engine as ignoring the option, including Spark JVM,
    # which honours it. Measured: .option() -> 3 rows, keyword -> 1 row, on the
    # same JVM session and the same file.
    n = spark.read.text(p, wholetext=True).count()
    assert n == 1, (
        f"wholetext returned {n} rows over a {plain}-line file; honoured means "
        "one row per file, so the option was accepted and silently ignored")


def read_json_multiline(spark):
    """`multiLine=True` must read a file that is a single JSON array."""
    p = _write_text_through_engine(
        spark, "t_multiline", '[\n  {"id": 1},\n  {"id": 2}\n]')
    plain = spark.read.text(p).count()
    assert plain == 4, (
        f"fixture is not a 4-line JSON array (a plain read gave {plain}) — "
        "this is a fixture failure, not a capability one")
    # Keyword form for the same reason, though json()'s parameters default to
    # None rather than False, so .option() does survive there. Spelling both
    # the same way keeps the pair from depending on that difference.
    rows = spark.read.json(p, multiLine=True).collect()
    assert len(rows) == 2, (
        f"multiLine returned {len(rows)} rows; the array holds 2 objects")


PROBES = [
    ("delta.write", "Delta write", delta_write),
    ("delta.append", "Delta append", delta_append),
    ("delta.time_travel_dataframe", "Time travel — `option(\"versionAsOf\")`", delta_time_travel_dataframe),
    ("delta.time_travel_sql", "Time travel — SQL `VERSION AS OF`", delta_time_travel_sql),
    ("delta.merge_registered", "`MERGE INTO` a registered table at a **local path** ᵃ", delta_merge_registered_table),
    ("delta.merge_path", "`MERGE INTO delta.`path`` (path target)", delta_merge_path_target),
    ("delta.optimize", "`OPTIMIZE`", delta_optimize),
    ("delta.vacuum", "`VACUUM`", delta_vacuum),
    ("delta.cdf", "Change Data Feed (must not be inert) ᵇ", delta_change_data_feed),
    ("streaming.read_rate", "`readStream` (rate source) — schema only ᶜ", streaming_read_rate),
    ("streaming.read_kafka", "`format(\"kafka\")` + bootstrap/subscribe (rows on the engine) ʲ", streaming_read_kafka),
    ("streaming.sink_console", "Streaming sink — console — liveness only ᶜ", streaming_sink_console),
    ("streaming.sink_memory", "Streaming sink — memory (rows readable)", streaming_sink_memory),
    ("streaming.sink_parquet", "Streaming sink — parquet (rows readable)", streaming_sink_parquet),
    ("streaming.sink_delta", "Streaming sink — **delta** (rows readable)", streaming_sink_delta),
    ("jvm.rdd_sparkcontext", "`sc` / RDD API", rdd_sparkcontext),
    ("jvm.bridge", "`spark._jvm` bridge", jvm_bridge),
    ("spark.create_dataframe_local", "`createDataFrame(local_rows)`", create_dataframe_local_rows),
    ("spark.python_udf", "Python UDF", python_udf),
    ("spark.sql_temp_view", "SQL over a temp view", sql_temp_view),
    ("spark.sql_filter_unprojected_window", "Filter on a `row_number()` column the `SELECT` drops ᵈ", sql_filter_on_unprojected_window_column),
    ("spark.sql_create_table_default_format", "`CREATE TABLE` with no `USING` defaults to Delta ᵍ", sql_create_table_defaults_to_delta),
    ("spark.sql_describe_registered_delta", "`DESCRIBE TABLE` on a registered Delta table ᵉ", sql_describe_registered_delta_table),
    ("spark.sql_describe_detail_registered", "`DESCRIBE DETAIL` on a registered Delta table ᶠ", sql_describe_detail_registered_delta),
    ("read.text_wholetext", "`read.text(wholetext=True)` — one row per file (must not be inert) ʰ", read_text_wholetext),
    ("read.json_multiline", "`read.json(multiLine=True)` over a JSON-array file ʰ", read_json_multiline),
]


def run_all(spark, engine):
    results = []
    for probe_id, description, fn in PROBES:
        entry = {"id": probe_id, "description": description, "engine": engine}
        try:
            fn(spark)
            entry["status"] = "pass"
        except Exception as exc:  # noqa: BLE001 - the failure detail is the payload
            entry["status"] = "fail"
            entry["error_class"] = type(exc).__name__
            # One line, trimmed: the distinguishing part of these errors is at
            # the front ("No table format found for: memory").
            entry["error"] = " ".join(str(exc).split())[:180]
        results.append(entry)
        print(f"  {probe_id}: {entry['status']}", flush=True)
    return results


def main():
    engine = os.environ.get("ENGINE", "unknown")
    from pyspark.sql import SparkSession

    builder = SparkSession.builder.appName(f"engine-matrix-{engine}")
    remote = os.environ.get("SPARK_REMOTE")
    if remote:
        builder = builder.remote(remote)
    else:
        # A classic JVM session needs Delta wired in explicitly, exactly as
        # e2e/spark-jvm/job.py does. Without it every Delta probe fails with
        # DELTA_CONFIGURE_SPARK_SESSION_WITH_EXTENSION_AND_CATALOG — which
        # measures the harness, not the engine. Sail needs no equivalent: its
        # Delta support is native.
        builder = (builder
                   .config("spark.sql.extensions",
                           "io.delta.sql.DeltaSparkSessionExtension")
                   .config("spark.sql.catalog.spark_catalog",
                           "org.apache.spark.sql.delta.catalog.DeltaCatalog"))
    spark = builder.getOrCreate()

    # The agent normalises byte-size confs per session, so the column that
    # claims to measure the agent's runtime does too. Deliberately keyed to the
    # `/livy` mount rather than to `remote`: only the sail+delta-rs service
    # mounts the agent package, and the BARE sail column must stay unaided —
    # normalising there would report the emulator's behaviour as the engine's.
    # A no-op against an engine already serving a byte count; on one serving
    # `'3GB'` it is what stops pyspark's `int()` from failing. Session-wide
    # rather than per-probe: the delta-rs interception returns its result *as* a
    # createDataFrame, so OPTIMIZE, VACUUM and MERGE all reach it long after the
    # operation itself has succeeded.
    if remote:
        sys.path.insert(0, "/livy")
        try:
            import connectconf
            connectconf.apply(spark)
        except ImportError:
            pass  # bare-sail service mounts probes.py alone; see above

    # The "sail+delta-rs" column measures the emulator's actual runtime, not a
    # bare engine: the Livy agent installs this same wrapper for every Sail
    # session. Importing the agent's module (rather than re-implementing it)
    # keeps the matrix honest — if the agent changes, the column moves with it.
    if os.environ.get("DELTA_OPS"):
        sys.path.insert(0, "/livy")
        import delta_ops
        delta_ops.install(spark)
        print("delta-rs interception installed", flush=True)

    print(f"engine={engine} connected", flush=True)

    results = run_all(spark, engine)
    out = os.environ.get("PROBE_OUT", f"/out/{engine}.json")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, "w") as fh:
        json.dump(results, fh, indent=2)
    print(f"wrote {out}", flush=True)
    # The matrix records failures; a probe failing is data, not an error.
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        traceback.print_exc()
        sys.exit(1)

# ---------------------------------------------------------------------------
