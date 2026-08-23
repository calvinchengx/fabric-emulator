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
    Artifact,
    ContextClaim,
    WriteClaim,
    context_chain,
    record,
    write_landing,
)

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
ACCT = "onelake.dfs.fabric.microsoft.com"
TABLE_DIR = "lake.Lakehouse/Tables/events"
# Contract 1's findings, as a plain JSON file rather than a Delta table: the
# out-of-band reader is then a single DFS GET with no Parquet parser, and the
# file is what a Fabric notebook can write with one documented call.
FINDINGS_PATH = "lake.Lakehouse/Files/conformance/context.json"

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


def log(msg: str) -> None:
    print(f"==> {msg}", flush=True)


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
    for _ in range(180):
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
                             f"&directory={urllib.parse.quote(TABLE_DIR)}", token=sft)[2]
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
            headers={"Authorization": "Bearer " + sft})
        with urllib.request.urlopen(r, timeout=60) as resp:
            found = json.loads(resp.read())
    except Exception as exc:  # noqa: BLE001 — a missing artifact is the finding
        said = next((ln.strip() for ln in _said.splitlines()
                     if ln.startswith("context findings NOT written:")), "")
        why = f" — the session said: {said}" if said else ""
        return ContextClaim(
            ok=False,
            error=f"no context findings at {FINDINGS_PATH}: {exc}{why}")
    log(f"context findings: {found}")
    return ContextClaim(
        ok=True,
        env_workspace=found.get("env_workspace", ""),
        context_workspace=found.get("context_workspace", ""),
        context_lakehouse=found.get("context_lakehouse", ""),
        fallback_workspace=found.get("fallback_workspace", ""),
        env_fallback_set=bool(found.get("env_fallback_set")),
        error="; ".join(found.get("errors", [])),
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
    rows = record(backend, live={1: ctx, 4: result})
    out = DIR / "out"
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{backend}.json"
    path.write_text(json.dumps(rows, indent=2) + "\n", encoding="utf-8")
    log(f"wrote {path} contract 1={ctx.status} {ctx.error}".rstrip())
    log(f"wrote {path} contract 4={result.status} {result.error}".rstrip())
    return 0


if __name__ == "__main__":
    sys.exit(main())
