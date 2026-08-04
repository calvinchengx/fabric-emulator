#!/usr/bin/env python3
"""Measure the Delta -> SQL analytics endpoint type map along the REAL route.

NOT A CI WITNESS. Nothing runs this automatically, and it is deliberately absent
from docs/witnesses.json — claiming it as a witness would be the exact failure
this repo has already paid for twice (a green row whose code never executed, a
`t.Skip` that hid a broken CRITICAL fix). It is a reproduction recipe: run it by
hand against a running stack when you want to see the map end to end. Wiring it
in is described at the bottom.

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
`varchar` OR `nvarchar` because that consumer does not care which; this one pins
`nvarchar`, because docs/16-warehouse-tds.md commits to it.

Usage (against an already-running stack):

    SPARK_REMOTE=sc://localhost:50051 \\
    TDS_SERVER=localhost,1433 \\
    LAKEHOUSE_ID=<item id> \\
    TDS_TOKEN=<Azure SQL audience JWT> \\
    LAKEHOUSE_TABLES=abfss://<ws>@onelake.../<lake>/Tables \\
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
    ("c_string", "'hello'", "nvarchar"),
    ("c_decimal", "cast(1.5 as decimal(9,2))", "decimal"),
    ("c_binary", "cast('hello' as binary)", "varbinary"),
]
DECIMAL_PRECISION, DECIMAL_SCALE = 9, 2


def write_through_spark(tables_uri):
    """Write the table the way a consumer does: from a notebook on Spark."""
    from pyspark.sql import SparkSession

    spark = SparkSession.builder.remote(os.environ["SPARK_REMOTE"]).getOrCreate()
    select = ", ".join(f"{expr} AS {name}" for name, expr, _ in COLUMNS)
    df = spark.sql(f"SELECT {select}")

    # Printed BEFORE the write, deliberately: if Spark did not write the type
    # you expected, every downstream reading is about a different question.
    print(f"SPARK SCHEMA: {df.schema.simpleString()}")
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

# TO WIRE THIS INTO CI it needs a stack with BOTH a Spark engine and the ODBC
# driver. e2e/medallion already has both (Spark Connect at sc://localhost:50051
# and ODBC Driver 18), so the cheapest real home is a step there rather than a
# new compose file. It is left unwired on purpose rather than added to
# docs/witnesses.json as a claim nothing executes.
