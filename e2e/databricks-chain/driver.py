#!/usr/bin/env python3
"""fabric-emulator submits a Databricks activity to databricks-emulator, and
the official Databricks SDK confirms the job really landed in the workspace.

WHAT THIS IS FOR. `FABRIC_DATABRICKS_URL` is documented in the README, in
docs/04-configuration.md (which names databricks-emulator as the target), in
the v0.25.0 notes and in internal/api/databricksremote.go — and appeared in no
workflow anywhere. The parity row grades the Databricks activities
"🟢 Real (notebook + python, local or FABRIC_DATABRICKS_URL)"; the local half
ran on every push and the remote half had never executed. This is not primarily
a witness, it is an unexercised code path getting exercised.

WHY THE SDK RATHER THAN A REST CALL. `databricks-sdk` is a real third-party
client with its own expectations: it parses the workspace's answers into its
own dataclasses. If fabric's submission produced a job the workspace recorded
in a shape Databricks does not use, the SDK would fail to read it back — which
a hand-written `requests.get` asserting the fields we happen to send would not.
That is the difference between a client that can disagree with us and one that
cannot.

WHAT IS ASSERTED. Both ends, because either alone is satisfiable by a lie:
  * the pipeline activity reaches Completed AND its output carries `job_id`
    and `run_id`, which only the remote branch can produce;
  * the SDK, talking to databricks-emulator directly, finds a run whose task
    carries the notebook we submitted.
An activity that quietly fell back to local execution would satisfy the first.
"""
from __future__ import annotations

import base64
import json
import os
import sys
import time

import requests
import urllib3
from databricks.sdk import WorkspaceClient

urllib3.disable_warnings()  # both emulators serve self-signed leaves

FABRIC = os.environ["FABRIC"]
DATABRICKS = os.environ["DATABRICKS"]
ENTRA = os.environ["ENTRA"]
PAT = os.environ["DATABRICKS_PAT"]
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"

S = requests.Session()
S.verify = False


def fail(msg: str) -> None:
    sys.exit(f"FAIL: {msg}")


def token(scope: str = "https://api.fabric.microsoft.com/.default") -> str:
    """OneLake needs the STORAGE audience and rejects the Fabric one outright
    (internal/onelake/onelake_test.go pins that rejection). Reusing the Fabric
    bearer for the seed writes is why the first run of this suite failed with
    `no file at Files/jobs/etl.py` — the writes were refused, and the symptom
    only surfaced later as an activity error about a missing file."""
    r = S.post(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", timeout=60, data={
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET, "scope": scope})
    r.raise_for_status()
    return r.json()["access_token"]


def main() -> int:
    for _ in range(60):
        try:
            if S.get(f"{FABRIC}/health", timeout=5).ok:
                break
        except requests.RequestException:
            pass
        time.sleep(2)
    else:
        fail("fabric never came up")

    h = {"Authorization": f"Bearer {token()}"}
    print("-- 1. workspace + lakehouse, and the notebook file the job will run")
    ws = S.post(f"{FABRIC}/v1/workspaces", headers=h, timeout=60,
                json={"displayName": "dbx-chain"}).json()["id"]
    lh = S.post(f"{FABRIC}/v1/workspaces/{ws}/items", headers=h, timeout=60,
                json={"displayName": "lake", "type": "Lakehouse"}).json()["id"]

    # A notebook whose body is unmistakable, so what ran can be identified.
    code = "print('from the databricks chain')\n"
    sh = {"Authorization": f"Bearer {token('https://storage.azure.com/.default')}"}
    base = f"https://onelake.dfs.fabric.microsoft.com/{ws}/{lh}/Files/jobs/etl.py"
    S.put(f"{base}?resource=file", headers=sh, timeout=60)
    S.patch(f"{base}?action=append&position=0", headers=sh, timeout=60,
            data=code.encode())
    S.patch(f"{base}?action=flush&position={len(code)}", headers=sh, timeout=60)

    # Assert the PRECONDITION rather than letting a failed seed surface later as
    # an activity error about a missing file. A suite that cannot tell "the
    # fixture never landed" from "the feature is broken" wastes the run it fails.
    back = S.get(base, headers=sh, timeout=60)
    if back.status_code != 200 or back.text.strip() != code.strip():
        fail(f"the seed did not land in OneLake ({back.status_code}): "
             f"{back.text[:200]!r} — the chain was never exercised")
    print(f"   workspace {ws[:8]}, lakehouse {lh[:8]}, etl.py seeded and read back")

    print("-- 2. a pipeline whose DatabricksSparkPython task submits REMOTELY")
    pid = S.post(f"{FABRIC}/v1/workspaces/{ws}/items", headers=h, timeout=60,
                 json={"displayName": "dbx-pipe", "type": "DataPipeline"}).json()["id"]
    content = {"properties": {"activities": [
        {"name": "Dbx", "type": "DatabricksSparkPython", "typeProperties": {
            "pythonFile": f"{lh}/Files/jobs/etl.py",
            "parameters": ["--mode", "chain"]}}]}}
    S.post(f"{FABRIC}/v1/workspaces/{ws}/items/{pid}/updateDefinition", headers=h,
           timeout=60, json={"definition": {"parts": [{
               "path": "pipeline-content.json",
               "payload": base64.b64encode(json.dumps(content).encode()).decode(),
               "payloadType": "InlineBase64"}]}})
    S.post(f"{FABRIC}/v1/workspaces/{ws}/items/{pid}/jobs/instances?jobType=Pipeline",
           headers=h, timeout=60, json={})

    inst = None
    for _ in range(120):
        runs = S.get(f"{FABRIC}/v1/workspaces/{ws}/items/{pid}/jobs/instances",
                     headers=h, timeout=60).json().get("value") or []
        inst = next((r for r in runs if r.get("status") in ("Completed", "Failed")), None)
        if inst:
            break
        time.sleep(1)
    if not inst:
        fail("the pipeline never reached a terminal state")
    detail = S.post(f"{FABRIC}/v1/workspaces/{ws}/items/{pid}/jobs/instances/"
                    f"{inst['id']}/queryactivityruns", headers=h, timeout=60,
                    json={}).json()
    acts = detail.get("value") or []
    if inst["status"] != "Completed":
        fail(f"pipeline {inst['status']}: {acts}")
    out = next((a.get("output") or {} for a in acts if a.get("activityName") == "Dbx"), {})

    executed_by = str(out.get("executedBy", ""))
    # `job_id` / `run_id` are the discriminator, NOT the executedBy text.
    #
    # The first version of this check tested `"databricks" in executedBy` and
    # passed against `"the emulator's Spark engine, not a Databricks cluster"` —
    # a substring match satisfied by a sentence saying the opposite. Worse, that
    # string is CORRECT on the remote path: databricksremote.go passes
    # `executedBy` through from the workspace, and databricks-emulator honestly
    # reports that its Spark engine ran the job, because it did. So the text
    # cannot distinguish local from remote in either direction.
    #
    # `job_id` and `run_id` can: only the remote branch emits them
    # (databricksremote.go:102-104), because only it created a job on a
    # workspace. The local branch has no ids to report.
    for key in ("job_id", "run_id"):
        if key not in out:
            fail(f"the activity output has no {key!r}, so it did NOT submit to a "
                 f"workspace — this ran on the local Spark agent: {out}")
    print(f"   pipeline Completed remotely: job_id={out['job_id']} "
          f"run_id={out['run_id']}; executedBy={executed_by[:60]!r}")

    print("-- 3. the official Databricks SDK reads the workspace side")
    # The SDK brings its own HTTP stack, so `S.verify = False` above does not
    # reach it — and disabling verification would be the wrong fix anyway. Trust
    # the PEM databricks-emulator PERSISTED, which its own tlsclient documents
    # as the way to trust a sibling. Harvesting the leaf live was the first
    # attempt and could never verify: the cert's SANs are localhost,
    # databricks-emulator and *.azuredatabricks.net, so the service had to be
    # NAMED databricks-emulator for the hostname to match at all.
    ca = os.environ.get("DBX_CA", "")
    if not ca or not os.path.isfile(ca):
        fail(f"databricks-emulator's persisted cert was not mounted at {ca!r}")
    os.environ["REQUESTS_CA_BUNDLE"] = ca
    os.environ["SSL_CERT_FILE"] = ca
    print(f"   trusting the persisted cert at {ca}")

    w = WorkspaceClient(host=DATABRICKS, token=PAT)
    me = w.current_user.me()
    print(f"   SDK authenticated as {me.user_name}")

    jobs = list(w.jobs.list())
    if not jobs:
        fail("fabric reported success but databricks-emulator has NO jobs — "
             "the submission never arrived")
    all_runs = []
    for j in jobs:
        all_runs.extend(list(w.jobs.list_runs(job_id=j.job_id)))
    if not all_runs:
        fail(f"{len(jobs)} job(s) created but no runs — run-now never happened")

    # A run must have SUCCEEDED, not merely have been created — a workspace
    # that recorded the submission and never ran it would satisfy the count.
    states = [(r.state.result_state.value if r.state and r.state.result_state else None)
              for r in all_runs]
    if "SUCCESS" not in states:
        fail(f"no run SUCCEEDED; states = {states}")

    # And the job must name OUR file. `list_runs` returns a SUMMARY — its tasks
    # carry task_key but not the spark_python_task detail — so the file name is
    # read from the job DEFINITION, which is also the stronger claim: it proves
    # what fabric submitted, not merely that something ran.
    defs = json.dumps([w.jobs.get(job_id=j.job_id).as_dict() for j in jobs])
    if "etl.py" not in defs:
        fail(f"no job definition references etl.py, so fabric submitted something "
             f"other than the file it was pointed at: {defs[:400]}")
    print(f"   SDK sees {len(jobs)} job(s), {len(all_runs)} run(s), one SUCCESS, "
          f"and the job definition names etl.py")

    print("\nDATABRICKS CHAIN E2E: PASS — fabric submitted to databricks-emulator, "
          "and Databricks' own SDK confirms the run from the workspace side")
    return 0


if __name__ == "__main__":
    sys.exit(main())
