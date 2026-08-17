"""One Spark SQL statement -> one Livy SQL statement output envelope.

Split out of agent.py the same way catalog.py was, for the same reason: agent.py
builds its SparkSession at import, so nothing in it was unit-testable — and the
uint64 recovery below is exactly the kind of branch a refactor could quietly
turn back into the absorption it replaces. This module imports no Spark; the
session arrives in `g`.
"""
import base64
import datetime
import decimal
import traceback


def _jsonable(v):
    """Coerce one Spark value into something json.dumps accepts.

    THE BUG THIS FIXES. The envelope went to json.dumps untouched, so any
    result carrying a DATE — `SELECT current_date()`, or a to_date() cast over
    a bronze column — raised `TypeError: Object of type date is not JSON
    serializable` INSIDE the HTTP handler. The reply was never written, so the
    client saw only `RemoteDisconnected` and could not tell a typed result from
    a network fault; a dbt run reported it as a model error with a truncated
    message. run_sql itself had already succeeded.

    The conversion lives here rather than as a blanket `default=` on the HTTP
    writer's json.dumps: only the SQL path builds typed result sets, and a
    blanket default would also stringify a genuine bug in some unrelated
    response instead of surfacing it.

    Decimal becomes a string, not a float: a warehouse decimal that silently
    loses precision on the way out is the kind of wrong answer that is worse
    than an error. The declared type still reaches the client in the envelope's
    schema, so a reader knows what the string holds.
    """
    if v is None or isinstance(v, (bool, int, float, str)):
        return v
    if isinstance(v, (datetime.datetime, datetime.date, datetime.time)):
        return v.isoformat()
    if isinstance(v, decimal.Decimal):
        return str(v)
    if isinstance(v, (bytes, bytearray)):
        return base64.b64encode(bytes(v)).decode("ascii")
    if isinstance(v, dict):
        return {str(k): _jsonable(x) for k, x in v.items()}
    if isinstance(v, (list, tuple, set)):
        # Covers ARRAY and STRUCT alike: a pyspark Row is a tuple subclass.
        return [_jsonable(x) for x in v]
    return str(v)


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
        rows = [[_jsonable(v) for v in r] for r in df.collect()]
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
                rows = [[_jsonable(v) for v in r.values()] for r in df.toArrow().to_pylist()]
                return {"status": "ok", "execution_count": 0,
                        "data": {"application/json": {
                            "schema": df.schema.jsonValue(), "data": rows}}}
            except Exception:
                # The old absorption, demoted to the fallback of last resort:
                # the write has landed either way.
                return {"status": "ok", "execution_count": 0, "data": {}}
        return {"status": "error", "ename": "SqlError",
                "evalue": tb[-1] if tb else "error", "traceback": tb}
