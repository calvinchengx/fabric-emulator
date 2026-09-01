#!/usr/bin/env python3
"""Does Sail still need the MERGE intercept? Run this, do not reason about it.

    uv run --no-project --python 3.12 --with "pysail==<version>" \
      --with "pyspark[connect]==3.5.5" --with setuptools --with pandas \
      --with pyarrow --with deltalake python scripts/probe_sail_merge_premise.py

`python/spark_agent/delta_ops.py` rewrites MERGE because Sail's plan resolver
failed on any Delta target holding a date or timestamp column. That was true
through pysail 0.7.0 and is FALSE on 0.7.1 — which nobody would have noticed,
because the intercept fires first and the statement succeeds either way.

Kept as a script rather than a note so the claim can be RE-MEASURED on the next
bump instead of re-argued. Deliberately NOT in `make check`: it needs pysail and
a running Sail, and the gate stays offline.

THE CONTROL IS THE POINT. If the plain table cannot merge either, the run says
nothing about temporal columns — it says the probe is broken. That case is
reported as inconclusive rather than folded into the verdict.

WHAT IT DOES NOT SETTLE: it merges against a LOCAL PATH. The emulator uses
`az://` OneLake URLs, and e2e/engine-matrix records the storage URL as exactly
what separates the two behaviours for MERGE. A green run here is necessary for
retiring the intercept and nowhere near sufficient.
"""
import os
import tempfile

from pysail.spark import SparkConnectServer
from pyspark.sql import SparkSession

srv = SparkConnectServer(port=0)
srv.start()
_, port = srv.listening_address
spark = SparkSession.builder.remote(f"sc://localhost:{port}").getOrCreate()
base = tempfile.mkdtemp()

def attempt(label, setup, merge):
    p = os.path.join(base, label.replace(" ", "_"))
    os.makedirs(p, exist_ok=True)
    try:
        spark.sql(setup).write.format("delta").mode("overwrite").save(p)
        spark.sql(merge.replace("@P", p))
        n = spark.read.format("delta").load(p).count()
        print(f"  {label:<34} SUCCEEDS (rows={n})")
        return True
    except Exception as e:
        print(f"  {label:<34} FAILS  {str(e).strip().splitlines()[0][:150]}")
        return False

AUDIT = ("SELECT 1 AS id, 'a' AS v, CAST('2026-01-01 00:00:00' AS TIMESTAMP) AS ingested_at, "
         "CAST('2026-01-01' AS DATE) AS d")

print("pysail 0.7.1 — the shapes the intercept exists for")
ctl = attempt("control: plain table, update-only",
              "SELECT 1 AS id, 'a' AS v",
              "MERGE INTO delta.`@P` AS t USING (SELECT * FROM VALUES (1,'b') AS s(id,v)) AS s "
              "ON t.id=s.id WHEN MATCHED THEN UPDATE SET t.v=s.v")
upd = attempt("audit cols, update-only", AUDIT,
              "MERGE INTO delta.`@P` AS t USING (SELECT * FROM VALUES (1,'b') AS s(id,v)) AS s "
              "ON t.id=s.id WHEN MATCHED THEN UPDATE SET t.v=s.v")
ins = attempt("audit cols, UPDATE + INSERT *", AUDIT,
              "MERGE INTO delta.`@P` AS t USING (SELECT 2 AS id, 'c' AS v, "
              "CAST('2026-02-02 00:00:00' AS TIMESTAMP) AS ingested_at, CAST('2026-02-02' AS DATE) AS d) AS s "
              "ON t.id=s.id WHEN MATCHED THEN UPDATE SET t.v=s.v WHEN NOT MATCHED THEN INSERT *")
print()
if not ctl:
    print("INCONCLUSIVE: the control failed, so nothing here separates cause from a broken probe.")
else:
    print("premise 'temporal columns break MERGE':", "STILL TRUE" if not upd else "NO LONGER TRUE")
    print("full medallion shape (INSERT *)      :", "works unaided" if ins else "STILL NEEDS THE INTERCEPT")
srv.stop()
