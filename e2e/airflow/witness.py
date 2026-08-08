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
TENANT = "11111111-1111-1111-1111-111111111111"
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
        "client_id": "cccccccc-0000-0000-0000-000000000002",
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

def witness():
    with open("/results/passed", "w", encoding="utf-8") as result:
        result.write("real airflow 2.10.5")

with DAG("fabric_emulator_witness", start_date=datetime(2024, 1, 1), schedule=None, catchup=False) as dag:
    PythonOperator(task_id="write_witness", python_callable=witness)
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
print("PASS: real Apache Airflow 2.10.5 loaded, scheduled, and executed the Fabric DAG")
