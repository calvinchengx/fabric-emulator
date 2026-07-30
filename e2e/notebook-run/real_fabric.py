#!/usr/bin/env python3
"""Publish and execute the representative notebook on real Microsoft Fabric."""
import base64
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

FABRIC = "https://api.fabric.microsoft.com"
TENANT = os.environ["AZURE_TENANT_ID"]
CLIENT_ID = os.environ["AZURE_CLIENT_ID"]
CLIENT_SECRET = os.environ["AZURE_CLIENT_SECRET"]
WORKSPACE_NAME = os.environ["FABRIC_TEST_WORKSPACE"]

NOTEBOOK = '''# Fabric notebook source

# CELL ********************
df = spark.createDataFrame([(1, "a"), (2, "b"), (3, "c")], ["id", "name"])
df.createOrReplaceTempView("compat_events")
assert df.count() == 3

# CELL ********************
# MAGIC %%sql
# MAGIC SELECT name, id FROM compat_events ORDER BY id

# CELL ********************
rows = spark.sql("SELECT id, name FROM compat_events ORDER BY id").collect()
assert [(row.id, row.name) for row in rows] == [(1, "a"), (2, "b"), (3, "c")]
print("representative Fabric notebook: PASS")

# METADATA ********************
# META {
# META   "kernel_info": {
# META     "name": "synapse_pyspark"
# META   },
# META   "dependencies": {}
# META }
'''


def call(method, url, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if body is not None else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(request, timeout=60) as response:
        raw = response.read()
        return response.status, response.headers, json.loads(raw) if raw else {}


def token():
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET,
        "scope": "https://api.fabric.microsoft.com/.default",
    }).encode()
    request = urllib.request.Request(
        f"https://login.microsoftonline.com/{TENANT}/oauth2/v2.0/token",
        data=form,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.loads(response.read())["access_token"]


def poll(location, bearer, terminal, timeout=1800):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        _, headers, body = call("GET", location, token=bearer)
        status = body.get("status")
        if status in terminal:
            return status, body
        time.sleep(min(max(int(headers.get("Retry-After", "10")), 1), 30))
    raise TimeoutError(f"timed out polling {location}")


bearer = token()
_, _, workspaces = call("GET", f"{FABRIC}/v1/workspaces", token=bearer)
matches = [workspace for workspace in workspaces.get("value", []) if workspace["displayName"] == WORKSPACE_NAME]
if len(matches) != 1:
    raise RuntimeError(f"expected exactly one workspace named {WORKSPACE_NAME!r}, found {len(matches)}")
workspace_id = matches[0]["id"]
notebook_id = None

try:
    name = "fabric-emulator-parity-" + uuid.uuid4().hex[:8]
    status, headers, body = call("POST", f"{FABRIC}/v1/workspaces/{workspace_id}/notebooks", {
        "displayName": name,
        "description": "Temporary fabric-emulator release-qualification notebook",
        "definition": {
            "format": "fabricGitSource",
            "parts": [{
                "path": "notebook-content.py",
                "payloadType": "InlineBase64",
                "payload": base64.b64encode(NOTEBOOK.encode()).decode(),
            }],
        },
    }, bearer)
    if status == 201:
        notebook_id = body["id"]
    else:
        operation = headers["Location"]
        operation_status, operation_body = poll(operation, bearer, {"Succeeded", "Failed"})
        if operation_status != "Succeeded":
            raise RuntimeError(f"notebook creation failed: {operation_body}")
        _, _, result = call("GET", operation.rstrip("/") + "/result", token=bearer)
        notebook_id = result["id"]

    _, headers, _ = call(
        "POST",
        f"{FABRIC}/v1/workspaces/{workspace_id}/items/{notebook_id}/jobs/RunNotebook/instances",
        {},
        bearer,
    )
    job_status, job = poll(headers["Location"], bearer, {"Completed", "Failed", "Cancelled", "Deduped"})
    if job_status != "Completed":
        raise RuntimeError(f"real Fabric notebook failed: {job}")
    print(f"REAL-FABRIC NOTEBOOK: PASS ({notebook_id})", flush=True)
finally:
    if notebook_id:
        try:
            call("DELETE", f"{FABRIC}/v1/workspaces/{workspace_id}/notebooks/{notebook_id}", token=bearer)
        except urllib.error.HTTPError as error:
            print(f"warning: failed to delete qualification notebook: HTTP {error.code}", flush=True)
