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

from . import credentials
from ._config import config
from ._http import request

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


def run(name, timeoutSeconds=90, arguments=None, workspaceId=None,
        spark_environment=None, attach_lakehouse=None, **kwargs):
    """Run notebook `name` and return its terminal job status.

    `arguments` are forwarded as the child run's `executionData.parameters`,
    which is the whole reason a caller passes them: a parameterised notebook
    invoked with none runs on its placeholder defaults and fails a validation
    the caller had in fact satisfied.

    `spark_environment` / `attach_lakehouse` are accepted because real Fabric
    accepts them, and callers inspect this signature to decide whether the
    runtime supports the option before passing it: a framework that introspects
    `run` and finds no such keyword will refuse to invoke a notebook at all.
    The emulator has one Spark session and attaches the notebook's own
    binding, so there is nothing to switch: they are accepted and ignored rather
    than advertised as unsupported.
    """
    token = credentials.getToken("fabric")
    ws, iid = _resolve_item(name, workspaceId, token)
    body = None
    if arguments:
        body = {"executionData": {"parameters": {
            k: {"value": v, "type": "string"} for k, v in arguments.items()}}}
    _status, hdrs, _ = request(
        "POST", f"{config().fabric_url}/v1/workspaces/{ws}/items/{iid}/jobs/instances?jobType=RunNotebook",
        token=token, body=body, raw=True)
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


def runMultiple(dag, useRootDefaultLakehouse=True, **kwargs):
    """Run several notebooks, honouring `dependencies`, and return each result.

    This is the orchestration primitive `run` is not: `run` blocks on one
    notebook, while a DAG expresses "these three, then that one". A pipeline can
    express the same shape, but a notebook that orchestrates notebooks is its
    own Fabric pattern and code that uses it cannot be rewritten as a pipeline
    without changing what it is.

    Two input shapes, both of which real Fabric accepts:

        runMultiple(["nb1", "nb2"])          # no dependencies — run them all
        runMultiple({"activities": [         # a DAG
            {"name": "a", "path": "nbA", "args": {...}},
            {"name": "b", "path": "nbB", "dependencies": ["a"]},
        ], "timeoutInSeconds": 3600, "concurrency": 5})

    Returns `{name: {"exitVal": ..., "message": ...}}` keyed by activity name.

    SEQUENTIAL, in dependency order. `concurrency` is accepted and recorded
    because callers pass it, but the emulator runs one notebook at a time: the
    Spark sidecar is a single session, so running two at once would queue them
    anyway while making the failure attribution worse. Order within a dependency
    level is the order given, which keeps a run reproducible — the property a
    test harness needs more than parallelism.

    A failed activity stops its dependents rather than the whole DAG: they are
    reported `Skipped` with the reason, because "did not run because `a` failed"
    and "ran and failed" are different facts and a caller acts on them
    differently.
    """
    activities = _normalise_dag(dag)
    _check_dependencies(activities)

    results, done = {}, {}
    for act in _in_dependency_order(activities):
        name = act["name"]
        blocked = [d for d in act.get("dependencies", []) if done.get(d) != "Completed"]
        if blocked:
            results[name] = {"exitVal": None, "message": None,
                             "status": "Skipped",
                             "error": f"dependency {blocked[0]!r} did not complete"}
            done[name] = "Skipped"
            continue
        try:
            status = run(
                act.get("path", name),
                timeoutSeconds=act.get("timeoutPerCellInSeconds", 90),
                arguments=act.get("args"),
                workspaceId=act.get("workspaceId"),
            )
            results[name] = {"exitVal": "", "message": None, "status": status, "error": None}
            done[name] = status
        except NotebookError as e:
            results[name] = {"exitVal": None, "message": None,
                             "status": "Failed", "error": str(e)}
            done[name] = "Failed"
    return results


def _normalise_dag(dag):
    """Accept either a bare list of notebook names or Fabric's DAG dict."""
    if isinstance(dag, dict):
        acts = dag.get("activities")
        if acts is None:
            raise NotebookError("a DAG needs an 'activities' list")
        out = []
        for a in acts:
            if not a.get("name"):
                raise NotebookError("every activity needs a 'name'")
            out.append(dict(a))
        return out
    if isinstance(dag, (list, tuple)):
        return [{"name": n, "path": n} for n in dag]
    raise NotebookError("runMultiple takes a list of notebook names or a DAG dict")


def _check_dependencies(activities):
    """Refuse a DAG that names a dependency it does not contain."""
    names = {a["name"] for a in activities}
    for a in activities:
        for d in a.get("dependencies", []):
            if d not in names:
                raise NotebookError(
                    f"activity {a['name']!r} depends on {d!r}, which is not in the DAG")


def _in_dependency_order(activities):
    """Dependencies first, input order preserved within a level.

    A cycle raises rather than hanging or silently dropping activities — the
    two failure modes a caller cannot debug from the outside.
    """
    remaining = list(activities)
    emitted, out = set(), []
    while remaining:
        ready = [a for a in remaining
                 if all(d in emitted for d in a.get("dependencies", []))]
        if not ready:
            stuck = ", ".join(sorted(a["name"] for a in remaining))
            raise NotebookError(f"dependency cycle among: {stuck}")
        for a in ready:
            out.append(a)
            emitted.add(a["name"])
        remaining = [a for a in remaining if a["name"] not in emitted]
    return out


class _Exit(Exception):
    def __init__(self, value):
        self.value = value


def exit(value=""):
    """Signal the notebook's exit value (as notebookutils.notebook.exit does)."""
    raise _Exit(value)
