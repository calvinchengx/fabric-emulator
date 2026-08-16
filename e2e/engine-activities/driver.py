#!/usr/bin/env python3
"""The three pipeline activities that EXECUTE code, against a real Spark agent.

Custom (ADF Batch), HDInsightSpark and SparkJobDefinition were each witnessed
only by Go tests driving a FAKE agent that records the statement it was handed.
That proves dispatch and nothing else: a fake cannot tell you the command ran,
what it printed, or that a failure fails the activity. Here the agent is the
shipped one with Sail behind it, so each activity is judged by what came back.

WHAT EACH ONE ASSERTS, and the negative half in every case:

  Custom            stdout carries a marker the URL cannot imply, exit code 0 —
                    and a NON-ZERO exit must FAIL the activity. Without that
                    second half the row is satisfied by an activity that runs
                    the command and ignores its verdict, which is the shape the
                    parity doc calls out as the reason it was off-by-default.

  HDInsightSpark    the entry file is read from OneLake and its code runs: the
                    marker it prints is unique to the file's CONTENTS, so an
                    activity that submitted the path without reading it cannot
                    produce it.

  SparkJobDefinition  same, through the item-job surface rather than a path:
                    the SJD's own `main.py` part must be what executed.
"""
from __future__ import annotations

import base64
import json
import os
import sys
import time

import requests
import urllib3

urllib3.disable_warnings()

FABRIC = os.environ["FABRIC"]
ENTRA = os.environ["ENTRA"]
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"

S = requests.Session()
S.verify = False


def fail(msg: str) -> None:
    sys.exit(f"FAIL: {msg}")


def token(scope: str = "https://api.fabric.microsoft.com/.default") -> str:
    r = S.post(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", timeout=60, data={
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET, "scope": scope})
    r.raise_for_status()
    return r.json()["access_token"]


H: dict[str, str] = {}
SH: dict[str, str] = {}


def post(path: str, body):
    r = S.post(f"{FABRIC}{path}", headers=H, json=body, timeout=120)
    assert r.status_code in (200, 201, 202), f"{path} -> {r.status_code} {r.text[:300]}"
    return r.json() if r.content and r.headers.get("content-type", "").startswith("application/json") else {}


def get(path: str):
    r = S.get(f"{FABRIC}{path}", headers=H, timeout=120)
    r.raise_for_status()
    return r.json()


def seed(ws: str, item: str, rel: str, body: str) -> None:
    """Write a file into OneLake. STORAGE audience, not Fabric — OneLake rejects
    the Fabric one, and a refused seed surfaces later as a confusing 'no file'
    error from whichever activity was pointed at it."""
    base = f"https://onelake.dfs.fabric.microsoft.com/{ws}/{item}/{rel}"
    S.put(f"{base}?resource=file", headers=SH, timeout=60)
    S.patch(f"{base}?action=append&position=0", headers=SH, timeout=60, data=body.encode())
    S.patch(f"{base}?action=flush&position={len(body.encode())}", headers=SH, timeout=60)
    back = S.get(base, headers=SH, timeout=60)
    if back.status_code != 200 or back.text.strip() != body.strip():
        fail(f"seed did not land at {rel} ({back.status_code}) — nothing below would mean anything")


def run_pipeline(ws: str, name: str, activities: list, want: str = "Completed") -> list:
    pid = post(f"/v1/workspaces/{ws}/items",
               {"displayName": name, "type": "DataPipeline"})["id"]
    payload = base64.b64encode(
        json.dumps({"properties": {"activities": activities}}).encode()).decode()
    post(f"/v1/workspaces/{ws}/items/{pid}/updateDefinition",
         {"definition": {"parts": [{"path": "pipeline-content.json",
                                    "payload": payload,
                                    "payloadType": "InlineBase64"}]}})
    post(f"/v1/workspaces/{ws}/items/{pid}/jobs/instances?jobType=Pipeline", {})
    inst = None
    for _ in range(240):
        got = get(f"/v1/workspaces/{ws}/items/{pid}/jobs/instances").get("value") or []
        inst = next((r for r in got if r.get("status") in ("Completed", "Failed")), None)
        if inst:
            break
        time.sleep(1)
    if not inst:
        fail(f"{name}: never reached a terminal state")
    runs = (post(f"/v1/workspaces/{ws}/items/{pid}/jobs/instances/{inst['id']}"
                 f"/queryactivityruns", {}) or {}).get("value") or []
    if inst["status"] != want:
        fail(f"{name}: job {inst['status']}, want {want}; runs={runs}")
    return runs


def output_of(runs: list, activity: str) -> dict:
    for r in runs:
        if r.get("activityName") == activity:
            return r.get("output") or {}
    raise SystemExit(
        f"FAIL: {activity!r} not in {[r.get('activityName') for r in runs]}")


def main() -> int:
    for _ in range(90):
        try:
            if S.get(f"{FABRIC}/health", timeout=5).ok:
                break
        except requests.RequestException:
            pass
        time.sleep(2)
    else:
        fail("fabric never came up")
    H.update({"Authorization": f"Bearer {token()}"})
    SH.update({"Authorization": f"Bearer {token('https://storage.azure.com/.default')}"})

    ws = post("/v1/workspaces", {"displayName": "engine-activities"})["id"]
    lh = post(f"/v1/workspaces/{ws}/items",
              {"displayName": "lake", "type": "Lakehouse"})["id"]
    print(f"-- workspace {ws[:8]}, lakehouse {lh[:8]}")

    print("-- Custom (ADF Batch): the command runs, and a non-zero exit FAILS it")
    runs = run_pipeline(ws, "pl-custom", [
        {"name": "Batch", "type": "Custom",
         "typeProperties": {"command": "echo engine-activities-marker"}}])
    out = output_of(runs, "Batch")
    if out.get("exitCode") != 0:
        fail(f"exitCode = {out.get('exitCode')!r}, want 0: {out}")
    if "engine-activities-marker" not in str(out.get("stdout", "")):
        fail(f"the command's stdout never came back: {out}")
    print("   exitCode=0, stdout carries the marker")

    # The half that carries the row. An activity that runs the command and
    # ignores its verdict passes everything above.
    run_pipeline(ws, "pl-custom-fail", [
        {"name": "Batch", "type": "Custom",
         "typeProperties": {"command": "exit 3"}}], want="Failed")
    print("   a non-zero exit fails the activity")

    print("-- HDInsightSpark: the outcome depends on the ENTRY FILE's contents")
    # Unlike Custom, this activity's output carries rootPath / entryFilePath /
    # arguments / executedBy and NOT the program's stdout, so a marker printed by
    # the file is not observable from here. The assertion that IS available is
    # stronger anyway: run the same activity twice, changing only the FILE, and
    # require the verdict to follow it. A submission that never executed the code
    # cannot fail on its contents.
    seed(ws, lh, "Files/jobs/etl.py", "print('ok')\n")
    run_pipeline(ws, "pl-hdi", [
        {"name": "Spark", "type": "HDInsightSpark", "typeProperties": {
            "rootPath": f"{lh}/Files/jobs", "entryFilePath": "etl.py",
            "arguments": ["--date", "2026-08-16"]}}])
    seed(ws, lh, "Files/jobs/boom.py", "raise SystemExit('entry file failed')\n")
    run_pipeline(ws, "pl-hdi-fail", [
        {"name": "Spark", "type": "HDInsightSpark", "typeProperties": {
            "rootPath": f"{lh}/Files/jobs", "entryFilePath": "boom.py"}}],
        want="Failed")
    print("   a good entry file succeeds and a raising one FAILS: the code runs")

    print("-- SparkJobDefinition: the item's own main.py executes")
    sjd = post(f"/v1/workspaces/{ws}/items", {
        "displayName": "job", "type": "SparkJobDefinition",
        "definition": {"parts": [
            {"path": "SparkJobDefinitionV1.json",
             "payload": base64.b64encode(json.dumps({
                 "executableFile": "main.py",
                 "defaultLakehouseArtifactId": lh,
                 "defaultLakehouseWorkspaceId": ws}).encode()).decode(),
             "payloadType": "InlineBase64"},
            {"path": "main.py",
             "payload": base64.b64encode(b"print('sjd-main-ran')\n").decode(),
             "payloadType": "InlineBase64"},
        ]}})
    sjd_id = sjd.get("id")
    if not sjd_id:
        for it in (get(f"/v1/workspaces/{ws}/items").get("value") or []):
            if it.get("displayName") == "job":
                sjd_id = it["id"]
    if not sjd_id:
        fail("the SparkJobDefinition item was never created")
    run_pipeline(ws, "pl-sjd", [
        {"name": "Step", "type": "SparkJobDefinition", "typeProperties": {
            "sparkJobDefinitionId": sjd_id, "workspaceId": ws}}])

    # Same shape as HDInsight: a SECOND definition whose main.py raises must
    # fail the activity. Two items differing only in their `main.py` part, with
    # opposite verdicts, is what proves the part is executed rather than named.
    bad = post(f"/v1/workspaces/{ws}/items", {
        "displayName": "job-bad", "type": "SparkJobDefinition",
        "definition": {"parts": [
            {"path": "SparkJobDefinitionV1.json",
             "payload": base64.b64encode(json.dumps({
                 "executableFile": "main.py",
                 "defaultLakehouseArtifactId": lh,
                 "defaultLakehouseWorkspaceId": ws}).encode()).decode(),
             "payloadType": "InlineBase64"},
            {"path": "main.py",
             "payload": base64.b64encode(b"raise SystemExit('sjd failed')\n").decode(),
             "payloadType": "InlineBase64"},
        ]}})
    bad_id = bad.get("id")
    if not bad_id:
        for it in (get(f"/v1/workspaces/{ws}/items").get("value") or []):
            if it.get("displayName") == "job-bad":
                bad_id = it["id"]
    run_pipeline(ws, "pl-sjd-fail", [
        {"name": "Step", "type": "SparkJobDefinition", "typeProperties": {
            "sparkJobDefinitionId": bad_id, "workspaceId": ws}}], want="Failed")
    print("   a good main.py succeeds and a raising one FAILS: the part runs")

    print("\nENGINE ACTIVITIES E2E: PASS — Custom, HDInsightSpark and "
          "SparkJobDefinition each ran real code on the shipped agent")
    return 0


if __name__ == "__main__":
    sys.exit(main())
