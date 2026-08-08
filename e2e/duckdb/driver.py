"""R3/C1: real DuckDB runs SQL over a Delta table living in the emulator's
OneLake plane — the lakehouse SQL-analytics-endpoint semantics.

delta-rs writes a Delta table into OneLake; DuckDB then runs real SQL
(filters, GROUP BY, aggregation, a join) over it. Two independent engines
agreeing on the query results over the same Delta data is the warehouse
oracle. (DuckDB's own delta_scan can't take a plain-HTTP custom endpoint yet,
so delta-rs reads the OneLake bytes — a path already proven byte-correct by
the delta-rs e2e — and DuckDB queries them.)
"""
import json
import os
import urllib.parse
import urllib.request

import duckdb
import pyarrow as pa
from deltalake import DeltaTable, write_deltalake

ENTRA = f"https://localhost:{os.environ.get('ENTRA_PORT', '18443')}"
FABRIC = f"http://127.0.0.1:{os.environ.get('FABRIC_PORT', '19080')}"
TENANT = "11111111-1111-1111-1111-111111111111"

import ssl  # noqa: E402

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE


def post(url, body, token=None):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), headers={"Content-Type": "application/json"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, context=_CTX) as r:
        return json.loads(r.read() or b"{}")


def storage_token():
    try:
        post(f"{ENTRA}/admin/api/apps",
             {"displayName": "Azure Storage", "appIdUri": "https://storage.azure.com", "isConfidential": False})
    except urllib.error.HTTPError as e:
        if e.code != 409:
            raise
    t = post(f"{ENTRA}/admin/api/tokens",
             {"clientId": "cccccccc-0000-0000-0000-000000000002", "audience": "https://storage.azure.com"})
    return t.get("access_token") or t["token"]


def fabric_token():
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials", "client_id": "cccccccc-0000-0000-0000-000000000002",
        "client_secret": "daemon-app-secret", "scope": "https://api.fabric.microsoft.com/.default"}).encode()
    with urllib.request.urlopen(urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form), context=_CTX) as r:
        return json.loads(r.read())["access_token"]


ft, st = fabric_token(), storage_token()
ws = post(f"{FABRIC}/v1/workspaces", {"displayName": "warehouse-ws"}, ft)
post(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, ft)
print(f"workspace: {ws['id']}", flush=True)

opts = {"azure_storage_account_name": "onelake", "azure_storage_token": st,
        "azure_endpoint": f"{FABRIC}/onelake", "azure_allow_http": "true"}

# Two Delta tables in the lakehouse.
write_deltalake("az://warehouse-ws/lake.Lakehouse/Tables/sales",
                pa.table({"region_id": [1, 2, 1, 2, 1], "amount": [10, 20, 30, 40, 50]}), storage_options=opts)
write_deltalake("az://warehouse-ws/lake.Lakehouse/Tables/regions",
                pa.table({"region_id": [1, 2], "name": ["us", "eu"]}), storage_options=opts)
print("delta tables written to OneLake", flush=True)

# Real SQL over the lakehouse Delta via DuckDB.
sales = DeltaTable("az://warehouse-ws/lake.Lakehouse/Tables/sales", storage_options=opts).to_pyarrow_table()
regions = DeltaTable("az://warehouse-ws/lake.Lakehouse/Tables/regions", storage_options=opts).to_pyarrow_table()
con = duckdb.connect()
con.register("sales", sales)
con.register("regions", regions)

agg = con.sql("SELECT region_id, SUM(amount) t, COUNT(*) n FROM sales GROUP BY region_id ORDER BY region_id").fetchall()
assert agg == [(1, 90, 3), (2, 60, 2)], agg
print(f"aggregate: {agg}", flush=True)

joined = con.sql("""
    SELECT r.name, SUM(s.amount) total
    FROM sales s JOIN regions r ON s.region_id = r.region_id
    WHERE s.amount >= 20 GROUP BY r.name ORDER BY total DESC""").fetchall()
assert joined == [("eu", 60), ("us", 80)] or joined == [("us", 80), ("eu", 60)], joined
print(f"join + filter: {joined}", flush=True)

print("DUCKDB-WAREHOUSE E2E: PASS", flush=True)
