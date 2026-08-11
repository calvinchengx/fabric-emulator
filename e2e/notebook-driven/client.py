"""A Fabric notebook run with nothing but the published stack.

This client PUBLISHES a notebook, SUBMITS a RunNotebook job, and POLLS. That is
all. It holds no Spark session, executes no cell and posts no result — so if the
job reaches Completed, something else ran it, and the only candidate in this
compose file is the emulator driving the spark-agent.

The contrast with e2e/notebook-run is the whole test. There a runner script
plays the Spark pool; that script ships in no artifact, so the path it proves
was one a consumer could not walk. Here the stack is exactly what
`docker compose pull` gives you.

Three assertions, weakest to strongest:
  1. the job reaches a terminal state at all — it used to hang forever
  2. the run detail says every cell Succeeded and carries the exit value
  3. the Delta table exists IN ONELAKE, read over plain HTTP by this process,
     which is not Spark and never touched the data
"""
import base64
import json
import time
import urllib.error
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
ACCT = "onelake.dfs.fabric.microsoft.com"

# A notebook that writes, reads back through SQL, and exits with a value.
# `mssparkutils.notebook.exit` IS the Fabric contract: the run stops there and
# the value reaches the caller through the job.
#
# This comment used to name the bare `notebook_exit`, which was never a Fabric
# global — the emulator's prelude injected it (#192) and this file said so in
# writing. docs/12 calls this harness "the path a consumer with no clone can
# walk", so it is the copy-paste template for exactly the people the injected
# name would strand on a tenant. A false claim in a template propagates further
# than the same claim in a library.
NOTEBOOK_BODY = '''# Fabric notebook source

# CELL ********************
df = spark.createDataFrame([(1, "a"), (2, "b"), (3, "c"), (4, "d")], ["id", "name"])
df.write.format("delta").mode("overwrite").saveAsTable("events")
print("wrote", df.count(), "rows")

# MARKDOWN ********************
# MAGIC %md
# MAGIC ## Count them back

# CELL ********************
# MAGIC %%sql
# MAGIC SELECT count(*) AS n FROM events

# CELL ********************
# Exits with JSON, deliberately. A notebook returning str(count) cannot detect
# a mangled exit value — "4" survives almost any quoting mistake, and one such
# mistake shipped: the driver read the value back with repr(), the agent repr'd
# it AGAIN, and anything containing a quote came back as \'{"a": 1}\'. The first
# real notebook to return a JSON summary found it immediately, so the fixture
# now returns the shape real notebooks return.
import json as _json
mssparkutils.notebook.exit(_json.dumps({"rows": spark.table("events").count(), "table": "events"}))

# CELL ********************
raise AssertionError("a cell after the notebook exit must never run")
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
    # Timed, like every other wait in this suite: an untimed urlopen is the one
    # way a bounded poll loop still hangs for a whole CI job (see e2e/sail).
    with urllib.request.urlopen(r, timeout=60) as resp:
        raw = resp.read()
        return resp.status, resp.headers, (json.loads(raw) if raw else {})


def log(m):
    print(f"==> {m}", flush=True)


try:
    req("POST", f"{ENTRA}/admin/api/apps", {
        "displayName": "Azure Storage", "appIdUri": "https://storage.azure.com",
        "isConfidential": False})
except urllib.error.HTTPError as e:
    if e.code != 409:
        raise

ft = req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials", "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET,
    "scope": "https://api.fabric.microsoft.com/.default"}, form=True)[2]["access_token"]

ws = req("POST", f"{FABRIC}/v1/workspaces", {"displayName": "nb-ws"}, token=ft)[2]["id"]
lake = req("POST", f"{FABRIC}/v1/workspaces/{ws}/lakehouses",
           {"displayName": "lake"}, token=ft)[2]
log(f"workspace {ws}, lakehouse {lake['id']}")

# The default lakehouse, declared the way real Fabric declares it — in the
# notebook's own metadata. It is what makes `saveAsTable("events")` land in this
# lakehouse rather than nowhere.
metadata = {
    "kernel_info": {"name": "synapse_pyspark"},
    "dependencies": {"lakehouse": {
        "default_lakehouse": lake["id"],
        "default_lakehouse_name": lake["displayName"],
        "default_lakehouse_workspace_id": ws}},
}
meta = "# METADATA ********************\n" + "\n".join(
    "# META " + line for line in json.dumps(metadata, indent=2).splitlines()) + "\n"

_, headers, _ = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items", {
    "displayName": "etl-nb", "type": "Notebook",
    "definition": {"parts": [{
        "path": "notebook-content.py", "payloadType": "InlineBase64",
        "payload": base64.b64encode((NOTEBOOK_BODY + meta).encode()).decode()}]}},
    token=ft)
opid = headers.get("x-ms-operation-id")
nb = None
for _ in range(60):
    body = req("GET", f"{FABRIC}/v1/operations/{opid}", token=ft)[2]
    if body.get("status") == "Succeeded":
        nb = req("GET", f"{FABRIC}/v1/operations/{opid}/result", token=ft)[2]["id"]
        break
    time.sleep(1)
assert nb, "the notebook item never finished creating"
log(f"notebook {nb}")

_, hdrs, _ = req(
    "POST", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances?jobType=RunNotebook",
    token=ft)
jid = hdrs["Location"].rstrip("/").rsplit("/", 1)[-1]
log(f"submitted RunNotebook job {jid} — and now this client does nothing")

# 1. TERMINAL AT ALL. Gate on the status, never a sleep: the run takes as long
#    as Spark takes, and a fixed wait either truncates it or pads the test.
base = f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}"
status = None
for _ in range(180):
    status = req("GET", base, token=ft)[2].get("status")
    if status in ("Completed", "Failed", "Cancelled", "Deduped"):
        break
    time.sleep(1)
if status != "Completed":
    # The run detail is the only thing that says WHICH cell died and why. A bare
    # "job status = Failed" sends the reader to the wrong logs.
    detail = req("GET", f"{base}/notebookRun", token=ft)[2]
    for c in sorted(detail.get("cells", []), key=lambda c: c["index"]):
        log(f"cell {c['index']} {c['status']}: {c.get('error') or c.get('output', '')[:300]}")
    raise AssertionError(f"job status = {status}")
log(f"job reached {status} with no runner in the stack")

# 2. WHAT EACH CELL DID. A terminal status says execution was reported; the run
#    detail says what happened, which is the stronger claim.
run = req("GET", f"{base}/notebookRun", token=ft)[2]
cells = sorted(run["cells"], key=lambda c: c["index"])
log(f"cells: {[(c['index'], c['status']) for c in cells]}, exit={run.get('exitValue')!r}")
exit_value = json.loads(run["exitValue"])  # must be JSON, not a mangled repr
assert exit_value == {"rows": 4, "table": "events"}, run["exitValue"]
assert [c["language"] for c in cells] == ["python", "sql", "python", "python"], cells
for c in cells[:3]:
    assert c["status"] == "Succeeded", c
# The cell after the exit must be untouched. If it had run it would have
# raised, so a Completed job is not on its own proof that it did not.
assert cells[3]["status"] == "Pending", f"execution continued past the exit: {cells[3]}"

# 2b. THE SHIM RETURNS IT. The assertion above proves the exit value reached the
#     REST surface; it says nothing about what a notebook author actually gets
#     back. `notebookutils.notebook.run` returned the job STATUS until 0.18.0,
#     so every check above passed while `run()` handed the caller "Completed".
#     This closes the loop engine -> service -> shim -> caller, which is the
#     only path a user's code takes.
import os  # noqa: E402 — the shim needs the env below set first
import sys  # noqa: E402

sys.path.insert(0, "/app/python")
os.environ.update({
    "NOTEBOOKUTILS_FABRIC_URL": FABRIC,
    "NOTEBOOKUTILS_ENTRA_URL": ENTRA,
    "NOTEBOOKUTILS_TENANT": TENANT,
    "NOTEBOOKUTILS_CLIENT_ID": CLIENT_ID,
    "NOTEBOOKUTILS_CLIENT_SECRET": CLIENT_SECRET,
    "NOTEBOOKUTILS_WORKSPACE_ID": ws,
    # Deliberately the SAME lakehouse the notebook is bound to. That makes this
    # a reference run under Fabric's lakehouse rule, so it also proves the guard
    # does not fire on a matching binding — a guard that refused everything
    # would pass a test that only checks the blocked case.
    "NOTEBOOKUTILS_LAKEHOUSE_ID": lake["id"],
})
from notebookutils import notebook as nbu  # noqa: E402

shim_exit = nbu.run("etl-nb")
log(f"notebookutils.notebook.run returned {shim_exit!r}")
assert json.loads(shim_exit) == {"rows": 4, "table": "events"}, shim_exit

# And through the orchestration primitive, where the value lands in `exitVal`.
dag = nbu.runMultiple({"activities": [{"name": "load", "path": "etl-nb"}]})
log(f"runMultiple exitVal = {dag['load']['exitVal']!r}")
assert json.loads(dag["load"]["exitVal"]) == {"rows": 4, "table": "events"}, dag
assert dag["load"]["exception"] is None, dag

# 3. THE BYTES. Read the Delta log over plain HTTP — this process is not Spark
#    and has touched none of the data, so what it finds is what actually landed.
sft = req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials", "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET,
    "scope": "https://storage.azure.com/.default"}, form=True)[2]["access_token"]
TABLE_DIR = "lake.Lakehouse/Tables/events"
listing = req("GET", f"http://{ACCT}/{ws}?resource=filesystem&recursive=true"
                     f"&directory={urllib.parse.quote(TABLE_DIR)}", token=sft)[2]
names = [p["name"] for p in listing.get("paths", [])]
assert names, f"OneLake has nothing under {TABLE_DIR} — the notebook wrote no bytes"

# List, then read the log the listing NAMES. Guessing the commit filename made
# this 404 against a table that was perfectly well written.
commits = sorted(n for n in names if "/_delta_log/" in n and n.endswith(".json"))
assert commits, f"no Delta commit log under {TABLE_DIR}: {names}"
assert any(n.endswith(".parquet") for n in names), f"no Parquet under {TABLE_DIR}: {names}"

r = urllib.request.Request(f"http://{ACCT}/{ws}/{urllib.parse.quote(commits[0])}",
                           headers={"Authorization": "Bearer " + sft})
with urllib.request.urlopen(r, timeout=60) as resp:
    commit = resp.read().decode()
# The log is the table's own account of what was written; a stray .parquet on
# disk is not part of the table.
adds = [json.loads(x)["add"] for x in commit.splitlines() if x.strip() and "add" in json.loads(x)]
assert adds, commit[:400]
log(f"OneLake holds {len(adds)} Delta add action(s), read by a client that is not Spark")

print("NOTEBOOK-DRIVEN E2E: PASS", flush=True)
