#!/usr/bin/env python3
"""Can a filtered relation cross a process boundary as Arrow and come back whole?

If yes, the system context never stages a snapshot: it reads privileged,
filters, and SENDS the rows to the user context over the protocol those two
already share. No scratch path, no shared volume, no compose change in the
platform repos -- and closer to Fabric, whose system context returns filtered
rows rather than writing them somewhere.

TYPES ARE THE RISK, not row counts. A round trip that turns a timestamp into a
string or a decimal into a float would be a silent corruption of user data, so
the table below is deliberately awkward: nulls, a decimal, a timestamp, a date,
a boolean, a binary column.
"""
import base64
import io
import os
import sys
from datetime import date, datetime
from decimal import Decimal

import pyarrow as pa
import pyarrow.ipc as ipc
from pyspark.sql import SparkSession
from pyspark.sql.types import (
    BinaryType,
    BooleanType,
    DateType,
    DecimalType,
    LongType,
    StringType,
    StructField,
    StructType,
    TimestampType,
)

REMOTE = os.environ.get("SPARK_REMOTE", "sc://sail:50051")
SCHEMA = StructType([
    StructField("region_id", LongType()),
    StructField("label", StringType()),
    StructField("amount", DecimalType(18, 4)),
    StructField("seen_at", TimestampType()),
    StructField("on", DateType()),
    StructField("ok", BooleanType()),
    StructField("blob", BinaryType()),
])
ROWS = [
    (1, "alpha", Decimal("10.5000"), datetime(2026, 8, 23, 1, 2, 3), date(2026, 8, 23),
     True, b"\x00\x01"),
    (1, None, Decimal("-0.0001"), datetime(1999, 12, 31, 23, 59, 59), date(1999, 12, 31),
     False, b""),
    (2, "gamma", Decimal("99999999999999.9999"), datetime(2026, 1, 1, 0, 0, 0),
     date(2026, 1, 1), None, None),
]


def main():
    system = SparkSession.builder.remote(REMOTE).create()
    df = system.createDataFrame(ROWS, SCHEMA)
    df.createOrReplaceTempView("sales")

    # The system context's job: read privileged, apply the filter, hand back
    # only what the caller may see.
    filtered = system.sql("SELECT region_id, label, amount, seen_at, `on`, ok, blob "
                          "FROM sales WHERE region_id = 1")
    expected = filtered.collect()
    print(f"system context filtered to {len(expected)} rows", flush=True)

    try:
        table = filtered.toArrow()
    except Exception as exc:  # noqa: BLE001
        print(f"  toArrow() unavailable: {type(exc).__name__}: {exc}", flush=True)
        print("  falling back to toPandas()", flush=True)
        table = pa.Table.from_pandas(filtered.toPandas(), preserve_index=False)

    sink = io.BytesIO()
    with ipc.new_stream(sink, table.schema) as w:
        w.write_table(table)
    wire = base64.b64encode(sink.getvalue())
    print(f"  on the wire: {len(wire)} base64 bytes for {table.num_rows} rows",
          flush=True)

    # The user context's job: bind the name to what it was given.
    user = SparkSession.builder.remote(REMOTE).create()
    with ipc.open_stream(io.BytesIO(base64.b64decode(wire))) as r:
        received = r.read_all()
    rebuilt = user.createDataFrame(received.to_pandas(), schema=filtered.schema)
    rebuilt.createOrReplaceTempView("sales")

    got = user.sql("SELECT * FROM sales").collect()
    print(f"  user context sees {len(got)} rows", flush=True)

    ok = True
    if [f.name for f in rebuilt.schema] != [f.name for f in filtered.schema]:
        print(f"  COLUMNS DIFFER: {rebuilt.schema} vs {filtered.schema}", flush=True)
        ok = False
    for f_exp, f_got in zip(filtered.schema, rebuilt.schema):
        if f_exp.dataType != f_got.dataType:
            print(f"  TYPE CHANGED: {f_exp.name} {f_exp.dataType} -> {f_got.dataType}",
                  flush=True)
            ok = False
    for a, b in zip(sorted(expected, key=str), sorted(got, key=str)):
        if a != b:
            print(f"  VALUE CHANGED:\n    {a}\n    {b}", flush=True)
            ok = False
    print(f"\nround trip {'PRESERVES' if ok else 'CORRUPTS'} the filtered relation",
          flush=True)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:  # noqa: BLE001 - a probe reports
        import traceback
        traceback.print_exc()
        sys.exit(0)
