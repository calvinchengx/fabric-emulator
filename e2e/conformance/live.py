"""Live contract-4 write-landing for sail and jvm.

writer() publishes a notebook, submits RunNotebook, and polls. Its claim is
the job status — Completed or not. It does not list OneLake and it does not
ask spark.table. Those would be the writer confirming its own write, which
is every false green in docs/38 §4.

reader() mints a fresh Storage-audience token and lists
`lake.Lakehouse/Tables/events` over DFS HTTP. That process is not Spark and
never submitted the job.
"""
from __future__ import annotations

import base64
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(DIR))
from probes import (  # noqa: E402
    CONTROL,
    Artifact,
    ContextClaim,
    FallThroughClaim,
    IsolationClaim,
    RuntimeClaim,
    SignatureClaim,
    WriteClaim,
    concurrent_isolation,
    context_chain,
    fall_through,
    record,
    runtime_floor,
    signature_shape,
    write_landing,
)

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
# THE DFS SURFACE, ADDRESSED BY CONTAINER AND ROUTED BY HOST HEADER.
#
# The emulator decides OneLake-vs-control-plane from the Host header, so the
# hostname in the URL is not load-bearing — and it must not be, because on the
# jvm leg `onelake.dfs.fabric.microsoft.com` now resolves to a TLS terminator
# that exists for hadoop's benefit (`abfss://` forces TLS). Pointing these plain
# reads at that alias broke every one of them at once: contract 1 reported
# `<urlopen error>`, and 2, 3, 5 and 6 reported "the session did not get as far
# as..." — one unreachable reader wearing five different failures.
#
# This process is still an out-of-band reader: a different container, a token it
# minted itself, and no Spark. Which hostname it dials is not part of that claim.
ACCT = "fabric-emulator"
DFS_HOST = "onelake.dfs.fabric.microsoft.com"
TABLE_DIR = "lake.Lakehouse/Tables/events"
# Contract 1's findings, as a plain JSON file rather than a Delta table: the
# out-of-band reader is then a single DFS GET with no Parquet parser, and the
# file is what a Fabric notebook can write with one documented call.
FINDINGS_PATH = "lake.Lakehouse/Files/conformance/context.json"
# Contract 5's children write one file each, keyed by the marker they were
# given, so a child that wrote another child's file is visible as a mismatch
# rather than as an overwrite nobody can see.
FANOUT_DIR = "lake.Lakehouse/Files/conformance/fanout"
FANOUT_N = 3

# One cell, parameterised by the marker the harness bakes in. Its whole job is
# to say WHICH notebook it believes it is — the identity a shared agent could
# leak — alongside the marker only this child was given.
CHILD_BODY = """# Fabric notebook source

# CELL ********************
import json as _json

try:
    import notebookutils as _nbu

    _ctx = dict(_nbu.runtime.context)
    _nbu.fs.put(
        "abfss://__WS__@onelake.dfs.fabric.microsoft.com/__DIR__/__MARKER__.json",
        _json.dumps({
            "marker": "__MARKER__",
            "notebook": _ctx.get("currentNotebookId", ""),
            "workspace": _ctx.get("currentWorkspaceId", ""),
        }), True)
    print("child __MARKER__ wrote its findings")
except Exception as _exc:  # noqa: BLE001
    print("child __MARKER__ NOT written:", type(_exc).__name__, _exc)
"""

# The notebook-driven write path, and only that path: unqualified
# saveAsTable("events"). No CTAS, MERGE, mount, or qualified name — those
# are later contracts. df.count() is the in-memory frame, not a catalog
# read-back; the harness must not confirm through spark.table either.
NOTEBOOK_BODY = """# Fabric notebook source

# CELL ********************
df = spark.createDataFrame([(1, "a"), (2, "b"), (3, "c"), (4, "d")], ["id", "name"])
df.write.format("delta").mode("overwrite").saveAsTable("events")
print("wrote", df.count(), "rows")

# CELL ********************
# Contract 1. Each link of the chain is asked SEPARATELY, in the order a
# framework probes it, and each answer is recorded even when it raises --
# "this link is broken" is the finding, not a reason to abandon the cell.
#
# THE WHOLE CELL IS GUARDED, imports included. Contracts 1 and 4 share one run
# to keep it cheap, and a partial guard is not a guard: the first version
# wrapped only the write, so `import notebookutils` failing on the JVM overlay
# still failed the job and turned contract 4 red -- with its table landed and
# the out-of-band listing seeing five paths. One contract's red must never be
# another's, and the absent artifact is contract 1's failure signal by itself.
import json as _json, os as _os

try:
    import notebookutils as _nbu

    def _try(fn):
        try:
            return fn(), ""
        except Exception as exc:  # noqa: BLE001 -- a broken link is a finding
            return "", f"{type(exc).__name__}: {exc}"

    _env_ws, _env_err = _try(lambda: _nbu.mssparkutils.env.getWorkspaceId())
    _ctx, _ctx_err = _try(lambda: dict(_nbu.runtime.context))
    _findings = {
        "env_workspace": _env_ws,
        "context_workspace": (_ctx or {}).get("currentWorkspaceId", ""),
        "context_lakehouse": (_ctx or {}).get("defaultLakehouseId", ""),
        # The fallback the emulator keeps for a kernel with no agent. Reported
        # so the harness can REFUSE a pass that only the fallback earned.
        "fallback_workspace": _os.environ.get("NOTEBOOKUTILS_WORKSPACE_ID", ""),
        "env_fallback_set": bool(_os.environ.get("NOTEBOOKUTILS_WORKSPACE_ID")
                                 or _os.environ.get("NOTEBOOKUTILS_LAKEHOUSE_ID")),
        "errors": [e for e in (_env_err, _ctx_err) if e],
        # Absent in the emulator today; recorded so the gap is visible in the
        # artifact rather than only in prose.
        "context_capacity": (_ctx or {}).get("currentCapacityId", ""),
    }
    # Contract 2, in the same artifact and the same run. Signature shape is a
    # property of the surface as the SESSION sees it, so it is read here rather
    # than from the client's own import -- the client runs a different
    # interpreter with a different package set, and asserting on that would
    # prove something about the harness.
    import inspect as _inspect

    _sigs = {}
    for _name in dir(_nbu.notebook):
        if _name.startswith("_"):
            continue
        _fn = getattr(_nbu.notebook, _name, None)
        if not callable(_fn):
            continue
        try:
            _sigs[_name] = [p.name for p in
                            _inspect.signature(_fn).parameters.values()
                            if p.kind not in (p.VAR_POSITIONAL, p.VAR_KEYWORD)]
        except (TypeError, ValueError):
            # A builtin with no introspectable signature is not "absent"; say so
            # rather than letting it read as a missing method.
            _sigs[_name] = ["<no signature>"]
    _findings["notebook_signatures"] = _sigs
    # Contract 3. Read from the SESSION's own interpreter, because that is the
    # one a notebook's imports resolve against — reading the client's would
    # describe the harness. `spark.version` is recorded, not asserted: whether
    # the engine behaves like Spark 3.5 is the engine matrix's question.
    import sys as _sys

    # Contract 6. Two statements against one small Delta table: OPTIMIZE, which
    # the agent's grammar recognises and routes to delta-rs, and a MERGE with
    # `WHEN MATCHED THEN DELETE`, which it deliberately does NOT intercept. The
    # first proves the interception is installed at all; the second is the
    # contract. Both outcomes are recorded, never asserted here — the harness
    # decides, and it decides differently for the control engine.
    # RUN ONLY WHERE THE STACK CAN ANSWER. Contract 6's statements address the
    # table by `abfss://`, and hadoop's ABFS driver forces TLS for that scheme —
    # `fs.azure.always.use.https=false` downgrades `abfs://` and nothing else. On
    # the JVM overlay against a plaintext stack every request fails at the socket
    # with status 0, hadoop-azure treats that as retryable, and the notebook
    # hangs: measured as five threads parked in AbfsRestOperation.completeExecute
    # for 324s-864s, one per statement, accumulating and never exiting.
    #
    # A HANG IS WORSE THAN A RED, which is why this is gated rather than left to
    # fail. The statements share a notebook with contracts 1, 2, 3 and 4, so one
    # hang takes five cells down and reports the harness's own timeout as five
    # separate defects. Gating keeps those four honest and leaves contract 6
    # recording a gap with the reason above.
    #
    # A TLS terminator in front of the OneLake alias DOES fix the statements
    # (4.6s / 2.8s / 1.5s / 2.5s where they previously hung), and is not here:
    # it also regressed cell 0's write to the local read-only Spark warehouse,
    # for a reason not yet understood. Shipping a fix that trades one red for
    # another is not a fix.
    if _os.environ.get("CONFORMANCE_FALL_THROUGH") == "1":
        # PATH-ADDRESSED, not by catalog name. The delta-rs interception resolves
        # a NAME through the emulator's registration, and a table written by
        # `saveAsTable` inside a notebook is not registered that way — `OPTIMIZE
        # events` fails with `cannot resolve 'events' to a table location: it was
        # not registered through the emulator`. Measured, twice, on this probe's
        # first two runs. The path form is what `e2e/livy` proves against a real
        # abfss OneLake table, so it is the shape a notebook author is told to
        # use.
        #
        # On `events`, the table cell 0 already wrote. The coupling with contract
        # 4 is one-way and harmless: OPTIMIZE compacts and the MERGE deletes at
        # most one row, while contract 4's reader asserts only that OneLake lists
        # paths under the table, which both leave true.
        _tbl = ("abfss://__WS__@onelake.dfs.fabric.microsoft.com"
                "/lake.Lakehouse/Tables/events")
        _rec, _rec_err = _try(lambda: spark.sql(f"OPTIMIZE delta.`{_tbl}`").collect())
        _unrec, _unrec_err = _try(lambda: spark.sql(
            f"MERGE INTO delta.`{_tbl}` t USING (SELECT 1 AS id) s "
            "ON t.id = s.id WHEN MATCHED THEN DELETE").collect())
        _findings["fall_through"] = {
            "table_error": "",
            "recognised_ok": not _rec_err,
            "recognised_error": _rec_err,
            "unrecognised_ok": not _unrec_err,
            "unrecognised_error": _unrec_err,
        }
    else:
        _findings["fall_through"] = {"skipped": True}

    _findings["runtime"] = {
        "declared": _os.environ.get("FABRIC_RUNTIME", ""),
        "python": ".".join(str(n) for n in _sys.version_info[:3]),
        "spark": _try(lambda: spark.version)[0],
    }
    # An EXPLICIT abfss target, never a relative path: fs._resolve() reads a
    # relative one out of the very runtime context under test, so a relative
    # write would make the artifact's location depend on the answer measured.
    _nbu.fs.put("abfss://__WS__@onelake.dfs.fabric.microsoft.com/__FINDINGS__",
                _json.dumps(_findings), True)
    print("context findings written")
except Exception as _exc:  # noqa: BLE001
    print("context findings NOT written:", type(_exc).__name__, _exc)
"""

# Set by writer() so the readers can address the same workspace, and so the
# contract-1 comparison has the ids the CONTROL PLANE issued rather than the
# ones the session believes. Empty if the writer never got that far.
_ws = ""
_lake = ""
# What the notebook printed. A missing artifact says only "404"; the cell that
# failed to write it says WHY, and a red that does not point at its cause is
# most of the way back to a silent skip.
_said = ""
# Contract 2's half of the same findings file, kept beside the contract-1 half
# so one DFS read serves both.
_sigs_seen = None
_runtime_seen = None
_fall_seen = None


def log(msg: str) -> None:
    print(f"==> {msg}", flush=True)


def req(method, url, body=None, token=None, form=False, headers=None):
    data, headers = None, dict(headers or {})
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
    with urllib.request.urlopen(r, timeout=60) as resp:
        raw = resp.read()
        return resp.status, resp.headers, (json.loads(raw) if raw else {})


def fabric_token() -> str:
    return req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET,
        "scope": "https://api.fabric.microsoft.com/.default"}, form=True)[2]["access_token"]


def storage_token() -> str:
    """A fresh Storage-audience token, minted here — never the writer's."""
    try:
        req("POST", f"{ENTRA}/admin/api/apps", {
            "displayName": "Azure Storage", "appIdUri": "https://storage.azure.com",
            "isConfidential": False})
    except urllib.error.HTTPError as e:
        if e.code != 409:
            raise
    return req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET,
        "scope": "https://storage.azure.com/.default"}, form=True)[2]["access_token"]


def writer() -> WriteClaim:
    """Publish + submit + poll. The claim is the job status, nothing else."""
    global _ws
    ft = fabric_token()
    ws = req("POST", f"{FABRIC}/v1/workspaces", {"displayName": "nb-ws"}, token=ft)[2]["id"]
    lake = req("POST", f"{FABRIC}/v1/workspaces/{ws}/lakehouses",
               {"displayName": "lake"}, token=ft)[2]
    _ws = ws
    global _lake
    _lake = lake["id"]
    log(f"workspace {ws}, lakehouse {lake['id']}")

    # Default lakehouse in the notebook's own metadata — what makes
    # saveAsTable("events") land in this lakehouse rather than nowhere.
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
            "payload": base64.b64encode(
                (NOTEBOOK_BODY.replace("__WS__", ws).replace("__FINDINGS__", FINDINGS_PATH)
                 + meta).encode()).decode()}]}},
        token=ft)
    opid = headers.get("x-ms-operation-id")
    nb = None
    for _ in range(60):
        body = req("GET", f"{FABRIC}/v1/operations/{opid}", token=ft)[2]
        if body.get("status") == "Succeeded":
            nb = req("GET", f"{FABRIC}/v1/operations/{opid}/result", token=ft)[2]["id"]
            break
        time.sleep(1)
    if not nb:
        return WriteClaim(ok=False, error="the notebook item never finished creating")
    log(f"notebook {nb}")

    _, hdrs, _ = req(
        "POST", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances?jobType=RunNotebook",
        token=ft)
    jid = hdrs["Location"].rstrip("/").rsplit("/", 1)[-1]
    log(f"submitted RunNotebook job {jid}")

    base = f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}"
    status = None
    # GENEROUS ON PURPOSE. This notebook now carries contracts 1, 2, 3, 4 and 6,
    # and contract 6 runs OPTIMIZE and MERGE against an abfss table — seconds on
    # Sail, minutes on the JVM overlay. At 180s the jvm leg reported
    # `job status = InProgress` and every contract in the artifact failed for
    # one reason: the harness gave up first. A timeout that reads as five
    # defects is worse than a slow run.
    for _ in range(900):
        status = req("GET", base, token=ft)[2].get("status")
        if status in ("Completed", "Failed", "Cancelled", "Deduped"):
            break
        time.sleep(1)
    if status != "Completed":
        # The run detail says which cell died. Logging it is not confirmation —
        # confirmation is the reader's listing.
        try:
            detail = req("GET", f"{base}/notebookRun", token=ft)[2]
            for c in sorted(detail.get("cells", []), key=lambda c: c["index"]):
                log(f"cell {c['index']} {c['status']}: "
                    f"{c.get('error') or c.get('output', '')[:300]}")
        except Exception as exc:  # noqa: BLE001 — best-effort diagnostics
            log(f"could not fetch run detail: {exc}")
        return WriteClaim(ok=False, error=f"job status = {status}")
    log(f"job reached {status}")
    _remember_output(base, ft)
    return WriteClaim(ok=True)


def _remember_output(base: str, ft: str) -> None:
    """Keep the cells' stdout so a later contract can quote its own failure.

    Fetched on SUCCESS as well as failure: contract 1's cell is guarded so it
    cannot fail the job, which means its reason only ever appears here.
    """
    global _said
    try:
        detail = req("GET", f"{base}/notebookRun", token=ft)[2]
        _said = "\n".join(c.get("output", "") for c in detail.get("cells", []))
    except Exception as exc:  # noqa: BLE001 — diagnostics, never the verdict
        log(f"could not fetch run detail: {exc}")


def reader() -> Artifact:
    """New Storage token, then a DFS listing. Not Spark, not the writer."""
    if not _ws:
        return Artifact(found=False, location=TABLE_DIR)
    # Fresh token, minted here — not the Fabric-audience token writer() used.
    sft = storage_token()
    try:
        listing = req("GET", f"http://{ACCT}/{_ws}?resource=filesystem&recursive=true"
                             f"&directory={urllib.parse.quote(TABLE_DIR)}",
                      token=sft, headers={"Host": DFS_HOST})[2]
    except Exception as exc:  # noqa: BLE001 — a missing table is found=False
        log(f"reader listing failed: {exc}")
        return Artifact(found=False, location=TABLE_DIR)
    names = [p["name"] for p in listing.get("paths", [])]
    log(f"OneLake listing under {TABLE_DIR}: {len(names)} path(s)")
    return Artifact(found=bool(names), location=TABLE_DIR)


def session_context() -> ContextClaim:
    """Read the findings the notebook wrote, over DFS. Not Spark, not the writer.

    This process never ran inside the session, so what it can testify to is
    what the session RECORDED. The comparison that makes that meaningful is
    against `_ws` / `_lake`, which the control plane issued to the harness
    before the notebook existed — see probes.context_chain.
    """
    if not _ws:
        return ContextClaim(ok=False, error="no workspace was created")
    try:
        sft = storage_token()
        r = urllib.request.Request(
            f"http://{ACCT}/{_ws}/{urllib.parse.quote(FINDINGS_PATH)}",
            headers={"Authorization": "Bearer " + sft, "Host": DFS_HOST})
        with urllib.request.urlopen(r, timeout=60) as resp:
            found = json.loads(resp.read())
    except Exception as exc:  # noqa: BLE001 — a missing artifact is the finding
        said = next((ln.strip() for ln in _said.splitlines()
                     if ln.startswith("context findings NOT written:")), "")
        why = f" — the session said: {said}" if said else ""
        return ContextClaim(
            ok=False,
            error=f"no context findings at {FINDINGS_PATH}: {exc}{why}")
    global _sigs_seen, _runtime_seen
    _sigs_seen = found.get("notebook_signatures")
    _runtime_seen = found.get("runtime")
    global _fall_seen
    _fall_seen = found.get("fall_through")
    log(f"context findings: { {k: v for k, v in found.items() if k != 'notebook_signatures'} }")
    log(f"notebook signatures reported: {len(_sigs_seen or {})} callables")
    return ContextClaim(
        ok=True,
        env_workspace=found.get("env_workspace", ""),
        context_workspace=found.get("context_workspace", ""),
        context_lakehouse=found.get("context_lakehouse", ""),
        fallback_workspace=found.get("fallback_workspace", ""),
        env_fallback_set=bool(found.get("env_fallback_set")),
        error="; ".join(found.get("errors", [])),
    )


def session_signatures() -> SignatureClaim:
    """Contract 2 reads the artifact contract 1 already fetched.

    Deliberately NOT a second notebook or a second read: the findings file
    carries both halves, so the two contracts describe one session and one
    round trip. `session_context()` must have run first; if it did not, the
    absent signatures are reported rather than silently read as an empty
    surface, which would fail every method for the wrong reason.
    """
    if _sigs_seen is None:
        return SignatureClaim(
            ok=False,
            error="no signatures in the findings artifact — "
                  "the session did not get as far as reporting them")
    return SignatureClaim(ok=True, seen=_sigs_seen)


def session_runtime() -> RuntimeClaim:
    """Contract 3 reads the same artifact contracts 1 and 2 already fetched."""
    if _runtime_seen is None:
        return RuntimeClaim(
            ok=False,
            error="no runtime in the findings artifact — "
                  "the session did not get as far as reporting it")
    return RuntimeClaim(
        ok=True,
        declared=_runtime_seen.get("declared", ""),
        python=_runtime_seen.get("python", ""),
        spark=_runtime_seen.get("spark", ""),
    )


def fan_out() -> dict[str, str]:
    """Publish N notebooks, submit them AT ONCE, and wait for all of them.

    Submitting serially would prove nothing: the leak contract 5 is about only
    exists while two sessions are live in the same agent at the same time. So
    every job is submitted before any is polled.

    Returns marker -> the notebook id the control plane issued, which is what
    the probe compares each child's own belief against.
    """
    ft = fabric_token()
    expected, jobs = {}, []
    for i in range(FANOUT_N):
        marker = f"child{i}"
        body = (CHILD_BODY.replace("__WS__", _ws)
                .replace("__DIR__", FANOUT_DIR).replace("__MARKER__", marker))
        _, headers, _ = req(
            "POST", f"{FABRIC}/v1/workspaces/{_ws}/items", {
                "displayName": f"fanout-{marker}", "type": "Notebook",
                "definition": {"parts": [{
                    "path": "notebook-content.py", "payloadType": "InlineBase64",
                    "payload": base64.b64encode(body.encode()).decode()}]}},
            token=ft)
        opid = headers.get("x-ms-operation-id")
        nb = None
        for _ in range(60):
            op = req("GET", f"{FABRIC}/v1/operations/{opid}", token=ft)[2]
            if op.get("status") == "Succeeded":
                nb = req("GET", f"{FABRIC}/v1/operations/{opid}/result", token=ft)[2]["id"]
                break
            time.sleep(1)
        if not nb:
            log(f"{marker}: notebook never finished creating")
            continue
        expected[marker] = nb
        _, hdrs, _ = req(
            "POST",
            f"{FABRIC}/v1/workspaces/{_ws}/items/{nb}/jobs/instances?jobType=RunNotebook",
            token=ft)
        jobs.append((marker, nb, hdrs["Location"].rstrip("/").rsplit("/", 1)[-1]))
    log(f"submitted {len(jobs)} concurrent RunNotebook jobs")

    deadline = time.monotonic() + 300
    pending = {j: (m, nb) for m, nb, j in jobs}
    while pending and time.monotonic() < deadline:
        for jid, (marker, nb) in list(pending.items()):
            base = f"{FABRIC}/v1/workspaces/{_ws}/items/{nb}/jobs/instances/{jid}"
            status = req("GET", base, token=ft)[2].get("status")
            if status in ("Completed", "Failed", "Cancelled", "Deduped"):
                log(f"{marker}: {status}")
                pending.pop(jid)
        if pending:
            time.sleep(1)
    return expected


def session_isolation(expected: dict) -> IsolationClaim:
    """Read every child's artifact over DFS. Not Spark, not any of the writers."""
    if not expected:
        return IsolationClaim(ok=False, error="no children were created")
    sft = storage_token()
    seen = {}
    for marker in expected:
        try:
            r = urllib.request.Request(
                f"http://{ACCT}/{_ws}/{urllib.parse.quote(FANOUT_DIR)}/{marker}.json",
                headers={"Authorization": "Bearer " + sft, "Host": DFS_HOST})
            with urllib.request.urlopen(r, timeout=60) as resp:
                seen[marker] = json.loads(resp.read())
        except Exception as exc:  # noqa: BLE001 — a missing child is the finding
            log(f"{marker}: no findings ({exc})")
    log(f"fan-out artifacts read: {len(seen)}/{len(expected)}")
    return IsolationClaim(ok=True, seen=seen)


def session_fall_through() -> FallThroughClaim:
    """Contract 6 reads the same artifact contracts 1-3 already fetched."""
    if _fall_seen is None:
        return FallThroughClaim(
            ok=False,
            error="no fall-through results in the findings artifact — "
                  "the session did not get as far as running the statements")
    if _fall_seen.get("skipped"):
        return FallThroughClaim(
            ok=False,
            error=("not run on this backend: the statements address the table by "
                   "`abfss://`, hadoop forces TLS for that scheme, and this stack "
                   "serves plaintext — every request fails at the socket and "
                   "hadoop-azure retries forever, hanging the notebook and taking "
                   "contracts 1-4 down with it. Measured: five threads parked in "
                   "AbfsRestOperation.completeExecute for 324s-864s"))
    if _fall_seen.get("table_error"):
        return FallThroughClaim(
            ok=False,
            error=("the probe table could not be created "
                   f"({_fall_seen['table_error'][:160]}), so neither statement "
                   "says anything about the grammar"))
    return FallThroughClaim(
        ok=True,
        recognised_ok=bool(_fall_seen.get("recognised_ok")),
        recognised_error=_fall_seen.get("recognised_error", ""),
        unrecognised_ok=bool(_fall_seen.get("unrecognised_ok")),
        unrecognised_error=_fall_seen.get("unrecognised_error", ""),
    )


def main() -> int:
    backend = os.environ.get("BACKEND", "")
    if backend not in ("sail", "jvm"):
        print(f"BACKEND must be sail or jvm, got {backend!r}", file=sys.stderr)
        return 2
    result = write_landing(
        writer=writer,
        reader=reader,
        expected_location=TABLE_DIR,
        backend=backend,
    )
    # Contract 1 rides the SAME run: the notebook that wrote the table also
    # recorded its context, so the two cells describe one session rather than
    # two, and the run costs no extra notebook.
    ctx = context_chain(
        session=session_context,
        expected_workspace=_ws,
        expected_lakehouse=_lake,
        backend=backend,
    )
    ref = json.loads((DIR / "notebookutils-reference.json").read_text(
        encoding="utf-8"))["modules"]["notebookutils.notebook"]
    sig = signature_shape(session=session_signatures, reference=ref, backend=backend)
    runtimes = json.loads((DIR / "fabric-runtimes.json").read_text(
        encoding="utf-8"))["runtimes"]
    floor = runtime_floor(session=session_runtime, runtimes=runtimes, backend=backend)
    expected = fan_out()
    iso = concurrent_isolation(
        session=lambda: session_isolation(expected),
        expected=expected, backend=backend)
    # `control` comes from the applicability table, not from a backend name:
    # the jvm column is `control` for this contract and required for the rest,
    # and the doc is the authority on which is which.
    fall = fall_through(session=session_fall_through, backend=backend,
                        control=(6, backend) in CONTROL)
    rows = record(backend,
                  live={1: ctx, 2: sig, 3: floor, 4: result, 5: iso, 6: fall})
    out = DIR / "out"
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{backend}.json"
    path.write_text(json.dumps(rows, indent=2) + "\n", encoding="utf-8")
    log(f"wrote {path} contract 1={ctx.status} {ctx.error}".rstrip())
    log(f"wrote {path} contract 2={sig.status} {sig.error}".rstrip())
    log(f"wrote {path} contract 3={floor.status} {floor.error}".rstrip())
    log(f"wrote {path} contract 5={iso.status} {iso.error}".rstrip())
    log(f"wrote {path} contract 6={fall.status} {fall.error}".rstrip())
    log(f"wrote {path} contract 4={result.status} {result.error}".rstrip())
    return 0


if __name__ == "__main__":
    sys.exit(main())
