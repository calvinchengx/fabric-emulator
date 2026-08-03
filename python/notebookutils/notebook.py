"""notebookutils.notebook — run other notebooks and exit with a value.

`run` submits an on-demand RunNotebook job through the Fabric control plane and
polls to a terminal state. Actual cell execution happens on the Spark sidecar
(via Livy) when one is attached.

A NOTEBOOK WITH CELLS NEEDS AN ENGINE TO REACH A TERMINAL STATE. The emulator no
longer completes such a job on its clock — that made `run` return "Completed"
for a notebook whose cells never executed, which is the one answer this function
must never give wrongly. With no engine attached, `run` now raises
NotebookError on `timeoutSeconds`, and that timeout is the truth: nothing ran.
A notebook with no executable cells still completes immediately, because there
is nothing to wait for.
"""
import time

from ._config import config
from ._http import request, HttpError
from . import credentials, runtime

_TERMINAL = {"Completed", "Failed", "Cancelled", "Deduped"}


class NotebookError(Exception):
    pass


def _resolve_item(name, workspaceId, token):
    """Find the notebook item id by displayName within the workspace."""
    ws = workspaceId or config().workspace_id
    resp = request("GET", f"{config().fabric_url}/v1/workspaces/{ws}/items?type=Notebook", token=token)
    for it in resp.get("value", []):
        if it.get("displayName") == name:
            return ws, it["id"]
    raise NotebookError(f"notebook {name!r} not found in workspace {ws}")


def run(name, timeoutSeconds=90, arguments=None, workspaceId=None):
    """Run notebook `name` and return its terminal job status."""
    token = credentials.getToken("fabric")
    ws, iid = _resolve_item(name, workspaceId, token)
    status, hdrs, _ = request(
        "POST", f"{config().fabric_url}/v1/workspaces/{ws}/items/{iid}/jobs/instances?jobType=RunNotebook",
        token=token, raw=True)
    loc = hdrs.get("Location")
    jid = loc.rstrip("/").rsplit("/", 1)[-1]
    deadline = time.time() + timeoutSeconds
    while time.time() < deadline:
        job = request("GET", f"{config().fabric_url}/v1/workspaces/{ws}/items/{iid}/jobs/instances/{jid}", token=token)
        st = job.get("status")
        if st in _TERMINAL:
            if st == "Failed":
                raise NotebookError(f"notebook {name!r} failed: {job.get('failureReason')}")
            return st
        time.sleep(0.3)
    raise NotebookError(f"notebook {name!r} did not finish within {timeoutSeconds}s")


class _Exit(Exception):
    def __init__(self, value):
        self.value = value


def exit(value=""):
    """Signal the notebook's exit value (as notebookutils.notebook.exit does)."""
    raise _Exit(value)
