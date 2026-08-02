#!/usr/bin/env python3
"""Composed parity witness over one physical OneLake Delta table."""
import base64
import json
import os
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

import duckdb
import mlflow
from mlflow import MlflowClient
from pyspark.sql import SparkSession

FABRIC = os.environ["FABRIC"]
ENTRA = os.environ["ENTRA"]
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
CLIENT_SECRET = "daemon-app-secret"
PBI = "https://analysis.windows.net/powerbi/api"


def request(method, url, body=None, token=None, form=False):
    headers, data = {}, None
    if body is not None:
        if form:
            data = urllib.parse.urlencode(body).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        else:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=60) as response:
        raw = response.read()
        return response.status, response.headers, json.loads(raw) if raw else {}


def token(scope):
    return request("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET, "scope": scope,
    }, form=True)[2]["access_token"]


def create_item(workspace_id, body, fabric_token):
    _, headers, item = request("POST", f"{FABRIC}/v1/workspaces/{workspace_id}/items", body, fabric_token)
    operation = headers.get("x-ms-operation-id")
    if not operation:
        return item
    for _ in range(100):
        state = request("GET", f"{FABRIC}/v1/operations/{operation}", token=fabric_token)[2]
        if state.get("status") == "Succeeded":
            return request("GET", f"{FABRIC}/v1/operations/{operation}/result", token=fabric_token)[2]
        if state.get("status") == "Failed":
            raise RuntimeError(state)
        time.sleep(0.1)
    raise RuntimeError("item operation timed out")


fabric_token = token("https://api.fabric.microsoft.com/.default")
workspace = request("POST", f"{FABRIC}/v1/workspaces", {"displayName": "science-loop"}, fabric_token)[2]
lakehouse = request("POST", f"{FABRIC}/v1/workspaces/{workspace['id']}/lakehouses", {"displayName": "lake"}, fabric_token)[2]
print(f"workspace={workspace['id']} lakehouse={lakehouse['id']}", flush=True)

# Spark SQL -> Delta -> OneLake. Sail is the real Spark Connect execution engine.
for attempt in range(30):
    try:
        spark = SparkSession.builder.remote(os.environ["SPARK_REMOTE"]).getOrCreate()
        spark.sql("SELECT 1").collect()
        break
    except Exception:
        if attempt == 29:
            raise
        time.sleep(2)
delta_path = f"abfs://{workspace['id']}@onelake.dfs.fabric.microsoft.com/{lakehouse['id']}/Tables/sales"
sales = spark.sql("""
    SELECT * FROM VALUES
      (1, 'us', 80), (2, 'eu', 60), (3, 'us', 45), (4, 'apac', 125)
    AS sales(id, region, amount)
""")
sales.write.format("delta").mode("overwrite").save(delta_path)
assert spark.read.format("delta").load(delta_path).count() == 4
print("Spark SQL -> OneLake Delta: PASS", flush=True)

# Publish a real TMSL Direct Lake model over that exact Lakehouse table.
model = {
    "name": "SalesDirectLake", "compatibilityLevel": 1604,
    "model": {
        "expressions": [{
            "name": "DL_Lakehouse", "kind": "m",
            "expression": (
                "let Source = AzureStorage.DataLake("
                f"\"https://onelake.dfs.fabric.microsoft.com/{workspace['id']}/{lakehouse['id']}\", "
                "[HierarchicalNavigation=true]) in Source"
            ),
        }],
        "tables": [{
            "name": "Sales",
            "columns": [
                {"name": "Id", "dataType": "int64", "sourceColumn": "id"},
                {"name": "Region", "dataType": "string", "sourceColumn": "region"},
                {"name": "Amount", "dataType": "int64", "sourceColumn": "amount"},
            ],
            "measures": [{"name": "Total", "expression": "SUM(Sales[Amount])"}],
            "partitions": [{
                "name": "Sales", "mode": "directLake",
                "source": {"type": "entity", "entityName": "sales", "schemaName": "dbo", "expressionSource": "DL_Lakehouse"},
            }],
        }],
    },
}
semantic = create_item(workspace["id"], {
    "displayName": "Sales Direct Lake", "type": "SemanticModel",
    "definition": {"parts": [{
        "path": "model.bim", "payloadType": "InlineBase64",
        "payload": base64.b64encode(json.dumps(model).encode()).decode(),
    }]},
}, fabric_token)

try:
    request("POST", f"{ENTRA}/admin/api/apps", {
        "displayName": "Power BI Service", "appIdUri": PBI, "isConfidential": False,
    })
except urllib.error.HTTPError as error:
    if error.code != 409:
        raise
pbi_token = token(PBI + "/.default")
dax = request(
    "POST",
    f"{FABRIC}/v1.0/myorg/groups/{workspace['id']}/datasets/{semantic['id']}/executeQueries",
    {"queries": [{"query": "EVALUATE SUMMARIZECOLUMNS(Sales[Region], \"Total\", [Total])"}]},
    pbi_token,
)[2]
rows = dax["results"][0]["tables"][0]["rows"]
totals = {row["Sales[Region]"]: row["[Total]"] for row in rows}
assert totals == {"us": 125, "eu": 60, "apac": 125}, totals
print(f"OneLake Delta -> Direct Lake DAX: PASS {totals}", flush=True)

# The POSITIVE branch of the Power BI lineage/refresh surface, witnessed here
# because this is the only e2e that builds a real Direct Lake model. The
# negative branch — an inline-data model reporting no datasources and refusing
# a refresh — is witnessed in e2e/semantic-model.
#
# DATASOURCES is the lineage a governance tool asks for: until this endpoint
# existed, "this model reads that lakehouse" lived only inside the TMSL and the
# emulator's resolver, and nothing could be asked.
base = f"{FABRIC}/v1.0/myorg/groups/{workspace['id']}/datasets/{semantic['id']}"
srcs = request("GET", f"{base}/datasources", token=pbi_token)[2]["value"]
assert len(srcs) == 1, f"want exactly the one lakehouse, got {srcs}"
assert lakehouse["id"] in srcs[0]["connectionDetails"]["url"], srcs
print(f"datasources: {srcs[0]['datasourceType']} -> {srcs[0]['connectionDetails']['path']}", flush=True)

# This model NAMES a source it can re-read, so isRefreshable must be true and
# the refresh must be accepted — the same predicate decides both, so a client
# that trusts the flag is never then refused.
one = request("GET", base, token=pbi_token)[2]
assert one["isRefreshable"] is True, f"a Direct Lake model must be refreshable: {one}"

status, headers, _ = request("POST", f"{base}/refreshes", {"notifyOption": "NoNotification"}, pbi_token)
assert status == 202, status
request_id = headers.get("RequestId")
assert request_id, "no RequestId header; a polling client has nothing to poll on"

history = request("GET", f"{base}/refreshes", token=pbi_token)[2]["value"]
assert history and history[0]["requestId"] == request_id, (request_id, history)
assert history[0]["status"] == "Completed", history[0]
one_refresh = request("GET", f"{base}/refreshes/{request_id}", token=pbi_token)[2]
assert one_refresh["requestId"] == request_id, one_refresh
print(f"refresh: 202 -> {request_id} -> {history[0]['status']}", flush=True)

# A Direct Lake model reads its Delta at QUERY time, so the refresh changed
# nothing and the answers must be identical. That is the point of reporting
# Completed immediately rather than staging a fake reload.
after = request(
    "POST", f"{base}/executeQueries",
    {"queries": [{"query": "EVALUATE SUMMARIZECOLUMNS(Sales[Region], \"Total\", [Total])"}]},
    pbi_token,
)[2]
after_totals = {r["Sales[Region]"]: r["[Total]"] for r in after["results"][0]["tables"][0]["rows"]}
assert after_totals == totals, (totals, after_totals)
print("post-refresh DAX unchanged — Direct Lake was already current", flush=True)

# THE SCANNER — how a catalog crawler learns what the tenant contains, rather
# than querying one thing it already knows about. This is the only surface that
# returns a model's tables, columns and measures, which is exactly what a
# governance tool needs and what nothing else here can supply.
#
# The four-call shape is the real one: find what changed, ask, poll, read. The
# emulator finishes synchronously, but a crawler written against the service
# polls scanStatus, so that call has to work rather than be skippable.
admin_base = f"{FABRIC}/v1.0/myorg/admin/workspaces"
modified = request("GET", f"{admin_base}/modified", token=fabric_token)[2]
assert any(m["id"] == workspace["id"] for m in modified), (workspace["id"], modified)

scan = request("POST", f"{admin_base}/getInfo"
               "?datasetSchema=true&datasetExpressions=true&datasourceDetails=true",
               {"workspaces": [workspace["id"]]}, fabric_token)[2]
assert scan["status"] == "Succeeded", scan
status = request("GET", f"{admin_base}/scanStatus/{scan['id']}", token=fabric_token)[2]
assert status["id"] == scan["id"] and status["status"] == "Succeeded", status

result = request("GET", f"{admin_base}/scanResult/{scan['id']}", token=fabric_token)[2]
scanned = [d for w in result["workspaces"] for d in w["datasets"] if d["id"] == semantic["id"]]
assert len(scanned) == 1, (semantic["id"], result)
model_info = scanned[0]

# Structure: the tables, columns and measures a catalog would ingest.
tables = {t["name"]: t for t in model_info["tables"]}
assert "Sales" in tables, tables
cols = {c["name"] for c in tables["Sales"]["columns"]}
assert {"Id", "Region", "Amount"} <= cols, cols
measures = {m["name"]: m["expression"] for m in tables["Sales"]["measures"]}
assert measures.get("Total"), measures

# Lineage in the SAME payload — the reason a crawler scans instead of calling
# /datasources per dataset.
assert any(lakehouse["id"] in d["connectionDetails"]["url"]
           for d in result["datasourceInstances"]), result["datasourceInstances"]
print(f"scan: {len(cols)} columns, measure {list(measures)[0]}, "
      f"{len(result['datasourceInstances'])} datasource(s)", flush=True)

# And the flags are honoured: without datasetSchema the dataset is still listed
# but carries no schema. A crawler that forgets the flag and gets the schema
# anyway would ship, then break against real Fabric.
bare_id = request("POST", f"{admin_base}/getInfo", {"workspaces": [workspace["id"]]}, fabric_token)[2]["id"]
bare = request("GET", f"{admin_base}/scanResult/{bare_id}", token=fabric_token)[2]
bare_ds = [d for w in bare["workspaces"] for d in w["datasets"] if d["id"] == semantic["id"]][0]
assert not bare_ds.get("tables"), bare_ds
assert bare_ds["name"], bare_ds
print("scan without datasetSchema: dataset listed, schema withheld", flush=True)

# Track the DAX observation through the real MLflow client and attached server.
tracking_uri = f"{FABRIC}/mlflow/workspaces/{workspace['id']}"
os.environ["MLFLOW_TRACKING_URI"] = tracking_uri
os.environ["MLFLOW_TRACKING_TOKEN"] = fabric_token
mlflow.set_tracking_uri(tracking_uri)
mlflow.set_experiment("direct-lake-validation")
with mlflow.start_run(run_name="dax-observation") as run:
    mlflow.log_param("semantic_model_id", semantic["id"])
    mlflow.log_metric("grand_total", sum(totals.values()))
    mlflow.log_text(json.dumps(totals, sort_keys=True), "dax/region_totals.json")
    run_id = run.info.run_id
client = MlflowClient(tracking_uri=tracking_uri)
client.create_registered_model("sales-direct-lake")
experiments = request("GET", f"{FABRIC}/v1/workspaces/{workspace['id']}/mlExperiments", token=fabric_token)[2]["value"]
models = request("GET", f"{FABRIC}/v1/workspaces/{workspace['id']}/mlModels", token=fabric_token)[2]["value"]
assert [item["displayName"] for item in experiments] == ["direct-lake-validation"], experiments
assert [item["displayName"] for item in models] == ["sales-direct-lake"], models
experiment_id = experiments[0]["id"]
storage_token = request("POST", f"{ENTRA}/admin/api/tokens", {
    "clientId": CLIENT_ID, "audience": "https://storage.azure.com",
})[2]
storage_token = storage_token.get("access_token") or storage_token["token"]
listing = request(
    "GET",
    f"http://onelake.dfs.fabric.microsoft.com/{workspace['id']}?resource=filesystem&directory={experiment_id}/Files/mlflow-artifacts&recursive=true",
    token=storage_token,
)[2]
assert any(path["name"].endswith("dax/region_totals.json") for path in listing["paths"]), listing
print(f"Direct Lake DAX -> MLflow tracking/model registry: PASS run={run_id}", flush=True)

# dbt-duckdb's built-in Delta plugin independently validates the same table.
env = {
    **os.environ,
    "DELTA_TABLE_PATH": f"az://{workspace['id']}/{lakehouse['id']}/Tables/sales",
    "AZURE_STORAGE_TOKEN": storage_token,
    "AZURE_STORAGE_ENDPOINT": f"{FABRIC}/onelake",
}
subprocess.run([
    "dbt", "build", "--project-dir", "/dbt", "--profiles-dir", "/dbt",
    "--target-path", "/tmp/dbt-target", "--log-path", "/tmp/dbt-logs",
], check=True, env=env)
with duckdb.connect("/tmp/analytics_db.duckdb", read_only=True) as connection:
    dbt_rows = connection.sql(
        "select region, total_amount, order_count from analytics.region_totals order by region"
    ).fetchall()
assert dbt_rows == [("apac", 125, 1), ("eu", 60, 1), ("us", 125, 2)], dbt_rows
print(f"OneLake Delta -> dbt-duckdb validation: PASS {dbt_rows}", flush=True)

spark.stop()
print("DATA SCIENCE LOOP E2E: PASS", flush=True)
