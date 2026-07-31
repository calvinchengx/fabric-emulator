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
def _rate_stream(spark):
    return spark.readStream.format("rate").option("rowsPerSecond", 5).load()


def streaming_read_rate(spark):
    df = _rate_stream(spark)
    assert "timestamp" in df.schema.simpleString()


def _try_sink(spark, fmt, **opts):
    wr = _rate_stream(spark).writeStream.format(fmt).outputMode("append")
    for k, v in opts.items():
        wr = wr.option(k, v)
    q = wr.start()
    time.sleep(4)
    active = q.isActive
    q.stop()
    assert active, f"{fmt} sink started but was not active"


def streaming_sink_console(spark):
    _try_sink(spark, "console")


def streaming_sink_memory(spark):
    wr = _rate_stream(spark).writeStream.format("memory").queryName("probe_mem").outputMode("append")
    q = wr.start()
    time.sleep(4)
    q.stop()


def streaming_sink_parquet(spark):
    _try_sink(spark, "parquet", path=_table_path("s_pq"),
              checkpointLocation=_table_path("s_pq_ck"))


def streaming_sink_delta(spark):
    _try_sink(spark, "delta", path=_table_path("s_delta"),
              checkpointLocation=_table_path("s_delta_ck"))


# ------------------------------------------------------- JVM-only surface
def rdd_sparkcontext(spark):
    assert spark.sparkContext.parallelize([1, 2, 3, 4]).map(lambda x: x * 2).sum() == 20


def jvm_bridge(spark):
    assert spark._jvm.java.lang.String is not None


# ----------------------------------------------------------- general Spark
def create_dataframe_local_rows(spark):
    # e2e/livy/agent.py presets this for every Connect session: Sail reports
    # localRelationSizeLimit as the string "3GB" and pyspark's client calls
    # int() on it. That is a client-compat quirk, not an engine capability, so
    # the probe applies the same preset the real runtime does — otherwise it
    # would report a gap the emulator does not actually have.
    try:
        spark.conf.set("spark.sql.session.localRelationSizeLimit", str(64 * 1024 * 1024))
    except Exception:  # noqa: BLE001 — classic sessions do not need it
        pass
    df = spark.createDataFrame([(1, "a"), (2, "b")], ["id", "name"])
    assert df.count() == 2


def python_udf(spark):
    """Python UDF execution.

    Known caveat on Sail via Spark Connect in this harness: the worker rejects
    the call with `read_udfs() missing 2 required positional arguments`, a
    pyspark client/worker protocol version mismatch rather than a missing
    capability — Sail embeds CPython through pyo3 specifically to run these.
    Treat a failure here as a version-pinning problem to fix, not an engine gap.
    """
    from pyspark.sql.functions import udf
    from pyspark.sql.types import IntegerType
    double = udf(lambda x: x * 2, IntegerType())
    rows = spark.sql("SELECT 21 AS n").select(double("n").alias("d")).collect()
    assert rows[0]["d"] == 42


def sql_temp_view(spark):
    spark.sql("SELECT 1 AS id").createOrReplaceTempView("v_probe")
    assert spark.sql("SELECT count(*) AS n FROM v_probe").collect()[0]["n"] == 1


PROBES = [
    ("delta.write", "Delta write", delta_write),
    ("delta.append", "Delta append", delta_append),
    ("delta.time_travel_dataframe", "Time travel — `option(\"versionAsOf\")`", delta_time_travel_dataframe),
    ("delta.time_travel_sql", "Time travel — SQL `VERSION AS OF`", delta_time_travel_sql),
    ("delta.merge_registered", "`MERGE INTO` a registered table at a **local path** ᵃ", delta_merge_registered_table),
    ("delta.merge_path", "`MERGE INTO delta.`path`` (path target)", delta_merge_path_target),
    ("delta.optimize", "`OPTIMIZE`", delta_optimize),
    ("delta.vacuum", "`VACUUM`", delta_vacuum),
    ("delta.cdf", "Change Data Feed (must not be inert)", delta_change_data_feed),
    ("streaming.read_rate", "`readStream` (rate source)", streaming_read_rate),
    ("streaming.sink_console", "Streaming sink — console", streaming_sink_console),
    ("streaming.sink_memory", "Streaming sink — memory", streaming_sink_memory),
    ("streaming.sink_parquet", "Streaming sink — parquet", streaming_sink_parquet),
    ("streaming.sink_delta", "Streaming sink — **delta**", streaming_sink_delta),
    ("jvm.rdd_sparkcontext", "`sc` / RDD API", rdd_sparkcontext),
    ("jvm.bridge", "`spark._jvm` bridge", jvm_bridge),
    ("spark.create_dataframe_local", "`createDataFrame(local_rows)`", create_dataframe_local_rows),
    ("spark.python_udf", "Python UDF ᵇ", python_udf),
    ("spark.sql_temp_view", "SQL over a temp view", sql_temp_view),
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
