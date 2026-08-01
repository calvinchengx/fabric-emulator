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
    """A path-addressed merge target — distinct from the registered-table form."""
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
    # The localRelationSizeLimit preset this needs is applied once at session
    # setup in main(), as the real runtime does.
    df = spark.createDataFrame([(1, "a"), (2, "b")], ["id", "name"])
    assert df.count() == 2


def python_udf(spark):
    """Python UDF execution — Sail embeds CPython through pyo3 to run these.

    This failed for a while with `read_udfs() missing 2 required positional
    arguments`, which looked like an engine gap but was a client pin: pyspark
    4.2.0 added two parameters to `pyspark.worker.read_udfs` and pysail 0.6.6
    calls the 3-argument form. Fixed by pinning the client to 4.1.1, the version
    pysail is built against. If this regresses, check that pin before the engine.
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

    Read the row carefully — it is expected to be RED EVERYWHERE, and that is
    the finding. Delta-by-default is a FABRIC property, and neither engine here
    reproduces it. So the override the examples carry is not a Sail workaround
    that a better engine would retire; it is the price of running dbt-fabricspark
    anywhere that is not Fabric, and it would still be needed on the JVM overlay.
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

    e2e/livy/delta_ops.py needs exactly this: when a statement names a
    registered table rather than a path, the delta-rs interception has to find
    where the table actually lives before it can act on it.

    Sail does not have DETAIL in its DESCRIBE grammar at all, so this raises
    rather than returning nothing. That is the BETTER failure — it is loud, and
    `collect()[0]` never runs on an empty list — but it does mean the
    name-resolution route is unavailable on Sail, which is why OPTIMIZE against
    a named table degrades to skipped there.

    Recorded separately from the DESCRIBE TABLE row ᵉ because the two fail in
    opposite ways, and the difference is the whole lesson: one is silent and
    cost a day, the other is noisy and cost minutes.
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

    # e2e/livy/agent.py presets this for every Connect session: Sail reports
    # localRelationSizeLimit as the string "3GB" and pyspark's client calls
    # int() on it. A client-compat quirk, not an engine capability, so the probe
    # applies the same preset the real runtime does — otherwise it reports gaps
    # the emulator does not have. Session-wide, not per-probe: the delta-rs
    # interception returns its result *as* a createDataFrame, so OPTIMIZE and
    # VACUUM hit this too and failed on '3GB' long after the operation itself
    # had succeeded.
    if remote:
        try:
            spark.conf.set("spark.sql.session.localRelationSizeLimit", str(64 * 1024 * 1024))
        except Exception:  # noqa: BLE001 — engine not reachable yet
            pass

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
