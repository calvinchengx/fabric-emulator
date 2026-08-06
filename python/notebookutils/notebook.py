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
from concurrent.futures import ThreadPoolExecutor

from . import credentials
from ._config import config
from ._http import request
from .common.exceptions import RunMultipleFailedException

_TERMINAL = {"Completed", "Failed", "Cancelled", "Deduped"}

# Fabric's documented defaults (notebookutils-notebook-run).
_DEFAULT_TIMEOUT_PER_CELL = 90
_DEFAULT_DAG_TIMEOUT = 43200  # 12 hours


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


def run(path, timeout_seconds=_DEFAULT_TIMEOUT_PER_CELL, arguments=None, workspace=None,
        spark_environment=None, attach_lakehouse=None, **kwargs):
    """Run notebook `path` and return ITS EXIT VALUE.

    The return value is the exact string the child passed to
    `notebookutils.notebook.exit(value)`, or `""` when the child never called
    it — Fabric's documented contract, and not what this returned until now.
    It returned the terminal job status ("Completed"), so a parent branching on
    `run(...)` took the wrong branch against the emulator and the right one
    against Fabric. Callers wanting the status can read it from the job
    instance; only one of the two can be the return value, and Fabric has
    already chosen.

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

    `timeoutSeconds` and `workspaceId` remain accepted as aliases: they were
    this function's parameter names before Fabric's own spelling was checked,
    and code in this repository passes them by keyword.
    """
    timeout_seconds = kwargs.pop("timeoutSeconds", None) or timeout_seconds
    workspace = kwargs.pop("workspaceId", None) or workspace
    exit_value, _status, _cells = _run_detail(
        path, timeout_seconds=timeout_seconds, arguments=arguments, workspace=workspace)
    return exit_value


def _run_detail(path, timeout_seconds=None, per_cell_seconds=None, arguments=None,
                workspace=None):
    """Run a notebook and return `(exit_value, status, cell_count)`.

    One request path, two views: `run` projects the exit value, `runMultiple`
    reads the whole tuple. Splitting it here keeps a single definition of what
    running a notebook means.

    The two timeouts are different units and both are Fabric's. `run` takes
    `timeout_seconds`, a whole-notebook deadline. A `runMultiple` activity
    takes `per_cell_seconds` — `timeoutPerCellInSeconds` — which becomes a
    notebook deadline only once multiplied by the cell count. That count is
    exact rather than assumed: creating the job parses the notebook, so
    `…/notebookRun` can be read for it before the run finishes.
    """
    token = credentials.getToken("fabric")
    ws, iid = _resolve_item(path, workspace, token)
    body = _execution_data(arguments)
    _status, hdrs, _ = request(
        "POST", f"{config().fabric_url}/v1/workspaces/{ws}/items/{iid}/jobs/instances?jobType=RunNotebook",
        token=token, body=body, raw=True)
    loc = hdrs.get("Location")
    jid = loc.rstrip("/").rsplit("/", 1)[-1]
    base = f"{config().fabric_url}/v1/workspaces/{ws}/items/{iid}/jobs/instances/{jid}"
    if timeout_seconds is None:
        timeout_seconds = (per_cell_seconds or _DEFAULT_TIMEOUT_PER_CELL) * _cell_count(base, token)
    deadline = time.time() + timeout_seconds
    while time.time() < deadline:
        job = request("GET", base, token=token)
        st = job.get("status")
        if st in _TERMINAL:
            if st == "Failed":
                raise NotebookError(f"notebook {path!r} failed: {job.get('failureReason')}")
            return _finish(base, token, st)
        time.sleep(0.3)
    raise NotebookError(f"notebook {path!r} did not finish within {timeout_seconds}s")


def _execution_data(arguments):
    """Build the child run's `executionData`, including the reference-run context.

    `parentLakehouseId` is what makes this a REFERENCE run rather than a bare
    job submission, and it is what lets the service apply Fabric's rule: a
    child bound to a different lakehouse than its parent is blocked unless
    `useRootDefaultLakehouse` says otherwise. Sending nothing meant the rule
    could never fire, so a DAG with a mis-bound child passed here and was
    refused in production.

    `useRootDefaultLakehouse` travels in `arguments`, which is where Fabric's
    reference puts it. It is lifted out rather than forwarded as a notebook
    parameter: it configures the run, and passing it through to the child's
    parameter cell would set a variable the notebook never declared.
    """
    args = dict(arguments or {})
    bypass = bool(args.pop("useRootDefaultLakehouse", False))
    exec_data = {}
    if args:
        exec_data["parameters"] = {
            k: {"value": v, "type": "string"} for k, v in args.items()}
    parent = config().lakehouse_id
    if parent:
        exec_data["parentLakehouseId"] = parent
    if bypass:
        exec_data["useRootDefaultLakehouse"] = True
    return {"executionData": exec_data} if exec_data else None


def _cell_count(base, token):
    """How many cells this run will execute; at least 1.

    Readable as soon as the job exists, because creating it parses the
    notebook's definition into the run record. A floor of 1 keeps a notebook
    with no executable cells — or a detail that cannot be read — from being
    handed a zero-second deadline, which would fail instantly.
    """
    try:
        detail = request("GET", f"{base}/notebookRun", token=token)
    except Exception:  # noqa: BLE001 — an unreadable detail must not fail the run
        return 1
    return max(len(detail.get("cells") or []), 1)


def _finish(base, token, status):
    """Read the run detail a terminal job left behind.

    The exit value has always been recorded — the engine posts it, the service
    stores it, and `…/notebookRun` serves it. Nothing asked for it, which is
    why `run` could only report a status. A run detail that cannot be read is
    not fatal: a notebook with no cells has nothing to report and still
    completed.
    """
    try:
        detail = request("GET", f"{base}/notebookRun", token=token)
    except Exception:  # noqa: BLE001 — an unreadable detail must not fail a completed run
        return "", status, 0
    return detail.get("exitValue") or "", status, len(detail.get("cells") or [])


def validateDAG(dag):
    """Return True if `dag` is structurally valid, else raise.

    Fabric exposes this so a production workflow can fail on a malformed DAG
    before any notebook runs. The checks are the ones `runMultiple` performs
    anyway — duplicate names, dependencies naming an activity outside the DAG,
    cycles — so this is the same code reached earlier, not a second opinion
    that could drift from it.
    """
    activities = _normalise_dag(dag)
    _check_dependencies(activities)
    _in_dependency_order(activities)
    return True


def runMultiple(dag, config=None, **kwargs):
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
        ], "timeoutInSeconds": 43200, "concurrency": 12})

    Returns `{name: {"exitVal": str, "exception": err or None}}` keyed by
    activity name — Fabric's two documented keys. `status` and `error` ride
    alongside as emulator extras, because a caller debugging a DAG locally
    wants to distinguish "did not run" from "ran and failed" and Fabric's shape
    cannot express it; nothing in this repository may depend on them.

    RAISES `RunMultipleFailedException` when any activity did not complete,
    carrying the full result dict on `.result`. That is Fabric's contract and
    the documented way to read partial results:

        try:
            results = notebookutils.notebook.runMultiple(DAG)
        except RunMultipleFailedException as ex:
            results = ex.result

    Returning quietly instead — which this did — means code written to that
    pattern never enters its except branch locally and never learns a notebook
    failed.

    SEQUENTIAL BY DEFAULT, in dependency order — a CHOSEN divergence, recorded
    in docs/parity.md. Fabric defaults `concurrency` to 3x the CPU count; a
    harness comparing two runs needs the same sequence more than it needs
    speed, so an unspecified `concurrency` runs one notebook at a time here.

    An EXPLICIT `concurrency` is honoured, because the reason to want it is not
    speed: sequential execution hides a real class of bug. Two independent
    activities that write the same table collide on Fabric and never collide
    here, so the emulator turns a genuine defect into a green run. `0` means
    unlimited, as Fabric documents.

    A failed activity stops its dependents rather than the whole DAG: they are
    reported with an exception saying which dependency did not complete,
    because "did not run because `a` failed" and "ran and failed" are different
    facts and a caller acts on them differently.
    """
    activities = _normalise_dag(dag)
    _check_dependencies(activities)
    dag_timeout = _dag_timeout(dag)
    deadline = time.time() + dag_timeout

    concurrency = _concurrency(dag)
    results, done = {}, {}
    for level in _dependency_levels(activities):
        runnable = []
        for act in level:
            name = act["name"]
            blocked = [d for d in act.get("dependencies", []) if done.get(d) != "Completed"]
            if blocked:
                results[name] = _failed_result(
                    "Skipped", NotebookError(f"dependency {blocked[0]!r} did not complete"))
                done[name] = "Skipped"
                continue
            if time.time() >= deadline:
                # Every activity gets a result. A DAG that timed out halfway and
                # returned a short dict is indistinguishable from one whose
                # remaining activities were never in it.
                results[name] = _failed_result(
                    "Skipped",
                    NotebookError(f"the DAG exceeded timeoutInSeconds ({dag_timeout}s) "
                                  f"before {name!r} started"))
                done[name] = "Skipped"
                continue
            runnable.append(act)
        # strict: one result per runnable activity, or a result has been lost.
        for act, (result, status) in zip(runnable, _run_level(runnable, concurrency), strict=True):
            results[act["name"]], done[act["name"]] = result, status

    failed = [n for n, r in results.items() if r["exception"] is not None]
    if failed:
        raise RunMultipleFailedException(
            f"{len(failed)} of {len(results)} activities did not complete: "
            f"{', '.join(sorted(failed))}",
            result=results)
    return results


def _run_level(activities, concurrency):
    """Run one dependency level, returning results IN THE ORDER GIVEN.

    Execution may be concurrent; the results never are. A caller diffing two
    runs of the same DAG compares the returned dict, and letting completion
    order decide it would make an unchanged DAG look changed.

    `concurrency` of 1 does not touch a thread pool at all. That is not an
    optimisation: the default path stays a plain loop, so the sequential
    contract cannot be broken by a scheduling detail.
    """
    if concurrency == 1 or len(activities) <= 1:
        return [_run_activity(a) for a in activities]
    workers = len(activities) if concurrency is None else min(concurrency, len(activities))
    with ThreadPoolExecutor(max_workers=workers) as pool:
        return list(pool.map(_run_activity, activities))


def _concurrency(dag):
    """How many activities of one level may run at once.

    Returns 1 for sequential (the emulator's default), None for unlimited
    (Fabric's `0`), else the cap. Fabric's own default is 3x the CPU count;
    diverging is deliberate and documented — see `runMultiple`.
    """
    if not isinstance(dag, dict) or dag.get("concurrency") is None:
        return 1
    requested = int(dag["concurrency"])
    return None if requested == 0 else max(1, requested)


def _run_activity(act):
    """Run one activity; return `(result, status)`.

    Never raises: a failure is this activity's result, and the DAG decides what
    that means for the rest. `runMultiple` raises at the end, once, with every
    result in hand.
    """
    name = act["name"]
    attempts = max(int(act.get("retry", 0)), 0) + 1
    interval = max(int(act.get("retryIntervalInSeconds", 0)), 0)
    last = None
    for attempt in range(attempts):
        try:
            exit_value, status, _cells = _run_detail(
                act.get("path", name),
                per_cell_seconds=act.get("timeoutPerCellInSeconds", _DEFAULT_TIMEOUT_PER_CELL),
                arguments=act.get("args"),
                workspace=act.get("workspace"),
            )
            return {"exitVal": exit_value, "exception": None,
                    "status": status, "error": None}, status
        except NotebookError as e:
            # The LAST error is the one reported. Keeping the first would
            # describe an attempt that is no longer why this activity failed,
            # and retries exist precisely because the first one is often
            # transient.
            last = e
            if attempt + 1 < attempts and interval:
                time.sleep(interval)
    return _failed_result("Failed", last), "Failed"


def _failed_result(status, exception):
    """The result shape for an activity that produced no exit value."""
    return {"exitVal": "", "exception": exception,
            "status": status, "error": str(exception)}


def _dag_timeout(dag):
    """The whole-DAG wall clock; Fabric defaults it to 12 hours."""
    if isinstance(dag, dict):
        return dag.get("timeoutInSeconds") or _DEFAULT_DAG_TIMEOUT
    return _DEFAULT_DAG_TIMEOUT


def _normalise_dag(dag):
    """Accept either a bare list of notebook names or Fabric's DAG dict.

    `workspaceId` is folded into `workspace`, Fabric's own field name, which
    takes a workspace name OR an id. Both spellings are accepted because this
    module used the former before Fabric's was checked.

    A duplicate `name` is refused. Activity names key the result dict and are
    what `dependencies` point at, so a repeat silently dropped one activity's
    result and made any dependency on that name ambiguous.
    """
    if isinstance(dag, dict):
        acts = dag.get("activities")
        if acts is None:
            raise NotebookError("a DAG needs an 'activities' list")
        out, seen = [], set()
        for a in acts:
            name = a.get("name")
            if not name:
                raise NotebookError("every activity needs a 'name'")
            if name in seen:
                raise NotebookError(f"duplicate activity name {name!r}: names key the "
                                    "results and are what dependencies refer to")
            seen.add(name)
            act = dict(a)
            if act.get("workspaceId") and not act.get("workspace"):
                act["workspace"] = act["workspaceId"]
            out.append(act)
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


def _dependency_levels(activities):
    """Group activities into levels; everything in a level may run at once.

    Level N depends only on levels before it, which is what makes bounded
    concurrency safe to apply within one. Input order is preserved inside each
    level, so a sequential run is reproducible.

    A cycle raises rather than hanging or silently dropping activities — the
    two failure modes a caller cannot debug from the outside.
    """
    remaining = list(activities)
    emitted, levels = set(), []
    while remaining:
        ready = [a for a in remaining
                 if all(d in emitted for d in a.get("dependencies", []))]
        if not ready:
            stuck = ", ".join(sorted(a["name"] for a in remaining))
            raise NotebookError(f"dependency cycle among: {stuck}")
        levels.append(ready)
        emitted.update(a["name"] for a in ready)
        remaining = [a for a in remaining if a["name"] not in emitted]
    return levels


def _in_dependency_order(activities):
    """The levels, flattened. Derived so the two orderings cannot disagree."""
    return [a for level in _dependency_levels(activities) for a in level]


class _Exit(Exception):
    def __init__(self, value):
        self.value = value


def exit(value=""):
    """Signal the notebook's exit value (as notebookutils.notebook.exit does)."""
    raise _Exit(value)
