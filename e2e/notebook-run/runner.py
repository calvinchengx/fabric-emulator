"""Real notebook cell execution on Spark, end to end.

Runs inside the Spark container. The flow mirrors how Fabric runs a notebook on
a Spark pool that reports back to the service:

  1. Publish a real Fabric notebook (multi-cell notebook-content.py) as a
     Notebook item, then submit a RunNotebook job.
  2. The **emulator parses** the notebook into ordered code cells (its Go
     parser) and records a Pending run — we fetch those cells back.
  3. **Real Spark executes** each cell in a shared kernel namespace against the
     emulator's OneLake plane (ABFS): a pyspark cell writes a Delta table, a
     %%sql cell queries it, a final cell computes a value and exits.
  4. The runner POSTs the per-cell results + exit value to the emulator, which
     finalises the run and the job's terminal status.
  5. Assertions: the job is really Completed, the Delta table landed in OneLake,
     and the exit value round-tripped — cells genuinely ran, not clock-derived.
"""
import base64
import io
import json
import sys
import traceback
import urllib.error
import urllib.parse
import urllib.request
from contextlib import redirect_stdout

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
CLIENT_SECRET = "daemon-app-secret"
ACCT = "onelake.dfs.fabric.microsoft.com"

# A real Fabric notebook: pyspark write → %%sql query → compute + exit.
NOTEBOOK = '''# Fabric notebook source

# CELL ********************
df = spark.createDataFrame([(1, "a"), (2, "b"), (3, "c")], ["id", "name"])
df.write.format("delta").mode("overwrite").save(TABLE_PATH)
df.createOrReplaceTempView("events")
print("wrote", df.count(), "rows")

# MARKDOWN ********************
# MAGIC %md
# MAGIC ## Count the rows

# CELL ********************
# MAGIC %%sql
# MAGIC SELECT count(*) AS n FROM events

# CELL ********************
total = spark.read.format("delta").load(TABLE_PATH).count()
notebook_exit(str(total))
'''


def req(method, url, body=None, token=None, form=False):
    data, headers = None, {}
    if body is not None:
        if form:
            data = urllib.parse.urlencode(body).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        else:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    r = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(r) as resp:
        raw = resp.read()
        # resp.headers is an email.message.Message — case-insensitive .get(),
        # which matters because Go canonicalises header names (X-Ms-Operation-Id).
        return resp.status, resp.headers, (json.loads(raw) if raw else {})


def log(m):
    print(f"==> {m}", flush=True)


# --- control plane: tokens, workspace, lakehouse ----------------------------
try:
    req("POST", f"{ENTRA}/admin/api/apps",
        {"displayName": "Azure Storage", "appIdUri": "https://storage.azure.com", "isConfidential": False})
except urllib.error.HTTPError as e:
    if e.code != 409:
        raise

ft = req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials", "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET, "scope": "https://api.fabric.microsoft.com/.default"}, form=True)[2]["access_token"]

ws = req("POST", f"{FABRIC}/v1/workspaces", {"displayName": "nb-ws"}, token=ft)[2]["id"]
req("POST", f"{FABRIC}/v1/workspaces/{ws}/lakehouses", {"displayName": "lake"}, token=ft)
log(f"workspace {ws}")

# Publish the notebook item with its definition (async create → poll the LRO).
_, hdrs, _ = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items", {
    "displayName": "etl-nb", "type": "Notebook",
    "definition": {"parts": [{
        "path": "notebook-content.py", "payloadType": "InlineBase64",
        "payload": base64.b64encode(NOTEBOOK.encode()).decode()}]}}, token=ft)
opid = hdrs.get("x-ms-operation-id")
nb = None
for _ in range(60):
    st, _, body = req("GET", f"{FABRIC}/v1/operations/{opid}", token=ft)
    if body.get("status") == "Succeeded":
        nb = req("GET", f"{FABRIC}/v1/operations/{opid}/result", token=ft)[2]["id"]
        break
if not nb:
    raise SystemExit("notebook item create did not complete")
log(f"notebook {nb}")

# Submit a RunNotebook job; the emulator parses the notebook now.
_, hdrs, _ = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances?jobType=RunNotebook", token=ft)
jid = hdrs["Location"].rstrip("/").rsplit("/", 1)[-1]

# Fetch the cells the EMULATOR parsed (proves its Go parser produced the work).
run = req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}/notebookRun", token=ft)[2]
cells = sorted(run["cells"], key=lambda c: c["index"])
log(f"emulator parsed {len(cells)} code cells: {[c['language'] for c in cells]}")
assert [c["language"] for c in cells] == ["python", "sql", "python"], cells

# --- real Spark executes the cells ------------------------------------------
from pyspark.sql import SparkSession  # noqa: E402

spark = (SparkSession.builder.appName("fabric-emu-notebook")
         .config("spark.sql.extensions", "io.delta.sql.DeltaSparkSessionExtension")
         .config("spark.sql.catalog.spark_catalog", "org.apache.spark.sql.delta.catalog.DeltaCatalog")
         .config("spark.hadoop.fs.azure.always.use.https", "false")
         .config(f"spark.hadoop.fs.azure.account.auth.type.{ACCT}", "Custom")
         .config(f"spark.hadoop.fs.azure.account.oauth.provider.type.{ACCT}",
                 "com.calvinchengx.fabricemu.EntraTokenProvider")
         .config("spark.hadoop.fs.azure.emu.token.endpoint", f"{ENTRA}/{TENANT}/oauth2/v2.0/token")
         .config("spark.hadoop.fs.azure.emu.client.id", CLIENT_ID)
         .config("spark.hadoop.fs.azure.emu.client.secret", CLIENT_SECRET)
         .config("spark.hadoop.fs.azure.emu.scope", "https://storage.azure.com/.default")
         .getOrCreate())
spark.sparkContext.setLogLevel("WARN")

TABLE_PATH = f"abfs://{ws}@{ACCT}/lake.Lakehouse/Tables/events"


class _Exit(Exception):
    def __init__(self, value):
        self.value = value


def notebook_exit(value=""):
    raise _Exit(value)


ns = {"spark": spark, "TABLE_PATH": TABLE_PATH, "notebook_exit": notebook_exit, "__name__": "__nb__"}
results, exit_value, overall = [], "", "Completed"
for c in cells:
    buf = io.StringIO()
    try:
        with redirect_stdout(buf):
            if c["language"] == "sql":
                print(spark.sql(c["source"]).collect())
            else:
                exec(compile(c["source"], f"<cell {c['index']}>", "exec"), ns)
        results.append({"index": c["index"], "status": "Succeeded", "output": buf.getvalue().strip()})
    except _Exit as e:
        exit_value = e.value
        results.append({"index": c["index"], "status": "Succeeded", "output": buf.getvalue().strip()})
        break
    except Exception:
        overall = "Failed"
        results.append({"index": c["index"], "status": "Failed",
                        "output": buf.getvalue().strip(), "error": traceback.format_exc()})
        break

log(f"executed cells: {[(r['index'], r['status']) for r in results]}, exit={exit_value!r}")

# Report the real run back to the emulator.
req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}/notebookRunResult",
    {"status": overall, "exitValue": exit_value, "cells": results}, token=ft)

# --- assertions: the run is real -------------------------------------------
job = req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}", token=ft)[2]
assert job["status"] == "Completed", f"job status {job['status']}"

detail = req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}/notebookRun", token=ft)[2]
assert detail["status"] == "Completed" and detail["exitValue"] == "3", detail

rows = sorted((r["id"], r["name"]) for r in spark.read.format("delta").load(TABLE_PATH).collect())
assert rows == [(1, "a"), (2, "b"), (3, "c")], rows

spark.stop()
log(f"delta table in OneLake: {rows}")
print("NOTEBOOK-RUN E2E: PASS", flush=True)
sys.exit(0)
