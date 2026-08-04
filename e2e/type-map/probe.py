#!/usr/bin/env python3
"""Measure the Delta -> SQL analytics endpoint type map along the REAL route.

RUN BY e2e/medallion, after the tutorial pipeline has built a lakehouse — that
suite already has both halves this needs, a Spark engine and the ODBC driver, so
it is the cheapest honest home. It is also runnable by hand against any stack.

WHY IT EXISTS ALONGSIDE THE GO TESTS. `internal/warehouse/types_test.go` proves
the MAP: given a Parquet column of each logical type, the right SQL type comes
out. It does not prove the ROUTE. Reflection exists for Delta written by
something ELSE — a notebook on Spark — and read back by a real ODBC client, and
no Go test can cover that seam because neither end is Go. This probe covers it,
and the gap was found by a consumer (contoso-data-platform), not by us.

WHAT IT ASSERTS, and why each part earns its place:

  * INFORMATION_SCHEMA.DATA_TYPE per column — the loud failure. A `date`
    surfacing as `bigint` is what broke a real dbt model.
  * A join against a real date — NOT redundant with the above. The schema query
    catches a wrong TYPE; the join catches a right type carrying a wrong VALUE.
    Different failure, and the second one is silent.
  * decimal by (precision, scale), not by name — a column that reflects as
    `decimal` while losing its scale passes a name check and is wrong by
    10^scale in every aggregate.
  * The SPARK schema, printed before the write — this is what separates "the
    endpoint mis-reflected the type" from "Spark never wrote the type you
    think", which look identical from the SQL side.

Adapted from throwaway probes written in contoso-data-platform, whose author
measured the original failure and re-measured the fix. Their version tolerated
`varchar` OR `nvarchar`; I first pinned `nvarchar` here and was WRONG — Fabric
documents `STRING -> varchar(8000)` and says of nvarchar "there's no similar
unicode data type in Parquet". It pins `varchar`.

NESTED COLUMNS ARE NOT LISTED BELOW ON PURPOSE. Fabric does not represent
struct/array/map at all, so the faithful behaviour is that those columns are
ABSENT from INFORMATION_SCHEMA. Write one if you want to check that — the
assertion is that it does not appear and that the columns around it are still
correct, which is what a nested column used to destroy.

Usage. Inside an example's project it needs nothing but the endpoints, because
it borrows that example's own state and token helpers:

    uv run --project examples/medallion-pyspark python e2e/type-map/probe.py

Standalone, supply what those helpers would have resolved:

    SPARK_REMOTE=sc://localhost:50051 TDS_SERVER=localhost,1433 \\
    LAKEHOUSE_ID=<item id> TDS_TOKEN=<Azure SQL audience JWT> \\
    LAKEHOUSE_TABLES=abfs://<ws>@onelake.dfs.fabric.microsoft.com/<lake>/Tables \\
        python e2e/type-map/probe.py
"""
import os
import struct
import sys

TABLE = "probe_types"

# The nine columns, their Spark expression, and the type the SQL analytics
# endpoint must report. Sourced from docs/16-warehouse-tds.md — if that table
# and this dict disagree, one of them is wrong and it is worth finding out which.
COLUMNS = [
    ("c_date", "to_date('2026-07-15')", "date"),
    ("c_timestamp", "to_timestamp('2026-07-15T20:00:00')", "datetime2"),
    ("c_int", "cast(42 as int)", "int"),
    ("c_bigint", "cast(42 as bigint)", "bigint"),
    ("c_double", "cast(1.5 as double)", "float"),
    ("c_boolean", "true", "bit"),
    ("c_string", "'hello'", "varchar"),
    ("c_decimal", "cast(1.5 as decimal(9,2))", "decimal"),
    ("c_binary", "cast('hello' as binary)", "varbinary"),
]
DECIMAL_PRECISION, DECIMAL_SCALE = 9, 2


def bootstrap():
    """Fill LAKEHOUSE_ID / TDS_TOKEN / LAKEHOUSE_TABLES from the example's own
    state, when running inside an example project that has already provisioned.

    Borrowed rather than reimplemented: `common` is the module the medallion
    example itself uses to mint a token and find its lakehouse, so the probe
    reaches the endpoint by exactly the route the tutorial documents. Absent
    (running standalone), the environment has to supply them.
    """
    try:
        from common import SQL_AUD, load, token  # type: ignore
    except ImportError:
        return
    st = load()
    lake, workspace = st.get("lakehouse"), st.get("workspace")
    if not lake or not workspace:
        return
    os.environ.setdefault("LAKEHOUSE_ID", lake)
    os.environ.setdefault("TDS_TOKEN", token(SQL_AUD))
    os.environ.setdefault(
        "LAKEHOUSE_TABLES",
        f"abfs://{workspace}@onelake.dfs.fabric.microsoft.com/{lake}/Tables")


def write_through_spark(tables_uri):
    """Write the table the way a consumer does: from a notebook on Spark."""
    from pyspark.sql import SparkSession

    spark = SparkSession.builder.remote(os.environ["SPARK_REMOTE"]).getOrCreate()
    select = ", ".join(f"{expr} AS {name}" for name, expr, _ in COLUMNS)
    df = spark.sql(f"SELECT {select}")

    # Printed BEFORE the write, deliberately: if Spark did not write the type
    # you expected, every downstream reading is about a different question.
    schema = df.schema.simpleString()
    print(f"SPARK SCHEMA: {schema}")

    # And CHECKED, not merely printed. If the engine cannot produce one of these
    # types, the endpoint will reflect something odd and the failure will read
    # as a type-map bug — blaming the wrong component. Naming it here as a
    # WRITE-side limitation keeps that diagnosis honest.
    missing = [name for name, _, _ in COLUMNS if f"{name}:" not in schema]
    if missing:
        sys.exit(f"FAIL (write side, not the type map): the Spark engine did not "
                 f"produce {missing} — the endpoint cannot reflect what was never "
                 f"written. Schema was: {schema}")
    (df.write.format("delta").mode("overwrite")
       .option("overwriteSchema", "true").save(f"{tables_uri}/{TABLE}"))
    print(f"WROTE {TABLE}")


def read_through_tds():
    """Read the reflected types back the way a client does: ODBC over TDS."""
    import pyodbc

    # SQL_COPT_SS_ACCESS_TOKEN (1256): 4-byte length + UTF-16-LE token. Same
    # encoding as e2e/dbt-fabric/runner.py, which is the canonical copy.
    enc = os.environ["TDS_TOKEN"].encode("utf-16-le")
    attrs = {1256: struct.pack("<i", len(enc)) + enc}
    dsn = ("DRIVER={ODBC Driver 18 for SQL Server};"
           f"SERVER={os.environ['TDS_SERVER']};"
           f"DATABASE={os.environ['LAKEHOUSE_ID']};"
           "Encrypt=no;TrustServerCertificate=yes")

    want = {name: sql for name, _, sql in COLUMNS}
    bad = []
    with pyodbc.connect(dsn, attrs_before=attrs, timeout=30) as conn:
        cur = conn.cursor()
        rows = cur.execute(
            "SELECT COLUMN_NAME, DATA_TYPE, NUMERIC_PRECISION, NUMERIC_SCALE "
            "FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_NAME = ? "
            "ORDER BY ORDINAL_POSITION", TABLE).fetchall()
        if not rows:
            sys.exit(f"FAIL: {TABLE} has no columns — was it reflected at all?")

        for name, dtype, precision, scale in rows:
            expected = want.get(name, "?")
            ok = dtype == expected
            extra = ""
            if dtype == "decimal":
                # By the NUMBERS, not the name: a decimal that keeps its type
                # and loses its scale is wrong by 10^scale everywhere.
                extra = f"({precision},{scale})"
                ok = ok and (precision, scale) == (DECIMAL_PRECISION, DECIMAL_SCALE)
                expected += f"({DECIMAL_PRECISION},{DECIMAL_SCALE})"
            print(f"  {name:<13} -> {dtype}{extra:<8} expected {expected:<14}"
                  f"{'OK' if ok else 'MISMATCH'}")
            if not ok:
                bad.append((name, f"{dtype}{extra}", expected))

        # The quiet half. A right DATA_TYPE over a wrong stored value passes
        # every check above and fails here.
        try:
            matched = cur.execute(
                f"SELECT COUNT(*) FROM {TABLE} "
                "WHERE c_date = CAST('2026-07-15' AS date)").fetchval()
            print(f"  join-as-date  -> matched {matched} row(s)      "
                  f"{'OK' if matched == 1 else 'MISMATCH'}")
            if matched != 1:
                bad.append(("c_date", f"join matched {matched}", "1"))
        except Exception as exc:  # noqa: BLE001 — the message IS the finding
            print(f"  join-as-date  -> FAILED: {str(exc)[:140]}")
            bad.append(("c_date", "join raised", "a matching row"))
    return bad


def main() -> int:
    bootstrap()
    tables = os.environ.get("LAKEHOUSE_TABLES")
    if tables:
        write_through_spark(tables)
    else:
        print("LAKEHOUSE_TABLES unset — reading an existing probe_types only")

    bad = read_through_tds()
    if bad:
        print(f"\nVERDICT: {len(bad)} column(s) wrong: {bad}")
        return 1
    print(f"\nVERDICT: all {len(COLUMNS)} types correct, and a date joins as a date")
    return 0


if __name__ == "__main__":
    sys.exit(main())

