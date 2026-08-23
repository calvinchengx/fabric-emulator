#!/usr/bin/env python3
"""Create a lakehouse, then give each engine one user's worth of work."""
import json
import subprocess
import sys
import urllib.parse
import urllib.request

FABRIC = "http://fabric-emulator"
ENTRA = "http://entra-emulator:8443"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"

form = urllib.parse.urlencode({
    "grant_type": "client_credentials",
    "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
    "client_secret": "daemon-app-secret",
    "scope": "https://api.fabric.microsoft.com/.default"}).encode()
req = urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form, method="POST",
                             headers={"Content-Type": "application/x-www-form-urlencoded"})
with urllib.request.urlopen(req, timeout=60) as r:
    token = json.loads(r.read())["access_token"]


def post(url, body):
    rq = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST",
                                headers={"Content-Type": "application/json",
                                         "Authorization": "Bearer " + token})
    with urllib.request.urlopen(rq, timeout=60) as r:
        return json.loads(r.read())


ws = post(f"{FABRIC}/v1/workspaces", {"displayName": "footprint-ws"})
lake = post(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"})
base = f"abfss://{ws['id']}@onelake.dfs.fabric.microsoft.com/{lake['id']}/Tables"

for host in ("sail", "sail2", "sail3", "sail4", "sail5"):
    rc = subprocess.run([sys.executable, "/workload.py", f"sc://{host}:50051",
                         f"{base}/t_{host}"]).returncode
    print(f"{host}: workload rc={rc}", flush=True)
