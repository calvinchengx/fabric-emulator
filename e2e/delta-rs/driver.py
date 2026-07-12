"""A1: the real delta-rs engine (Python `deltalake`) writes and reads a
Delta table through fabric-emulator's OneLake Blob surface, authenticated
with an entra-emulator Storage-audience token.

object_store (delta-rs's storage layer) is pointed at the emulator via its
endpoint override; the account-prefixed path form /onelake/{workspace}/…
reaches the Blob dialect on any host. Delta's _delta_log commits exercise
put-if-absent (If-None-Match: *) — the correctness primitive R0 added.
"""
import json
import os
import urllib.request
import urllib.parse

import pyarrow as pa
from deltalake import DeltaTable, write_deltalake

ENTRA = f"https://localhost:{os.environ.get('ENTRA_PORT', '18443')}"
FABRIC = f"http://127.0.0.1:{os.environ.get('FABRIC_PORT', '19080')}"
TENANT = "11111111-1111-1111-1111-111111111111"

import ssl
INSECURE = ssl.create_default_context()
INSECURE.check_hostname = False
INSECURE.verify_mode = ssl.CERT_NONE


def post_json(url, body, token=None, ctx=None):
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, context=ctx) as r:
        return json.loads(r.read() or b"{}")


def entra_token(scope=None, audience=None):
    if audience:  # forge: arbitrary audience (Storage)
        tok = post_json(f"{ENTRA}/admin/api/tokens",
                        {"clientId": "cccccccc-0000-0000-0000-000000000002", "audience": audience},
                        ctx=INSECURE)
        return tok.get("access_token") or tok["token"]
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": "cccccccc-0000-0000-0000-000000000002",
        "client_secret": "daemon-app-secret",
        "scope": scope,
    }).encode()
    req = urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form)
    with urllib.request.urlopen(req, context=INSECURE) as r:
        return json.loads(r.read())["access_token"]


fabric_token = entra_token(scope="https://api.fabric.microsoft.com/.default")
storage_token = entra_token(audience="https://storage.azure.com")

# Control plane: workspace + lakehouse.
ws = post_json(f"{FABRIC}/v1/workspaces", {"displayName": "deltaws"}, fabric_token)
post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, fabric_token)
print(f"workspace: {ws['id']}")

url = "az://deltaws/lake.Lakehouse/Tables/events"
storage_options = {
    "azure_storage_account_name": "onelake",
    "azure_storage_token": storage_token,
    "azure_endpoint": f"{FABRIC}/onelake",
    "azure_allow_http": "true",
}

table = pa.table({"id": pa.array([1, 2, 3], pa.int64()), "name": ["a", "b", "c"]})
write_deltalake(url, table, storage_options=storage_options)
print("delta write: OK")

dt = DeltaTable(url, storage_options=storage_options)
assert dt.version() == 0, dt.version()
got = dt.to_pyarrow_table().sort_by("id")
assert got.column("id").to_pylist() == [1, 2, 3], got
assert got.column("name").to_pylist() == ["a", "b", "c"], got
print("delta read-back: OK (3 rows, version 0)")

# Append: a second real Delta commit (put-if-absent on 00000001.json).
write_deltalake(url, pa.table({"id": pa.array([4], pa.int64()), "name": ["d"]}),
                mode="append", storage_options=storage_options)
dt = DeltaTable(url, storage_options=storage_options)
assert dt.version() == 1, dt.version()
assert dt.to_pyarrow_table().num_rows == 4
print("delta append: OK (version 1, 4 rows)")

# The same files are visible through the DFS surface — one storage substrate.
req = urllib.request.Request(
    f"{FABRIC}/{ws['id']}?resource=filesystem&recursive=true",
    headers={"Authorization": "Bearer " + storage_token, "Host": "onelake.dfs.fabric.microsoft.com"},
)
with urllib.request.urlopen(req) as r:
    listing = json.loads(r.read())
names = [p["name"] for p in listing["paths"]]
assert any("_delta_log/00000000000000000000.json" in n for n in names), names
assert any(n.endswith(".parquet") for n in names), names
print(f"DFS sees the Delta table: {len(names)} paths incl _delta_log + parquet")

print("DELTA-RS E2E: PASS")
