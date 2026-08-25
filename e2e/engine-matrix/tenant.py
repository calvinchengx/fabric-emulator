#!/usr/bin/env python3
"""The engine probes, run as a NOTEBOOK on whichever target is configured.

WHY THIS EXISTS. `docs/engine-matrix.md` compares Sail against JVM Spark, and
its own header now says what that is worth: **the JVM column is upstream Apache
Spark, not Fabric's distribution**, so a matching row means "matches OSS Spark"
rather than "matches Fabric". Nothing in this repository has ever compared an
engine capability against Fabric's own Spark.

This closes that by running THE SAME `probes.py` — not a re-implementation —
inside a published notebook, so the probe set has one definition and the tenant
column cannot drift from the Sail and JVM columns.

HOW THE RESULTS GET OUT, and why it is not the exit value. On real Fabric
`…/jobs/instances/{id}/notebookRun` answers **404** (measured 2026-08-11, see
the `notebookutils.notebook.run` — exit values OVER REST row in parity.md), so
a notebook's output is not retrievable from outside. The documented way through
is for the notebook to write its result where a client can read it, which is
also the conformance kit's out-of-band rule: the component that ran the work is
not the one that reports it. So the notebook writes JSON to the lakehouse and
this reads it back over OneLake with a separate token.

WHAT DOES NOT PORT, named rather than dropped. A tenant notebook has no Kafka
broker and no place to park a streaming query, and the JVM-bridge probes ask
about a runtime detail rather than a Fabric capability. Those record as
`skipped` WITH THE REASON instead of vanishing: a probe that disappears from a
column reads as one that passed.

Run it against either target — the emulator path is what makes this testable
before any tenant credential exists:

    FABRIC_TARGET=emulator FABRIC_WORKSPACE=<name> python e2e/engine-matrix/tenant.py
    FABRIC_TARGET=real     FABRIC_WORKSPACE=<name> python e2e/engine-matrix/tenant.py

Only a `real` run writes `out/fabric.json`. An emulator run exercises every step
and prints its results, because writing the emulator's own answers into the
column labelled Fabric would be the plainest possible lie.
"""
from __future__ import annotations

import base64
import json
import os
import pathlib
import sys
import time
import urllib.request

DIR = pathlib.Path(__file__).resolve().parent
OUT = DIR / "out"

sys.path.insert(0, str(DIR))
import probes  # noqa: E402  — the SAME probe definitions the other columns use

# Probes that cannot mean anything in a published notebook, each with the
# reason. Recorded as `skipped` in the results, never omitted.
TENANT_SKIP = {
    "streaming.read_kafka": "needs a Kafka broker; a tenant notebook has none",
    "streaming.sink_console": "a console sink writes to a driver stdout no client reads",
    "streaming.sink_memory": "an in-memory sink dies with the session that held it",
    "streaming.sink_parquet": "a long-running query has nowhere to park in a batch job",
    "streaming.sink_delta": "as sink_parquet",
    "streaming.read_rate": "as sink_parquet — the rate source needs a running query",
    "jvm.rdd_sparkcontext": "asks which runtime is underneath, not what Fabric can do",
    "jvm.bridge": "as rdd_sparkcontext",
}

RESULT_PATH = "Files/engine-matrix/results.json"

# TWO DIFFERENT ADDRESSES FOR ONE STORE, and conflating them is what the first
# run did. The abfss URIs the ENGINE resolves always name the canonical OneLake
# host: that is what Fabric's Spark expects, and what the emulator's stack
# resolves too, because it routes on the Host header rather than on DNS. The
# CLIENT-side read below uses `t.onelake_url`, which is the emulator's own
# address locally and the canonical host on a tenant.
#
# Using the client address in the engine URI produced, from Sail:
#   URL did not match any known pattern for scheme:
#   abfss://<ws>@fabric-emulator/<item>/Files/engine-probe/t_write
ONELAKE_HOST = "onelake.dfs.fabric.microsoft.com"


def notebook_source(probe_dir: str, result_uri: str) -> str:
    """A Fabric notebook that runs the probes and writes its findings out.

    `probes.py` is EMBEDDED rather than imported: a notebook on a tenant has no
    access to this repository, and vendoring the source is what keeps the tenant
    column running the identical probe bodies rather than a copy that drifts.
    """
    body = probes.__file__ and pathlib.Path(probes.__file__).read_text(encoding="utf-8")
    cell = (
        "import json, os\n"
        f"os.environ['PROBE_DIR'] = {probe_dir!r}\n"
        f"SKIP = {json.dumps(TENANT_SKIP)}\n"
        "\n"
        "# ---- probes.py, verbatim ----\n"
        f"{body}\n"
        "# ---- end probes.py ----\n"
        "\n"
        "results = []\n"
        "for probe_id, description, fn in PROBES:\n"
        "    entry = {'id': probe_id, 'description': description, 'engine': 'fabric'}\n"
        "    if probe_id in SKIP:\n"
        "        entry['status'] = 'skipped'\n"
        "        entry['error'] = SKIP[probe_id]\n"
        "    else:\n"
        "        try:\n"
        "            fn(spark)\n"
        "            entry['status'] = 'pass'\n"
        "        except Exception as exc:\n"
        "            entry['status'] = 'fail'\n"
        "            entry['error_class'] = type(exc).__name__\n"
        "            entry['error'] = ' '.join(str(exc).split())[:180]\n"
        "    results.append(entry)\n"
        "    print(probe_id, entry['status'], flush=True)\n"
        "\n"
        "# OUT OF BAND: the notebook writes; a different client reads. The exit\n"
        "# value cannot carry this — notebookRun 404s on real Fabric.\n"
        "import notebookutils\n"
        # An ABSOLUTE abfss URI, not `Files/...`: a relative path needs a bound
        # default lakehouse, and this notebook is published without one. The
        # emulator says so outright — "relative path needs a default lakehouse"
        # — which is how this was found; a tenant would fail differently, and
        # an explicit URI depends on neither.
        f"notebookutils.fs.put({result_uri!r}, json.dumps(results), True)\n"
        "print('engine probes written:', len(results), flush=True)\n"
    )
    return "# Fabric notebook source\n\n# CELL ********************\n" + cell


def _by_name(session, path, display_name, attempts=60):
    """Items with this display name, polled until the create settles.

    A create may answer 201 with the item or 202 with an operation; listing
    covers both, and the retry covers the async one still landing.
    """
    for _ in range(attempts):
        found = [item for item in session.get(path).json().get("value", [])
                 if item.get("displayName") == display_name]
        if found:
            return found
        time.sleep(1)
    return []


def main() -> int:
    try:
        import fabric_target
    except ImportError:
        print("fabric_target is not installed: uv run --group fabric-target ...",
              file=sys.stderr)
        return 2

    t = fabric_target.target()
    session = t.session()
    name = os.environ.get("FABRIC_WORKSPACE")
    try:
        ws = t.workspace(name)
    except fabric_target.TargetError:
        # ON A TENANT THE WORKSPACE IS PRE-PROVISIONED and this must not invent
        # one: FABRIC_TEST_WORKSPACE names a dedicated throwaway, and a runner
        # that creates workspaces on someone's tenant is a different kind of
        # program. The emulator leg has nothing to reuse, so it makes its own —
        # and `emulator_only` is what makes that asymmetry a refusal rather
        # than a comment nobody reads.
        t.emulator_only(f"creating the workspace {name!r} for a probe run")
        t.poll_lro(session.post("/workspaces", json={"displayName": name}))
        ws = t.workspace(name)

    lakes = session.get(f"/workspaces/{ws.id}/lakehouses").json().get("value", [])
    if not lakes:
        session.post(f"/workspaces/{ws.id}/lakehouses",
                     json={"displayName": "engine-probe-lake"})
        # RESOLVED BY LISTING, not from the create response. `poll_lro` hands
        # back the OPERATION's state for a 202, not the item it made, and both
        # outcomes are legal here — so reading an id out of it works against
        # one target and returns a status document against the other.
        lakes = _by_name(session, f"/workspaces/{ws.id}/lakehouses",
                         "engine-probe-lake")
    lake = lakes[0]
    probe_dir = (f"abfss://{ws.id}@{ONELAKE_HOST}/"
                 f"{lake['id']}/Files/engine-probe")

    result_uri = f"abfss://{ws.id}@{ONELAKE_HOST}/{lake['id']}/{RESULT_PATH}"
    source = notebook_source(probe_dir, result_uri)
    name = f"engine-probes-{int(time.time())}"
    session.post(f"/workspaces/{ws.id}/items", json={
        "displayName": name, "type": "Notebook",
        "definition": {"parts": [{
            "path": "notebook-content.py", "payloadType": "InlineBase64",
            "payload": base64.b64encode(source.encode()).decode()}]}})
    found = _by_name(session, f"/workspaces/{ws.id}/items?type=Notebook", name)
    if not found:
        print(f"the notebook {name!r} was never created", file=sys.stderr)
        return 1
    item = found[0]
    print(f"published {item['id']}", flush=True)

    started = session.post(
        f"/workspaces/{ws.id}/items/{item['id']}/jobs/instances?jobType=RunNotebook")
    location = started.headers.get("Location", "")
    jid = location.rstrip("/").rsplit("/", 1)[-1]
    base = f"/workspaces/{ws.id}/items/{item['id']}/jobs/instances/{jid}"
    status = None
    for _ in range(900):
        status = session.get(base).json().get("status")
        if status in ("Completed", "Failed", "Cancelled", "Deduped"):
            break
        time.sleep(1)
    print(f"job {status}", flush=True)
    if status != "Completed":
        # WHY IT FAILED, where the target will say. `…/notebookRun` answers on
        # the emulator and 404s on real Fabric (parity.md), so this is
        # best-effort by construction — but a runner that cannot report the
        # cell that died leaves the reader with "Failed" and nothing else.
        try:
            detail = session.get(f"{base}/notebookRun").json()
            for cell in sorted(detail.get("cells", []), key=lambda c: c["index"]):
                if cell.get("error") or cell.get("status") == "Failed":
                    print(f"  cell {cell['index']} {cell.get('status')}: "
                          f"{(cell.get('error') or cell.get('output', ''))[:600]}",
                          file=sys.stderr)
        except Exception as exc:  # noqa: BLE001 — diagnostics, never the verdict
            print(f"  (no run detail on this target: {exc})", file=sys.stderr)

    results = read_results(t, ws.id, lake["id"])
    if results is None:
        print("the notebook left no findings — nothing to record", file=sys.stderr)
        return 1

    passed = sum(1 for r in results if r["status"] == "pass")
    skipped = sum(1 for r in results if r["status"] == "skipped")
    print(f"{passed} pass, {len(results) - passed - skipped} fail, {skipped} skipped")

    if not t.is_real:
        # The emulator's own answers must never fill the column labelled Fabric.
        print("target is the emulator: results printed, out/fabric.json NOT written")
        print(json.dumps(results, indent=2)[:2000])
        return 0
    OUT.mkdir(parents=True, exist_ok=True)
    (OUT / "fabric.json").write_text(json.dumps(results, indent=2) + "\n",
                                     encoding="utf-8")
    print(f"wrote {OUT / 'fabric.json'}")
    return 0


def read_results(t, workspace_id, lakehouse_id):
    """The findings, read over OneLake by a client that is not the notebook."""
    token = t.credential.get_token("https://storage.azure.com/.default").token
    url = f"{t.onelake_url}/{workspace_id}/{lakehouse_id}/{RESULT_PATH}"
    request = urllib.request.Request(url, headers={
        "Authorization": "Bearer " + token,
        "Host": ONELAKE_HOST})
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return json.loads(response.read())
    except Exception as exc:  # noqa: BLE001 — absence is the finding
        print(f"could not read {url}: {exc}", file=sys.stderr)
        return None


if __name__ == "__main__":
    sys.exit(main())
