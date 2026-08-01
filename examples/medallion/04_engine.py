"""Execute the queued notebook run on a real Spark engine, and report back.

**The emulator never executes notebooks.** It parses a Notebook item into
ordered code cells and records a `Pending` run; an engine then executes those
cells and reports the outcome to `notebookRunResult`. Real Fabric does the same
thing with its own Spark pools behind the curtain — here the engine is Sail
(Rust Spark Connect, no JVM) and this script plays the driver, exactly as
`e2e/notebook-run` does.

Reporting the read/write set alongside the results is what turns the run into
lineage. The emulator records those edges verbatim; it never parses the
notebook's code to guess what it touched.
"""
import io
import time
from contextlib import redirect_stdout

from common import FABRIC, S, SPARK_REMOTE, fabric_headers, load, log

st = load()
jid, nb = st["orders_job"], st["orders_notebook"]
remote = SPARK_REMOTE
assert remote, "SPARK_REMOTE is empty — no Spark engine is attached to run the notebook"

# The cells the EMULATOR parsed, not the source we uploaded: the run is what an
# engine is asked to execute.
run = S.get(f"{FABRIC}/v1/workspaces/{st['workspace']}/items/{nb}/jobs/instances/{jid}/notebookRun",
            headers=fabric_headers())
run.raise_for_status()
cells = sorted(run.json()["cells"], key=lambda c: c["index"])
log(f"emulator parsed {len(cells)} cell(s); connecting to Spark at {remote}")

from pyspark.sql import SparkSession  # noqa: E402  (only needed on the engine path)

for attempt in range(30):
    try:
        spark = SparkSession.builder.remote(remote).getOrCreate()
        spark.sql("SELECT 1").collect()
        break
    except Exception:  # noqa: BLE001 — the engine may still be binding its port
        if attempt == 29:
            raise
        time.sleep(2)

results, overall = [], "Completed"
ns = {"spark": spark, "__name__": "__nb__"}
for c in cells:
    buf = io.StringIO()
    try:
        with redirect_stdout(buf):
            exec(c["source"], ns)  # noqa: S102 — running notebook code IS the engine's job
        results.append({"index": c["index"], "status": "Succeeded", "output": buf.getvalue()})
    except Exception as e:  # noqa: BLE001
        results.append({"index": c["index"], "status": "Failed", "error": str(e)})
        overall = "Failed"
        break

# What the cell actually read and wrote. The engine knows because it ran the
# code; the emulator would have to guess, so it never does.
lake = st["lakehouse"]
if results and results[0]["status"] == "Succeeded":
    results[0]["reads"] = [{"itemId": lake, "path": f"Files/landing/contoso_pos/{st['landing_date']}/orders.jsonl"}]
    results[0]["writes"] = [{"itemId": lake, "path": "Tables/bronze_orders"}]

r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/items/{nb}/jobs/instances/{jid}/notebookRunResult",
           headers=fabric_headers(), json={"status": overall, "cells": results})
assert r.status_code == 200, f"report: {r.status_code} {r.text}"
assert overall == "Completed", f"notebook failed on Spark: {results}"
log(f"Spark executed {len(results)} cell(s) and reported the run + its read/write set")
