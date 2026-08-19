#!/usr/bin/env python3
"""Entra -> Fabric item/files/jobs -> real Airflow scheduler/executor."""
import json
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

ENTRA = "http://entra-emulator:8443"
FABRIC = "https://fabric-emulator:9443"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
SSL = ssl._create_unverified_context()


def request(method, url, token=None, payload=None, content_type="application/json"):
    data = None
    if payload is not None:
        data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
    headers = {"Content-Type": content_type}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, context=SSL, timeout=30) as response:
            raw = response.read()
            return response.status, response.headers, json.loads(raw) if raw else None
    except urllib.error.HTTPError as error:
        raise RuntimeError(f"{method} {url}: {error.code} {error.read().decode()}") from error


form = urllib.parse.urlencode(
    {
        "grant_type": "client_credentials",
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
        "client_secret": "daemon-app-secret",
        "scope": "https://api.fabric.microsoft.com/.default",
    }
).encode()
req = urllib.request.Request(
    f"{ENTRA}/{TENANT}/oauth2/v2.0/token",
    data=form,
    headers={"Content-Type": "application/x-www-form-urlencoded"},
)
with urllib.request.urlopen(req, timeout=20) as response:
    token = json.load(response)["access_token"]

_, _, workspace = request("POST", f"{FABRIC}/v1/workspaces", token, {"displayName": "Airflow e2e"})
_, _, item = request(
    "POST",
    f"{FABRIC}/v1/workspaces/{workspace['id']}/apacheAirflowJobs",
    token,
    {"displayName": "Real Airflow"},
)

dag = b'''from airflow import DAG
from airflow.operators.python import PythonOperator
from datetime import datetime
import json, os, time

# THREE TASKS THAT OVERLAP, not one that runs.
#
# This DAG had a single task, and a single task cannot tell CeleryExecutor from
# SequentialExecutor -- which is exactly how the sidecar ran Sequential against
# a green witness for as long as it did. Fabric forbids overriding
# AIRFLOW__CORE__EXECUTOR and its default is Celery, so an emulator that
# serialises is offering behaviour no Fabric user can have, and the witness has
# to be able to SEE that.
#
# Each branch records when it started and stopped. Under a parallel executor
# their windows overlap; under Sequential they cannot, because the second task
# does not begin until the first returns. The assertion is made on the
# consumer side, from these files.
def branch(name):
    def run():
        started = time.time()
        time.sleep(3)
        with open(f"/results/{name}.json", "w", encoding="utf-8") as fh:
            json.dump({"started": started, "ended": time.time()}, fh)
    return run

with DAG("fabric_emulator_witness", start_date=datetime(2024, 1, 1), schedule=None, catchup=False) as dag:
    fan = [PythonOperator(task_id=f"branch_{i}", python_callable=branch(f"branch_{i}"))
           for i in range(3)]

    def witness():
        with open("/results/passed", "w", encoding="utf-8") as result:
            result.write("real airflow 2.10.5")

    PythonOperator(task_id="write_witness", python_callable=witness) << fan
'''
files = f"{FABRIC}/v1/workspaces/{workspace['id']}/apacheAirflowJobs/{item['id']}/files"
request("PUT", files + "/dags/witness.py?beta=true", token, dag, "application/octet-stream")
_, _, listed = request("GET", files + "?beta=true", token)
assert listed["value"] == [{"filePath": "dags/witness.py", "sizeInBytes": len(dag)}], listed

status, headers, _ = request(
    "POST",
    f"{FABRIC}/v1/workspaces/{workspace['id']}/items/{item['id']}/jobs/instances?jobType=Run",
    token,
    {"executionData": {"dagId": "fabric_emulator_witness", "conf": {"source": "e2e"}}},
)
assert status == 202
location = headers["Location"]
for _ in range(180):
    _, _, job = request("GET", location.replace("fabric-emulator", "fabric-emulator"), token)
    if job["status"] in ("Completed", "Failed"):
        break
    time.sleep(1)
else:
    raise AssertionError("Airflow job did not reach a terminal state")

assert job["status"] == "Completed", job
assert Path("/results/passed").read_text() == "real airflow 2.10.5"

# THE EXECUTOR IS PARALLEL, witnessed rather than configured. Three independent
# branches each sleep 3s and record their window. Under CeleryExecutor -- which
# is what Fabric runs and forbids changing -- those windows overlap. Under
# SequentialExecutor they are strictly ordered, because the next task does not
# start until the previous returns, and the whole DAG takes 3x as long.
#
# Asserting the CONFIG says Celery would prove only that a string is in a file.
# This proves the scheduler actually handed work to a worker concurrently.
windows = [json.loads(Path(f"/results/branch_{i}.json").read_text()) for i in range(3)]
span = max(w["ended"] for w in windows) - min(w["started"] for w in windows)
serial = sum(w["ended"] - w["started"] for w in windows)
assert span < serial * 0.75, (
    f"the three branches did not overlap: wall span {span:.1f}s against "
    f"{serial:.1f}s of work. That is SequentialExecutor's signature -- a "
    f"parallel executor finishes all three in roughly one branch's time. "
    f"windows={windows}"
)
print(
    f"PASS: real Apache Airflow 2.10.5 loaded, scheduled and executed the Fabric DAG, "
    f"and ran 3 branches CONCURRENTLY ({span:.1f}s wall for {serial:.1f}s of work)"
)
