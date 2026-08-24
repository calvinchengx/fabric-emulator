"""One notebook, executed twice: by a real Jupyter kernel, and by the emulator.

WHAT THIS IS FOR, AND WHAT IT IS NOT. `make up-jupyter` ships a real JupyterLab
attached to the family (docs/44), and until now nothing executed it in CI: the
only mention of the profile in any workflow was a comment. An editor that has
never been run is a claim, not a witness.

It is NOT a probe of Fabric's execution model. The Jupyter kernel is IPython;
Fabric's notebook runtime is Microsoft's, and this emulator's is the Go cell
parser plus the spark agent. `%%configure`, `%run` and the parameters cell mean
different things under IPython than under either, so measuring them here would
produce real numbers describing a system nobody ships. Axis C belongs to
`e2e/notebook-run` and `e2e/notebook-driven`, which drive the parser and the
agent. docs/56 says which axis is whose.

What only this harness can prove is the pair of things below.

  1. THE AUTHORING ROUND TRIP. A notebook authored as `.ipynb` — the format the
     editor writes and `notebookutils.notebook.create` takes — is published,
     the emulator derives the executable `notebook-content.py`, and the job
     runs it. That derivation has a Go unit test and had no end-to-end path.

  2. THE CROSS-HARNESS DIFFERENTIAL, which is the interesting one. The same
     notebook runs under the kernel and under the agent, and the per-cell
     output must agree. Neither harness alone can see the class of bug where
     the shim depends on the environment the agent provides: process-wide state
     that behaves under one and corrupts under the other. That class is not
     hypothetical here — `sys.argv` and `os.environ` were process-wide in the
     agent while same-wave tasks ran as concurrent goroutines.

HONEST LIMIT, stated because a green suite will be read as more than it is:
both harnesses are ours. Agreement between them is consistency, not fidelity to
Fabric. The only oracle for that is a tenant (`real-fabric.yml`).

The cells are written to be harness-independent on purpose. They address
OneLake by explicit `abfss://` path rather than through a default lakehouse
binding, because that binding legitimately differs between a kernel and a job
and this suite is not the place that tests it (`e2e/notebook-driven` is). And
cell 1 prints the KEYS of `runtime.context`, never the values: a kernel and a
job know different things about which notebook is running, so comparing values
would encode a false expectation.
"""
import base64
import json
import os
import sys
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


def log(message):
    print(f"==> {message}", flush=True)


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
    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    # Timed, like every wait in these suites: an untimed urlopen is how a
    # bounded poll loop still hangs for a whole CI job.
    with urllib.request.urlopen(request, timeout=60) as response:
        raw = response.read()
        return response.status, response.headers, (json.loads(raw) if raw else {})


try:
    req("POST", f"{ENTRA}/admin/api/apps", {
        "displayName": "Azure Storage", "appIdUri": "https://storage.azure.com",
        "isConfidential": False})
except urllib.error.HTTPError as exc:
    if exc.code != 409:
        raise

token = req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials", "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET,
    "scope": "https://api.fabric.microsoft.com/.default"}, form=True)[2]["access_token"]

ws = req("POST", f"{FABRIC}/v1/workspaces", {"displayName": "jupyter-ws"}, token=token)[2]["id"]
lake = req("POST", f"{FABRIC}/v1/workspaces/{ws}/lakehouses",
           {"displayName": "lake"}, token=token)[2]
log(f"workspace {ws}, lakehouse {lake['id']}")

BASE = f"abfss://{ws}@{ACCT}/{lake['displayName']}.Lakehouse"
TABLE = f"{BASE}/Tables/jupyter_events"
FILE = f"{BASE}/Files/jupyter-note.txt"

# Three cells, each printing one line this driver compares between harnesses.
# Every value is fixed or derived from fixed input; nothing here reads a clock,
# a hostname or a path that differs between a kernel and an agent.
CELLS = [
    """\
import notebookutils
# A NAMED SUBSET, not the whole key set. A kernel and a job legitimately know
# different things about which notebook is running -- the job has an item, the
# kernel has an environment -- so comparing every key would encode a false
# expectation and fail for a correct reason. These two are the contract both
# harnesses owe: the shim knows which workspace and which lakehouse it is in.
ctx = notebookutils.runtime.context
print("CTX", [k for k in ("currentWorkspaceId", "defaultLakehouseId") if k in ctx])
""",
    f"""\
from pyspark.sql import SparkSession
spark = SparkSession.builder.getOrCreate()
rows = [(1, "a"), (2, "b"), (3, "c"), (4, "d")]
df = spark.createDataFrame(rows, ["id", "name"])
df.write.format("delta").mode("overwrite").save("{TABLE}")
back = spark.read.format("delta").load("{TABLE}")
print("SPARK", back.count(), sorted(r["name"] for r in back.collect()))
""",
    f"""\
import notebookutils
notebookutils.fs.put("{FILE}", "written-by-the-notebook", True)
print("ONELAKE", notebookutils.fs.head("{FILE}", 1024))
""",
]

# Built with nbformat rather than by hand, so what this publishes is a real
# .ipynb and not our idea of one. `source` must be a STRING: the schema permits
# a list of lines and nbclient assumes the string, which is a difference a
# hand-built dict finds the hard way (it did).
import nbformat  # noqa: E402

NOTEBOOK = nbformat.v4.new_notebook(
    cells=[nbformat.v4.new_code_cell(source) for source in CELLS],
    metadata={"kernelspec": {"display_name": "Python 3", "language": "python",
                             "name": "python3"},
              "language_info": {"name": "python"}})
IPYNB_BYTES = nbformat.writes(NOTEBOOK).encode()


def marked(text):
    """The marker lines a cell printed, so incidental chatter cannot drift.

    Spark and the shim both log, and a comparison over whole stdout would fail
    on a warning that means nothing. Comparing named lines keeps the assertion
    on what the cell computed.
    """
    return [line.strip() for line in text.splitlines()
            if line.startswith(("CTX ", "SPARK ", "ONELAKE "))]


# --- 1. the kernel -----------------------------------------------------------
#
# nbclient drives a REAL ipykernel over the Jupyter protocol, in this image,
# which has the shim on PYTHONPATH and SPARK_REMOTE pointing at Sail — exactly
# what `make up-jupyter` gives a person.
from nbclient import NotebookClient  # noqa: E402

os.environ.setdefault("NOTEBOOKUTILS_WORKSPACE_ID", ws)
notebook = nbformat.reads(nbformat.writes(NOTEBOOK), as_version=4)
log("executing the notebook in a real Jupyter kernel")
NotebookClient(notebook, timeout=600, kernel_name="python3",
               allow_errors=False).execute()

kernel_output = []
for index, cell in enumerate(notebook.cells):
    text = "".join(out.get("text", "") for out in cell.get("outputs", [])
                   if out.get("output_type") == "stream")
    lines = marked(text)
    assert lines, f"kernel cell {index} printed no marker line; got {text!r}"
    kernel_output.append(lines)
    log(f"  kernel cell {index}: {lines}")

# --- 2. the same notebook, published and run by the emulator -----------------

_, headers, _ = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items", {
    "displayName": "authored-in-jupyter", "type": "Notebook",
    "definition": {"parts": [{
        "path": "notebook-content.ipynb", "payloadType": "InlineBase64",
        "payload": base64.b64encode(IPYNB_BYTES).decode()}]}},
    token=token)
operation = headers.get("x-ms-operation-id")
item = None
for _ in range(60):
    body = req("GET", f"{FABRIC}/v1/operations/{operation}", token=token)[2]
    if body.get("status") == "Succeeded":
        item = req("GET", f"{FABRIC}/v1/operations/{operation}/result", token=token)[2]["id"]
        break
    time.sleep(1)
assert item, "the notebook item never finished creating"

# THE DERIVATION, asserted before anything runs. An .ipynb create that stores
# happily and produces nothing executable used to fail one API call later, as a
# job that died on a missing part.
definition = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items/{item}/getDefinition",
                 token=token)[2]
paths = sorted(part["path"] for part in definition["definition"]["parts"])
assert paths == ["notebook-content.ipynb", "notebook-content.py"], paths
derived = base64.b64decode(
    next(p for p in definition["definition"]["parts"]
         if p["path"] == "notebook-content.py")["payload"]).decode()
assert "CTX" in derived and "SPARK" in derived, derived[:400]
log(f"the .ipynb was kept and notebook-content.py derived from it ({len(derived)} bytes)")

_, headers, _ = req(
    "POST", f"{FABRIC}/v1/workspaces/{ws}/items/{item}/jobs/instances?jobType=RunNotebook",
    token=token)
job = headers["Location"].rstrip("/").rsplit("/", 1)[-1]
instance = f"{FABRIC}/v1/workspaces/{ws}/items/{item}/jobs/instances/{job}"
status = None
for _ in range(300):
    status = req("GET", instance, token=token)[2].get("status")
    if status in ("Completed", "Failed", "Cancelled", "Deduped"):
        break
    time.sleep(1)
run = req("GET", f"{instance}/notebookRun", token=token)[2]
cells = sorted(run.get("cells", []), key=lambda c: c["index"])
if status != "Completed":
    for cell in cells:
        log(f"  cell {cell['index']} {cell['status']}: "
            f"{cell.get('error') or cell.get('output', '')[:300]}")
    raise AssertionError(f"job status = {status}")
log(f"the emulator ran the same notebook through the agent: {status}")

job_output = []
for cell in cells:
    lines = marked(cell.get("output", ""))
    assert lines, f"job cell {cell['index']} printed no marker line; got {cell.get('output')!r}"
    job_output.append(lines)
    log(f"  job cell {cell['index']}: {lines}")

# --- 3. the differential -----------------------------------------------------

assert len(kernel_output) == len(job_output), (kernel_output, job_output)
for index, (from_kernel, from_job) in enumerate(zip(kernel_output, job_output)):
    if from_kernel != from_job:
        raise AssertionError(
            f"cell {index} disagrees between the harnesses.\n"
            f"  kernel: {from_kernel}\n"
            f"  job   : {from_job}\n"
            "Both are ours, so this is an inconsistency in the shim or in what "
            "the two execution paths give it — not evidence about Fabric.")
log(f"all {len(kernel_output)} cells agree between the Jupyter kernel and the agent")

# The Spark cell's own claim, checked rather than assumed: agreement between
# two harnesses that both printed nothing useful would also be agreement.
assert any("SPARK 4" in line for line in kernel_output[1]), kernel_output[1]
assert any("written-by-the-notebook" in line for line in kernel_output[2]), kernel_output[2]

log("PASS")
sys.exit(0)
