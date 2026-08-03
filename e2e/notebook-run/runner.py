"""Real notebook cell execution on Spark, end to end.

Runs inside the Spark container. The flow mirrors how Fabric runs a notebook on
a Spark pool that reports back to the service:

  1. Publish a real Fabric notebook (multi-cell notebook-content.py) as a
     Notebook item, then submit a RunNotebook job.
  2. The **emulator parses** the notebook into ordered code cells (its Go
     parser) and records a Pending run — we fetch those cells back.
  3. The selected engine executes each cell in a shared kernel namespace
     against the emulator's OneLake plane (ABFS): a PySpark cell writes a Delta table, a
     %%sql cell queries it, a final cell computes a value and exits.
  4. The runner POSTs the per-cell results + exit value to the emulator, which
     finalises the run and the job's terminal status.
  5. Assertions: the run detail reaches Completed with the exit value, and the
     Delta table is verified IN ONELAKE over plain HTTP — commit log, Parquet
     magic bytes, byte lengths and row count — by a reader that is not Spark.

A note on what proves what. The job instance's status is derived from the
emulator's CLOCK, so it reads "Completed" even when no engine ran a single cell
(see TestJobStatusIsNotEvidenceOfNotebookExecution). Asserting on it here would
pass with the whole engine switched off. The honest witnesses are the run detail
— which only leaves Pending when an engine reports back — and the bytes in
OneLake, so those are what this asserts.
"""
import base64
import io
import json
import importlib
import os
import subprocess
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
NOTEBOOK_BODY = '''# Fabric notebook source

# CELL ********************
df = spark.createDataFrame([(1, "a"), (2, "b"), (3, "c")], ["id", "name"])
df.write.format("delta").mode("overwrite").saveAsTable("events")
print("wrote", df.count(), "rows")

# MARKDOWN ********************
# MAGIC %md
# MAGIC ## Count the rows

# CELL ********************
# MAGIC %%sql
# MAGIC SELECT count(*) AS n FROM events

# CELL ********************
total = spark.table("events").count()
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


def inline_part(path, content):
    return {"path": path, "payloadType": "InlineBase64",
            "payload": base64.b64encode(content.encode()).decode()}


def publish_item(display_name, item_type, parts):
    _, headers, _ = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items", {
        "displayName": display_name, "type": item_type,
        "definition": {"parts": parts}}, token=ft)
    opid = headers.get("x-ms-operation-id")
    for _ in range(60):
        body = req("GET", f"{FABRIC}/v1/operations/{opid}", token=ft)[2]
        if body.get("status") == "Succeeded":
            return req("GET", f"{FABRIC}/v1/operations/{opid}/result", token=ft)[2]["id"]
    raise RuntimeError(f"{item_type} item create did not complete")


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
lake = req("POST", f"{FABRIC}/v1/workspaces/{ws}/lakehouses", {"displayName": "lake"}, token=ft)[2]
log(f"workspace {ws}")

# Attach a portable Environment and the default lakehouse through real Fabric
# notebook metadata. The runner consumes only the resolved run contract.
env = publish_item("runtime", "Environment", [
    inline_part("requirements.txt", "cloudpickle\n"),
    inline_part("Setting/Sparkcompute.json", json.dumps({
        "sparkProperties": {"spark.sql.shuffle.partitions": 7}})),
])
metadata = {
    "kernel_info": {"name": "synapse_pyspark"},
    "dependencies": {
        "lakehouse": {"default_lakehouse": lake["id"],
                      "default_lakehouse_name": lake["displayName"],
                      "default_lakehouse_workspace_id": ws},
        "environment": {"environmentId": env, "workspaceId": ws},
    },
}
meta = "# METADATA ********************\n" + "\n".join(
    "# META " + line for line in json.dumps(metadata, indent=2).splitlines()) + "\n"
nb = publish_item("etl-nb", "Notebook", [inline_part("notebook-content.py", NOTEBOOK_BODY + meta)])
log(f"notebook {nb}")

# Submit a RunNotebook job; the emulator parses the notebook now.
_, hdrs, _ = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances?jobType=RunNotebook", token=ft)
jid = hdrs["Location"].rstrip("/").rsplit("/", 1)[-1]

# Fetch the cells the EMULATOR parsed (proves its Go parser produced the work).
run = req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}/notebookRun", token=ft)[2]
cells = sorted(run["cells"], key=lambda c: c["index"])
log(f"emulator parsed {len(cells)} code cells: {[c['language'] for c in cells]}")
assert [c["language"] for c in cells] == ["python", "sql", "python"], cells
assert run["binding"] == {"workspaceId": ws, "lakehouseId": lake["id"],
                           "lakehouseName": "lake", "environmentId": env,
                           "environmentWorkspaceId": ws}, run["binding"]
assert run["environment"]["pythonPackages"] == ["cloudpickle"], run["environment"]

# --- selected compute engine executes the cells -----------------------------
from pyspark.sql import SparkSession  # noqa: E402

import time

TABLE_PATH = f"abfs://{ws}@{ACCT}/lake.Lakehouse/Tables/events"
TABLES_PATH = TABLE_PATH.rsplit("/", 1)[0]

if os.environ.get("SPARK_REMOTE"):
    # Sail: auth and endpoint configuration live on the Spark Connect server.
    for _attempt in range(30):
        try:
            spark = SparkSession.builder.remote(os.environ["SPARK_REMOTE"]).getOrCreate()
            spark.sql("SELECT 1").collect()
            break
        except Exception:
            if _attempt == 29:
                raise
            time.sleep(2)
    spark.conf.set("spark.sql.session.localRelationSizeLimit", str(64 * 1024 * 1024))
    engine = "sail"
else:
    # JVM oracle: the same notebook runs on the Spark 3.5 / Delta 3.2 baseline
    # used by Fabric Runtime 1.3, with ABFS authenticated by the test provider.
    spark = (SparkSession.builder.appName("fabric-emulator-notebook-jvm")
             .config("spark.sql.extensions", "io.delta.sql.DeltaSparkSessionExtension")
             .config("spark.sql.catalog.spark_catalog", "org.apache.spark.sql.delta.catalog.DeltaCatalog")
             .config("spark.sql.warehouse.dir", TABLES_PATH)
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
    engine = "jvm-spark-3.5"
log(f"compute engine: {engine}")

# Apply the Environment before user code. Python declarations are verified in
# the actual kernel and Spark properties are applied to the actual session.
environment_site = "/tmp/fabric-environment"
os.makedirs(environment_site, exist_ok=True)
sys.path.insert(0, environment_site)
for package in run["environment"].get("pythonPackages", []):
    module = package.split("=", 1)[0].split(">", 1)[0].replace("-", "_")
    try:
        importlib.import_module(module)
    except ModuleNotFoundError:
        subprocess.run(
            ["uv", "pip", "install", "--quiet", "--target", environment_site, package],
            check=True,
            env={**os.environ, "UV_CACHE_DIR": "/tmp/uv-cache"},
        )
        importlib.import_module(module)
for key, value in run["environment"].get("sparkConfig", {}).items():
    spark.conf.set(key, value)
assert spark.conf.get("spark.sql.shuffle.partitions") == "7"

# Bind the session catalog to the attached lakehouse. Unqualified Fabric table
# APIs now resolve to OneLake Tables/ instead of a runner-injected file path.
if engine == "sail":
    spark.sql(f"CREATE DATABASE IF NOT EXISTS lake LOCATION '{TABLES_PATH}'")
    spark.catalog.setCurrentDatabase("lake")


class _Exit(Exception):
    def __init__(self, value):
        self.value = value


def notebook_exit(value=""):
    raise _Exit(value)


ns = {"spark": spark, "notebook_exit": notebook_exit, "__name__": "__nb__"}
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
if overall == "Failed":
    print(results[-1]["error"], flush=True)

# Report the real run back to the emulator.
req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}/notebookRunResult",
    {"status": overall, "exitValue": exit_value, "cells": results}, token=ft)

# --- assertions: the run is real -------------------------------------------
# Kept, but it is the WEAKEST check here and must not be read as proof of
# execution: this value is clock-derived and reads "Completed" with no engine
# at all. It is asserted only so a regression that made it *disagree* with the
# run detail would surface.
job = req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}", token=ft)[2]
assert job["status"] == "Completed", f"job status {job['status']}"

detail = req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}/notebookRun", token=ft)[2]
assert detail["status"] == "Completed" and detail["exitValue"] == "3", detail

rows = sorted((r["id"], r["name"]) for r in spark.table("events").collect())
assert rows == [(1, "a"), (2, "b"), (3, "c")], rows

# --- the table is REALLY in OneLake, read by something that is not Spark ----
#
# The assertion above goes back through the same session that wrote the table,
# so it cannot tell "Delta landed in OneLake" from "this Spark session still
# remembers a table". Both look identical from inside the writer. Everything
# below therefore reads the storage plane over plain HTTP with a STORAGE-
# audience token: different process, different protocol, different credential,
# no Spark anywhere in the path.
#
# What it establishes, in order: a Delta commit log exists; the log names
# Parquet files; those files are really Parquet (magic bytes at BOTH ends —
# the trailing one is what a truncated upload loses); their byte length agrees
# with the size the commit recorded; and the row count the commit claims is the
# row count the notebook wrote. Two independent records of the same write have
# to agree, which is a stronger statement than either alone.
log("verifying the Delta table in OneLake over HTTP (no Spark)")

st_tok = req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials", "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET,
    "scope": "https://storage.azure.com/.default"}, form=True)[2]["access_token"]

TABLE_DIR = f"{lake['displayName']}.Lakehouse/Tables/events"


def onelake_get(path, want_json=False):
    """GET one path from the OneLake DFS plane. Returns raw bytes."""
    url = f"http://{ACCT}/{ws}/{urllib.parse.quote(path)}"
    r = urllib.request.Request(url, headers={"Authorization": "Bearer " + st_tok})
    with urllib.request.urlopen(r) as resp:
        raw = resp.read()
    return json.loads(raw) if want_json else raw


listing = req("GET", f"http://{ACCT}/{ws}?resource=filesystem&recursive=true"
                     f"&directory={urllib.parse.quote(TABLE_DIR)}", token=st_tok)[2]
names = [p["name"] for p in listing.get("paths", [])]
assert names, f"OneLake has nothing under {TABLE_DIR} — the notebook wrote no bytes"

commits = sorted(n for n in names if "/_delta_log/" in n and n.endswith(".json"))
parquet = sorted(n for n in names if n.endswith(".parquet"))
assert commits, f"no Delta commit log under {TABLE_DIR}: {names}"
assert parquet, f"no Parquet files under {TABLE_DIR}: {names}"

# The commit log is the table's own account of what was written. Parse the
# `add` actions rather than trusting the directory listing: a stray .parquet on
# disk is not part of the table, and a table is what the log says it is.
added, claimed_rows = {}, 0
for line in onelake_get(commits[0]).decode().splitlines():
    if not line.strip():
        continue
    action = json.loads(line)
    if "add" not in action:
        continue
    a = action["add"]
    added[a["path"].split("/")[-1]] = a["size"]
    # stats is a JSON string inside the JSON — Delta's own encoding.
    claimed_rows += json.loads(a.get("stats") or "{}").get("numRecords", 0)

assert added, f"the commit log records no added files: {commits[0]}"
assert claimed_rows == 3, f"commit log claims {claimed_rows} rows, notebook wrote 3"

for name, size in added.items():
    blob = onelake_get(f"{TABLE_DIR}/{name}")
    # PAR1 at both ends. The header alone is what a zero-length or truncated
    # write still satisfies; the footer is what proves the file was finished.
    assert blob[:4] == b"PAR1" and blob[-4:] == b"PAR1", (
        f"{name} is not a complete Parquet file: "
        f"head={blob[:4]!r} tail={blob[-4:]!r} len={len(blob)}")
    assert len(blob) == size, (
        f"{name} is {len(blob)} bytes in OneLake but the Delta commit "
        f"recorded {size} — the two records of one write disagree")

log(f"OneLake: {len(added)} Parquet file(s), {sum(added.values())} bytes, "
    f"{claimed_rows} rows, Delta commit {commits[0].split('/')[-1]}")

# Spark Job Definition: publish, resolve, execute on the same selected engine,
# and report the real outcome through its independent job lifecycle.
sjd_source = "result = spark.table('events').count() + int(sys.argv[2])\nprint(f'sjd-result={result}')\n"
sjd_config = {
    "executableFile": "main.py", "arguments": ["--increment", "2"],
    "defaultLakehouseArtifactId": lake["id"],
    "defaultLakehouseWorkspaceId": ws, "environmentArtifactId": env,
}
sjd = publish_item("aggregate-job", "SparkJobDefinition", [
    inline_part("SparkJobDefinitionV1.json", json.dumps(sjd_config)),
    inline_part("main.py", sjd_source),
])
_, headers, _ = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{sjd}/jobs/instances?jobType=sparkjob", token=ft)
sjd_jid = headers["Location"].rstrip("/").rsplit("/", 1)[-1]
sjd_run = req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{sjd}/jobs/instances/{sjd_jid}/sparkJobRun", token=ft)[2]
assert sjd_run["binding"]["lakehouseId"] == lake["id"] and sjd_run["job"]["mainFile"] == "main.py"
old_argv = sys.argv
buf = io.StringIO()
try:
    sys.argv = [sjd_run["job"]["mainFile"], *sjd_run["job"]["arguments"]]
    with redirect_stdout(buf):
        exec(compile(sjd_run["job"]["source"], "<spark-job>", "exec"), {"spark": spark, "sys": sys})
finally:
    sys.argv = old_argv
assert "sjd-result=5" in buf.getvalue(), buf.getvalue()
req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{sjd}/jobs/instances/{sjd_jid}/sparkJobRunResult",
    {"status": "Completed", "output": buf.getvalue()}, token=ft)
assert req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{sjd}/jobs/instances/{sjd_jid}", token=ft)[2]["status"] == "Completed"

# A JAR-bearing Environment is an explicit JVM requirement. Sail rejects it;
# the JVM oracle advertises the JVM surface used to load such dependencies.
jar_env = publish_item("jar-runtime", "Environment", [inline_part("Libraries/probe.jar", "not-a-real-jar")])
jar_config = dict(sjd_config, environmentArtifactId=jar_env)
jar_sjd = publish_item("jar-job", "SparkJobDefinition", [
    inline_part("SparkJobDefinitionV1.json", json.dumps(jar_config)),
    inline_part("main.py", "print('jvm-only')\n"),
])
_, headers, _ = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{jar_sjd}/jobs/instances?jobType=sparkjob", token=ft)
jar_jid = headers["Location"].rstrip("/").rsplit("/", 1)[-1]
jar_run = req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{jar_sjd}/jobs/instances/{jar_jid}/sparkJobRun", token=ft)[2]
assert jar_run["environment"]["jars"] == ["Libraries/probe.jar"], jar_run
if engine == "sail":
    assert not hasattr(spark, "sparkContext"), "Sail unexpectedly exposed SparkContext"
    req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{jar_sjd}/jobs/instances/{jar_jid}/sparkJobRunResult",
        {"status": "Failed", "error": "JAR libraries require the JVM Spark runtime"}, token=ft)
    assert req("GET", f"{FABRIC}/v1/workspaces/{ws}/items/{jar_sjd}/jobs/instances/{jar_jid}", token=ft)[2]["status"] == "Failed"
else:
    assert spark.sparkContext._jvm is not None, "JVM Spark did not expose _jvm"
    req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{jar_sjd}/jobs/instances/{jar_jid}/sparkJobRunResult",
        {"status": "Completed", "output": "JVM dependency surface available"}, token=ft)

spark.stop()
log(f"delta table in OneLake: {rows}")
print(f"NOTEBOOK + SJD + ENVIRONMENT E2E ({engine}): PASS", flush=True)
sys.exit(0)
