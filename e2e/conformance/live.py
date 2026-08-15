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
from probes import Artifact, WriteClaim, record, write_landing  # noqa: E402

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
ACCT = "onelake.dfs.fabric.microsoft.com"
TABLE_DIR = "lake.Lakehouse/Tables/events"

# The notebook-driven write path, and only that path: unqualified
# saveAsTable("events"). No CTAS, MERGE, mount, or qualified name — those
# are later contracts. df.count() is the in-memory frame, not a catalog
# read-back; the harness must not confirm through spark.table either.
NOTEBOOK_BODY = """# Fabric notebook source

# CELL ********************
df = spark.createDataFrame([(1, "a"), (2, "b"), (3, "c"), (4, "d")], ["id", "name"])
df.write.format("delta").mode("overwrite").saveAsTable("events")
print("wrote", df.count(), "rows")
"""

# Set by writer() so reader() can list the same workspace. Empty if the
# writer never got as far as creating one.
_ws = ""


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


def writer() -> WriteClaim:
    """Publish + submit + poll. The claim is the job status, nothing else."""
    global _ws
    ft = fabric_token()
    ws = req("POST", f"{FABRIC}/v1/workspaces", {"displayName": "nb-ws"}, token=ft)[2]["id"]
    lake = req("POST", f"{FABRIC}/v1/workspaces/{ws}/lakehouses",
               {"displayName": "lake"}, token=ft)[2]
    _ws = ws
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
    return WriteClaim(ok=True)


def reader() -> Artifact:
    """New Storage token, then a DFS listing. Not Spark, not the writer."""
    if not _ws:
        return Artifact(found=False, location=TABLE_DIR)
    try:
        req("POST", f"{ENTRA}/admin/api/apps", {
            "displayName": "Azure Storage", "appIdUri": "https://storage.azure.com",
            "isConfidential": False})
    except urllib.error.HTTPError as e:
        if e.code != 409:
            raise
    # Fresh token, minted here — not the Fabric-audience token writer() used.
    sft = req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET,
        "scope": "https://storage.azure.com/.default"}, form=True)[2]["access_token"]
    try:
        listing = req("GET", f"http://{ACCT}/{_ws}?resource=filesystem&recursive=true"
                             f"&directory={urllib.parse.quote(TABLE_DIR)}", token=sft)[2]
    except Exception as exc:  # noqa: BLE001 — a missing table is found=False
        log(f"reader listing failed: {exc}")
        return Artifact(found=False, location=TABLE_DIR)
    names = [p["name"] for p in listing.get("paths", [])]
    log(f"OneLake listing under {TABLE_DIR}: {len(names)} path(s)")
    return Artifact(found=bool(names), location=TABLE_DIR)


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
    rows = record(backend, live_write=lambda: result)
    out = DIR / "out"
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{backend}.json"
    path.write_text(json.dumps(rows, indent=2) + "\n", encoding="utf-8")
    log(f"wrote {path} contract 4={result.status} {result.error}".rstrip())
    return 0


if __name__ == "__main__":
    sys.exit(main())
