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
import base64
import json
import time
import warnings
from concurrent.futures import ThreadPoolExecutor

from . import credentials
from ._config import config, session_workspace_id
from ._http import request
from .common.exceptions import RunMultipleFailedException

# `list` BECOMES A NOTEBOOK API IN THIS MODULE (Fabric names it that), which
# shadows the builtin for everything defined after it. Captured here, before the
# shadow exists, so the two internal uses keep meaning the builtin. Measured, not
# guessed: without this, 37 tests fail inside runMultiple's own helpers.
_list = list

_TERMINAL = {"Completed", "Failed", "Cancelled", "Deduped"}

# Fabric's documented defaults (notebookutils-notebook-run).
_DEFAULT_TIMEOUT_PER_CELL = 90

# The whole-notebook ceiling used when the cell count cannot be read — which is
# every run on real Fabric, whose job-instance surface has no run-detail
# endpoint. Deliberately GENEROUS: the two failure modes are not symmetric. Too
# short invents a timeout for a notebook that was working, which is a false
# failure someone has to debug; too long only delays reporting a notebook that
# had genuinely hung, which the caller's own deadline can cut short anyway.
_TIMEOUT_WITHOUT_CELL_COUNT = 1800
_DEFAULT_DAG_TIMEOUT = 43200  # 12 hours


class NotebookError(Exception):
    pass


def _resolve_item(name, workspaceId, token):
    """Find the notebook item id by displayName within the workspace."""
    ws = workspaceId or session_workspace_id(config().workspace_id)
    resp = request("GET", f"{config().fabric_url}/v1/workspaces/{ws}/items?type=Notebook", token=token)
    for it in resp.get("value", []):
        if it.get("displayName") == name:
            return ws, it["id"]
    raise NotebookError(f"notebook {name!r} not found in workspace {ws}")


class Artifact:
    """What the management APIs return: `displayName`, `id`, `description`.

    An object rather than the raw dict, because Microsoft's examples read
    `notebook.displayName` and `notebook.id`. A dict would make every one of
    those lines an AttributeError in a notebook that works on Fabric.
    """

    __slots__ = ("description", "displayName", "id", "type", "workspaceId")

    def __init__(self, raw):
        self.id = raw.get("id", "")
        self.displayName = raw.get("displayName", "")
        self.description = raw.get("description", "")
        self.type = raw.get("type", "")
        self.workspaceId = raw.get("workspaceId", "")

    def __repr__(self):
        return f"Artifact(displayName={self.displayName!r}, id={self.id!r})"

    def __eq__(self, other):
        return isinstance(other, Artifact) and other.id == self.id

    def _asdict(self):
        return {"id": self.id, "displayName": self.displayName,
                "description": self.description, "type": self.type,
                "workspaceId": self.workspaceId}


def _follow(status, headers, payload, *, what, want_result=True):
    """Resolve the 200-or-202 outcome the items API is documented to have.

    BOTH ARE LEGAL and a real tenant answers 202, so a client that reads the
    202 body gets `null` and reports an empty result rather than an error. The
    emulator can be told to always answer 202 (FABRIC_FORCE_LRO) precisely so
    this path is exercised locally instead of only in production.

    `want_result` is False for operations that have no result document — real
    Fabric's updateDefinition is one, and polling `/result` for it would 404 on
    a success.
    """
    if status != 202:
        return json.loads(payload) if payload else {}
    op = headers.get("x-ms-operation-id") or headers.get("X-Ms-Operation-Id")
    location = headers.get("Location") or headers.get("location")
    if not op and not location:
        raise NotebookError(f"{what} returned 202 with no operation to follow")
    op_url = location or f"{config().fabric_url}/v1/operations/{op}"
    deadline = time.monotonic() + 120
    while True:
        state = request("GET", op_url, token=credentials.getToken("pbi"))
        st = state.get("status")
        if st == "Succeeded":
            break
        if st == "Failed":
            raise NotebookError(f"{what} operation failed: {state.get('error')}")
        if time.monotonic() > deadline:
            raise NotebookError(f"{what} operation did not complete")
        time.sleep(0.2)
    if not want_result:
        return {}
    return request("GET", op_url.rstrip("/") + "/result",
                   token=credentials.getToken("pbi"))


def _ws(workspaceId):
    ws = workspaceId or session_workspace_id(config().workspace_id)
    if not ws:
        raise NotebookError(
            "no workspace: pass workspaceId, or run inside a notebook whose "
            "session carries one")
    return ws


def _notebook_parts(content):
    """Definition parts for `.ipynb` content, as Fabric's `create` documents it.

    ONLY THE `.ipynb` IS SENT. The emulator derives the executable
    `notebook-content.py` from it server-side, where the parser lives, so this
    client does not carry a second copy of that conversion — and real Fabric
    does its own. A client-side conversion would be two definitions of one
    thing, which is the defect this repository keeps finding.
    """
    if isinstance(content, dict):
        content = json.dumps(content)
    if isinstance(content, str):
        content = content.encode()
    if not content:
        # Documented: "Cannot be empty". Refused here rather than sent, so the
        # error names the parameter instead of arriving as a 400 about parts.
        raise NotebookError("content cannot be empty; pass valid .ipynb JSON")
    # A JSON OBJECT WITH NO `cells` IS EMPTY IN THE WAY THAT MATTERS: `{}` is
    # valid JSON and carries no notebook, so the server would derive an
    # executable part with nothing in it and the create would "succeed" into a
    # notebook that runs zero cells. Fabric documents the minimum shape
    # (cells / metadata / nbformat), and `cells` is the part that makes it one.
    #
    # Content that is not JSON at all is passed through untouched: the `.py`
    # form is what getDefinition falls back to, and refusing it here would break
    # the round trip updateDefinition uses for a lakehouse-only change.
    try:
        parsed = json.loads(content)
    except ValueError:
        parsed = None
    if isinstance(parsed, dict) and "cells" not in parsed:
        raise NotebookError(
            "content is not a notebook: valid .ipynb JSON needs `cells` "
            "(minimum: {\"cells\": [], \"metadata\": {}, \"nbformat\": 4, "
            "\"nbformat_minor\": 5})")
    return [{"path": "notebook-content.ipynb", "payloadType": "InlineBase64",
             "payload": base64.b64encode(content).decode()}]


def create(name, description="", content=None, defaultLakehouse=None,
           defaultLakehouseWorkspace=None, workspaceId=None):
    """Create a notebook item and return its `Artifact`."""
    ws = _ws(workspaceId)
    body = {"displayName": name, "type": "Notebook",
            "definition": {"parts": _notebook_parts(content)}}
    if description:
        body["description"] = description
    if defaultLakehouse:
        # The binding lives in the notebook's own metadata on Fabric, and the
        # emulator reads it from there too, so it is applied to the definition
        # rather than sent as an item field that neither would honour.
        body["definition"]["parts"] = _with_lakehouse(
            body["definition"]["parts"], defaultLakehouse,
            defaultLakehouseWorkspace or ws)
    status, headers, payload = request(
        "POST", f"{config().fabric_url}/v1/workspaces/{ws}/items",
        token=credentials.getToken("pbi"), body=body, raw=True)
    return Artifact(_follow(status, headers, payload, what="create"))


def _with_lakehouse(parts, lakehouse, lakehouse_ws):
    """Append the `dependencies.lakehouse` metadata Fabric reads for a default.

    Kept beside the parts rather than mutating the caller's `.ipynb`: the
    metadata block is Fabric's own convention for the binding, and rewriting a
    user's notebook JSON to carry it would change the artifact they handed us.
    """
    meta = {"dependencies": {"lakehouse": {
        "default_lakehouse_name": lakehouse,
        "default_lakehouse_workspace_id": lakehouse_ws}}}
    return [*parts, {
        "path": "notebook-content-metadata.json", "payloadType": "InlineBase64",
        "payload": base64.b64encode(json.dumps(meta).encode()).decode()}]


def get(name, workspaceId=None):
    """The notebook's `Artifact`, by display name or id."""
    ws, item_id = _resolve_item_or_id(name, workspaceId)
    raw = request("GET", f"{config().fabric_url}/v1/workspaces/{ws}/items/{item_id}",
                  token=credentials.getToken("pbi"))
    return Artifact(raw)


def getDefinition(name, workspaceId=None, format=None):  # noqa: A002 - Fabric's name
    """The notebook's content as `.ipynb`, as a string.

    `format` is accepted because Fabric documents it and defaults to `ipynb`;
    the emulator stores one definition, so anything else is refused rather than
    silently answered with ipynb.
    """
    if format not in (None, "", "ipynb"):
        raise NotebookError(f"unsupported format {format!r}; Fabric documents 'ipynb'")
    ws, item_id = _resolve_item_or_id(name, workspaceId)
    status, headers, payload = request(
        "POST",
        f"{config().fabric_url}/v1/workspaces/{ws}/items/{item_id}/getDefinition",
        token=credentials.getToken("pbi"), raw=True)
    body = _follow(status, headers, payload, what="getDefinition")
    parts = (body.get("definition") or {}).get("parts") or []
    for wanted in ("notebook-content.ipynb", "notebook-content.py"):
        for part in parts:
            if part.get("path") == wanted:
                return base64.b64decode(part.get("payload", "")).decode()
    raise NotebookError(
        f"notebook {name!r} has no notebook-content part to return")


def update(name, newName, description=None, workspaceId=None):
    """Rename a notebook (and optionally re-describe it). Returns the Artifact."""
    ws, item_id = _resolve_item_or_id(name, workspaceId)
    body = {"displayName": newName}
    if description is not None:
        body["description"] = description
    raw = request("PATCH", f"{config().fabric_url}/v1/workspaces/{ws}/items/{item_id}",
                  token=credentials.getToken("pbi"), body=body)
    return Artifact(raw)


def updateDefinition(name, content=None, defaultLakehouse=None,
                     defaultLakehouseWorkspace=None, workspaceId=None,
                     environmentId=None, environmentWorkspaceId=None):
    """Replace the notebook's content and/or its default lakehouse. True on success.

    `environmentId` / `environmentWorkspaceId` are accepted because Fabric
    documents them for the Spark runtime; the emulator binds an Environment
    through the item's own definition, so they are recorded in the metadata part
    rather than dropped — a parameter accepted and ignored is the shape §2 of
    docs/38 calls correct, but recording it costs nothing and keeps the
    round trip honest.
    """
    ws, item_id = _resolve_item_or_id(name, workspaceId)
    if content is None and defaultLakehouse is None:
        raise NotebookError(
            "updateDefinition needs content, defaultLakehouse, or both")
    parts = _notebook_parts(content) if content is not None else []
    if not parts:
        # Lakehouse-only change: keep what is there and re-send it, because the
        # API replaces the whole definition rather than patching it.
        current = getDefinition(name, workspaceId=ws)
        parts = _notebook_parts(current)
    if defaultLakehouse:
        parts = _with_lakehouse(parts, defaultLakehouse,
                                defaultLakehouseWorkspace or ws)
    if environmentId:
        parts = [*parts, {
            "path": "notebook-environment.json", "payloadType": "InlineBase64",
            "payload": base64.b64encode(json.dumps({
                "environmentId": environmentId,
                "environmentWorkspaceId": environmentWorkspaceId or ws,
            }).encode()).decode()}]
    status, headers, payload = request(
        "POST",
        f"{config().fabric_url}/v1/workspaces/{ws}/items/{item_id}/updateDefinition",
        token=credentials.getToken("pbi"), body={"definition": {"parts": parts}},
        raw=True)
    _follow(status, headers, payload, what="updateDefinition", want_result=False)
    return True


def delete(name, workspaceId=None):
    """Delete a notebook. True on success."""
    ws, item_id = _resolve_item_or_id(name, workspaceId)
    request("DELETE", f"{config().fabric_url}/v1/workspaces/{ws}/items/{item_id}",
            token=credentials.getToken("pbi"))
    return True


def list(workspaceId=None, maxResults=1000):  # noqa: A001 - Fabric's name
    """Every notebook in the workspace, as `Artifact`s."""
    ws = _ws(workspaceId)
    resp = request("GET", f"{config().fabric_url}/v1/workspaces/{ws}/items?type=Notebook",
                   token=credentials.getToken("pbi"))
    items = resp.get("value", [])[:maxResults]
    return [Artifact(it) for it in items]


def _resolve_item_or_id(name, workspaceId):
    """(workspace, item id) for a display name OR an id.

    Fabric's management APIs take "name or ID" everywhere, so a caller holding
    an id from `create()` must not have to look the name up again.
    """
    ws = _ws(workspaceId)
    try:
        return _resolve_item(name, ws, credentials.getToken("pbi"))
    except NotebookError:
        # Not a display name in this workspace; try it as an id before giving up.
        try:
            request("GET", f"{config().fabric_url}/v1/workspaces/{ws}/items/{name}",
                    token=credentials.getToken("pbi"))
        except Exception:
            raise NotebookError(
                f"notebook {name!r} not found in workspace {ws}") from None
        return ws, name


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
        cells = _cell_count(base, token)
        timeout_seconds = (
            (per_cell_seconds or _DEFAULT_TIMEOUT_PER_CELL) * cells
            if cells is not None
            else _TIMEOUT_WITHOUT_CELL_COUNT
        )
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


def _read_run_detail(base, token):
    """The run detail, or None when this target does not serve one.

    `…/jobs/instances/{jid}/notebookRun` is the EMULATOR's endpoint, paired with
    the `notebookRunResult` callback an engine posts to. MEASURED 2026-08-11:
    real Fabric answers it `404 EntityNotFound`. So every caller here has to
    work without it, and — more importantly — has to be able to TELL that it is
    working without it, rather than reading absence as an answer.
    """
    try:
        return request("GET", f"{base}/notebookRun", token=token)
    except Exception:  # noqa: BLE001 — an unreadable detail must not fail the run
        return None


def _warn_exit_value_unavailable():
    """Say WHY the exit value is missing, once, where it happens.

    Silence is the actual harm here. `None` is honest but a caller who never
    reads it learns nothing, and a caller who does gets a falsy value with no
    account of itself — so they debug their notebook, which is working, instead
    of learning that this path cannot carry an exit value at all.

    A warning rather than a raise, deliberately: plenty of orchestration submits
    notebooks and never looks at the return value, and failing those runs to
    report a limitation they are not hitting would be the cure being worse.
    The message names the pattern that does work, because "not supported" with
    no alternative is a dead end.

    Only on the real target. Against the emulator the endpoint exists, so a
    failure to read it is a transient worth surfacing on its own terms rather
    than a target limitation to explain away.
    """
    if not config().is_real:
        return
    warnings.warn(
        "This notebook run completed, but its exit value could not be "
        "retrieved and is reported as None.\n"
        "Real Fabric's REST job surface has no run-detail endpoint, so a run "
        "SUBMITTED OVER REST cannot carry an exit value back. This is not a "
        "failure of the notebook, and it is not something a retry will fix.\n"
        "Two things do work: call notebookutils.notebook.run() from INSIDE a "
        "Fabric notebook, where the runtime does return the child's exit value; "
        "or have the child write its result to a one-row Delta table and read "
        "that table, which is what the medallion examples do.",
        RuntimeWarning,
        stacklevel=4,
    )


def _cell_count(base, token):
    """How many cells this run will execute, or None when that is unknowable.

    NOT a number when the answer is unknown. The previous version returned 1,
    which reads as "a one-cell notebook" and is indistinguishable from the
    truth — and on real Fabric, where the detail endpoint does not exist, it
    was returned for EVERY run. A twelve-cell notebook then got a one-cell
    deadline and failed with a spurious timeout, while the same notebook passed
    against the emulator with the full budget. A fabricated 1 is the smallest
    possible lie and it produced the largest possible error.
    """
    detail = _read_run_detail(base, token)
    if detail is None:
        return None
    return max(len(detail.get("cells") or []), 1)


def _finish(base, token, status):
    """Read the run detail a terminal job left behind.

    Returns `(exitValue, status, cellCount)` where **exitValue is None when this
    target cannot report one at all**, and `""` when it reported that the
    notebook exited without a value. Those are different facts and the previous
    version collapsed them: it returned `""` for both, so on real Fabric — where
    the detail endpoint does not exist — every run looked like a notebook that
    had deliberately exited empty.

    That is the dangerous direction. A caller reading `""` concludes the
    notebook ran and returned nothing; the truth was that the value could not be
    obtained. Returning None makes the caller's `or ""`/`or 0` fallbacks behave
    exactly as before while leaving the distinction available to anyone who
    checks — and it is why contoso-data-platform returns metrics through a
    one-row Delta table rather than an exit value.
    """
    detail = _read_run_detail(base, token)
    if detail is None:
        _warn_exit_value_unavailable()
        return None, status, 0
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
        return _list(pool.map(_run_activity, activities))


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
    if isinstance(dag, (_list, tuple)):
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
    remaining = _list(activities)
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
