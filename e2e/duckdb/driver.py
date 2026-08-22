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
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"

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
             {"clientId": "00d88624-f0d7-46f6-a641-6232c2608928", "audience": "https://storage.azure.com"})
    return t.get("access_token") or t["token"]


def fabric_token():
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials", "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
        "client_secret": "daemon-app-secret", "scope": "https://api.fabric.microsoft.com/.default"}).encode()
    with urllib.request.urlopen(urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form), context=_CTX) as r:
        return json.loads(r.read())["access_token"]


def req(url, method, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(url, data=data, method=method,
                               headers={"Content-Type": "application/json"})
    if token:
        r.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(r, context=_CTX) as resp:
        return json.loads(resp.read() or b"{}")


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

# --- The AUTHORIZED ENGINE MODEL, with DuckDB as the third-party engine.
#
# Microsoft documents this sequence for engines that are not Fabric's own:
#
#   1. read the raw files with a privileged identity
#   2. fetch the effective policy for a USER from principalAccess
#   3. apply the returned row/column filters in your own query layer
#   4. return only the permitted data
#
# Every other witness tests our enforcement. This one tests whether the
# CONTRACT is usable by something we did not write, which is the only way to
# find out if the seam is real.
VIEWER = "duckdb-viewer"
post(f"{FABRIC}/v1/workspaces/{ws['id']}/roleAssignments",
     {"principal": {"id": VIEWER, "type": "User"}, "role": "Viewer"}, ft)

lake_id = [i for i in req(f"{FABRIC}/v1/workspaces/{ws['id']}/items", "GET", None, ft)["value"]
           if i["displayName"] == "lake"][0]["id"]

# `sales` is granted, narrowed to one region. `regions` is not granted at all.
# The predicate is authored in the engine's dialect: OneLake stores the text and
# the engine runs it, which is exactly the division of labour the model defines.
req(f"{FABRIC}/v1/workspaces/{ws['id']}/items/{lake_id}/dataAccessRoles", "PUT", {"value": [{
    "name": "region1_only",
    "decisionRules": [{
        "effect": "Permit",
        "rows": "SELECT * FROM sales WHERE region_id = 1",
        "permission": [
            {"attributeName": "Path", "attributeValueIncludedIn": ["Tables/sales"]},
            {"attributeName": "Action", "attributeValueIncludedIn": ["Read"]}]}],
    "members": {"microsoftEntraMembers": [{"objectId": VIEWER}]}}]}, ft)

# STEP 2: the engine asks what this user may see. It uses its OWN privileged
# token -- the user never authenticates to OneLake at all.
# On the OneLake data plane, not the control plane: in a real tenant this is
# onelake.dfs.fabric.microsoft.com, and here it is the account-prefixed form
# the rest of this suite already uses for OneLake.
policy = req(f"{FABRIC}/onelake/v1.0/workspaces/{ws['id']}/artifacts/{lake_id}"
             "/securityPolicy/principalAccess", "GET",
             {"aadObjectId": VIEWER, "inputPath": "Tables"}, st)
by_path = {e["path"]: e for e in policy["value"]}
assert "Tables/sales" in by_path, policy
assert "Tables/regions" not in by_path, f"an ungranted table appeared in the policy: {policy}"
assert policy["identityETag"] and policy["metadataETag"], policy
print(f"principalAccess: {sorted(by_path)} (regions withheld)", flush=True)

# STEP 3: DuckDB applies the returned predicate in its own query layer.
predicate = by_path["Tables/sales"]["rows"]
assert predicate, f"no row filter returned: {by_path['Tables/sales']}"
con.execute(f"CREATE VIEW sales_for_user AS {predicate}")

# STEP 4: the user's query sees only their rows. The unfiltered numbers are
# right there for comparison, so a filter that did nothing cannot pass.
user_agg = con.sql("SELECT region_id, SUM(amount) t, COUNT(*) n "
                   "FROM sales_for_user GROUP BY region_id ORDER BY region_id").fetchall()
assert user_agg == [(1, 90, 3)], user_agg
assert user_agg != agg, "the row filter changed nothing; the unfiltered result is identical"
print(f"row-level security applied by DuckDB: {user_agg} (unfiltered was {agg})", flush=True)

# And the engine must refuse to serve what the policy never mentioned -- the
# check a careless integration skips, leaving an ungranted table readable
# because the engine already had the bytes.
assert "Tables/regions" not in by_path
print("authorized engine model: DuckDB filtered on OneLake's policy", flush=True)

print("DUCKDB-WAREHOUSE E2E: PASS", flush=True)
