"""One Spark SQL statement -> one Livy SQL statement output envelope.

Split out of agent.py the same way catalog.py was, for the same reason: agent.py
builds its SparkSession at import, so nothing in it was unit-testable — and the
uint64 recovery below is exactly the kind of branch a refactor could quietly
turn back into the absorption it replaces. This module imports no Spark; the
session arrives in `g`.
"""
import traceback


def run_sql(code, g):
    """Execute one Spark SQL statement, returning a Livy SQL statement output.

    A statement whose plan has output columns (SELECT, SHOW, DESCRIBE, …)
    returns the SQL envelope with schema + rows, which is what a client like
    dbt-fabricspark reads a result set out of. DDL (CREATE, USE, …) has no
    output columns — an empty envelope, which dbt reads as an empty result set.
    DML (INSERT, MERGE) has one: the engine's row count, recovered below.
    """
    spark = g.get("spark")
    if spark is None:
        return {"status": "error", "ename": "NoSparkSession",
                "evalue": "no spark session in this REPL namespace", "traceback": []}
    try:
        df = spark.sql(code)
        if len(df.schema.fields) == 0:
            return {"status": "ok", "execution_count": 0, "data": {}}
        rows = [list(r) for r in df.collect()]
        return {"status": "ok", "execution_count": 0,
                "data": {"application/json": {"schema": df.schema.jsonValue(),
                                              "data": rows}}}
    except Exception:
        tb = traceback.format_exc().splitlines()
        # Sail quirk: DML executes at spark.sql(); collect() then fails
        # CLIENT-SIDE converting DataFusion's uint64 row count (Spark has no
        # unsigned types — the schema even reads bigint; only the Arrow payload
        # is unsigned). The statement's result is a cached relation, so reading
        # it again does not re-run the statement — measured: a 3-row INSERT
        # recovered this way leaves the table at 3 rows, not 6. toArrow() skips
        # the Spark-type conversion that rejects uint64, so the count the
        # engine already reported reaches the caller instead of vanishing into
        # an empty envelope, which is what this branch used to return and what
        # kept parity.md's "DML row-count envelopes" row yellow.
        if "uint64" in tb[-1] or "Unsigned" in tb[-1]:
            try:
                rows = [list(r.values()) for r in df.toArrow().to_pylist()]
                return {"status": "ok", "execution_count": 0,
                        "data": {"application/json": {
                            "schema": df.schema.jsonValue(), "data": rows}}}
            except Exception:
                # The old absorption, demoted to the fallback of last resort:
                # the write has landed either way.
                return {"status": "ok", "execution_count": 0, "data": {}}
        return {"status": "error", "ename": "SqlError",
                "evalue": tb[-1] if tb else "error", "traceback": tb}
