#!/usr/bin/env python3
"""Replay the exact extension 1.18.1 authoring protocol against the family."""
import json
import ssl
import urllib.error
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
PBI = "https://api.powerbi.com"
TENANT = "11111111-1111-1111-1111-111111111111"
SSL = ssl._create_unverified_context()


def request(method, url, token=None, payload=None, token_scheme="Bearer", headers=None):
    data = None
    if payload is not None:
        data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
    all_headers = {"Content-Type": "application/json", **(headers or {})}
    if token:
        all_headers["Authorization"] = f"{token_scheme} {token}"
    req = urllib.request.Request(url, data=data, headers=all_headers, method=method)
    try:
        with urllib.request.urlopen(req, context=SSL, timeout=30) as response:
            raw = response.read()
            ctype = response.headers.get("Content-Type", "")
            body = json.loads(raw) if raw and "json" in ctype else raw
            return response.status, response.headers, body
    except urllib.error.HTTPError as error:
        return error.code, error.headers, error.read()


contract = json.loads(open("/extension-contract.json", encoding="utf-8").read())
assert contract["extension"] == "SynapseVSCode.synapse" and contract["version"] == "1.18.1"
assert len(contract["routes"]) == 15

form = urllib.parse.urlencode(
    {
        "grant_type": "client_credentials",
        "client_id": "cccccccc-0000-0000-0000-000000000002",
        "client_secret": "daemon-app-secret",
        "scope": "https://analysis.windows.net/powerbi/api/.default",
    }
).encode()
with urllib.request.urlopen(
    urllib.request.Request(
        f"{ENTRA}/{TENANT}/oauth2/v2.0/token",
        data=form,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    ),
    timeout=20,
) as response:
    token = json.load(response)["access_token"]

status, _, cluster = request("PUT", PBI + "/spglobalservice/GetOrInsertClusterUrisByTenantLocation", token)
assert status == 200 and cluster["FixedClusterUri"] == PBI, cluster

_, _, workspace = request("POST", PBI + "/v1/workspaces", token, {"displayName": "VS Code e2e"})
wid, capacity = workspace["id"], workspace["capacityId"]
status, _, workspaces = request("GET", PBI + "/metadata/v201606/workspaces/", token)
assert status == 200 and workspaces[0]["id"] == wid and workspaces[0]["capacityObjectId"] == capacity

notebook = {
    "cells": [{"cell_type": "code", "metadata": {}, "source": ["print('v1')\n"], "outputs": [], "execution_count": None}],
    "metadata": {}, "nbformat": 4, "nbformat_minor": 5,
}
status, headers, artifact = request(
    "POST", PBI + f"/metadata/workspaces/{wid}/artifacts", token,
    {"artifactType": "SynapseNotebook", "displayName": "Extension notebook", "description": "e2e", "workloadPayload": json.dumps(notebook)},
)
assert status == 201 and artifact["artifactType"] == "SynapseNotebook" and headers["ETag"]
iid = artifact["objectId"]
_, _, artifacts = request("GET", PBI + f"/metadata/workspaces/{wid}/artifacts", token)
assert artifacts[0]["objectId"] == iid
_, _, selected = request("POST", PBI + "/metadata/datahub/V2/artifacts", token, {"supportedTypes": ["SynapseNotebook"]})
assert selected[0]["artifactObjectId"] == iid and selected[0]["workspaceObjectId"] == wid

_, _, mwc = request(
    "POST", PBI + "/metadata/v201606/generatemwctoken", token,
    {"capacityObjectId": capacity, "workspaceObjectId": wid, "workloadType": "Notebook", "artifactObjectIds": [iid]},
)
assert mwc["TargetUriHost"] == "api.powerbi.com" and mwc["Token"] == token
content_url = PBI + f"/webapi/capacities/{capacity}/workloads/Notebook/Data/Direct/api/workspaces/{wid}/artifacts/{iid}/content"
status, headers, downloaded = request("GET", content_url, token, token_scheme="MwcToken")
etag = headers["ETag"]
assert status == 200 and downloaded["cells"][0]["source"] == ["print('v1')\n"]
status, headers, body = request("HEAD", content_url, token, token_scheme="MwcToken")
assert status == 200 and headers["ETag"] == etag and body == b""

notebook["cells"][0]["source"] = ["print('v2')\n"]
status, headers, _ = request("PUT", content_url, token, json.dumps(notebook).encode(), "MwcToken", {"If-Match": etag})
assert status == 200 and headers["ETag"] != etag
status, _, _ = request("PUT", content_url, token, json.dumps(notebook).encode(), "MwcToken", {"If-Match": etag})
assert status == 412

resource = PBI + f"/webapi/capacities/{capacity}/workloads/Notebook/Data/Direct/api/workspaces/{wid}/artifacts/{iid}/filesystem/workdir/data.txt"
status, _, _ = request("PUT", resource, token, b"resource", "MwcToken", {"Content-Type": "application/octet-stream", "ms-filesystem-entry-type": "file"})
assert status == 200
status, _, raw = request("GET", resource, token, token_scheme="MwcToken")
assert status == 200 and raw == b"resource"
status, _, _ = request("DELETE", resource, token, token_scheme="MwcToken")
assert status == 200

status, _, updated = request("PATCH", PBI + f"/metadata/artifacts/{iid}", token, {"displayName": "Renamed notebook", "description": "published"})
assert status == 200 and updated["displayName"] == "Renamed notebook"
status, _, _ = request("DELETE", PBI + f"/metadata/artifacts/{iid}", token)
assert status == 200
print("PASS: Fabric Data Engineering VS Code 1.18.1 shared-backend/MWC authoring contract")
