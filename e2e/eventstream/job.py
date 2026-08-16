"""Eventstream notebook API against a real Kafka topic — never `rate`.

Works on JVM Spark (OSS kafka source) and on Sail (emulator consume +
LocalRelation + local foreachBatch). Same notebook snippet either way.
"""
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"


def request(method, url, body=None, token=None, form=False):
    data, headers = None, {}
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
    with urllib.request.urlopen(req) as response:
        raw = response.read()
        return json.loads(raw) if raw else {}


fabric_token = request("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials",
    "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET,
    "scope": "https://api.fabric.microsoft.com/.default",
}, form=True)["access_token"]
workspace = request("POST", f"{FABRIC}/v1/workspaces", {"displayName": "eventstream-ws"}, fabric_token)
item = request("POST", f"{FABRIC}/v1/workspaces/{workspace['id']}/eventstreams",
               {"displayName": "clicks"}, fabric_token)
streams = (item.get("properties") or {}).get("streams") or []
assert streams and streams[0].get("id"), item
item_id, datasource_id = item["id"], streams[0]["id"]
print("eventstream item:", item_id, "datasource:", datasource_id, flush=True)

lake = request("POST", f"{FABRIC}/v1/workspaces/{workspace['id']}/lakehouses",
               {"displayName": "clicks-lh"}, fabric_token)
bound = request(
    "POST",
    f"{FABRIC}/v1/workspaces/{workspace['id']}/eventstreams/{item_id}/destinations",
    {"type": "Lakehouse", "itemId": lake["id"], "table": "clicks"},
    fabric_token,
)
assert bound.get("type") == "Lakehouse" and bound.get("table") == "clicks", bound
print("lakehouse destination bound:", lake["id"], flush=True)

reflex = request("POST", f"{FABRIC}/v1/workspaces/{workspace['id']}/reflexes",
                 {"displayName": "clicks-reflex"}, fabric_token)
pipe_def = {
    "properties": {
        "activities": [{
            "name": "Capture",
            "type": "SetVariable",
            "typeProperties": {
                "variableName": "seen",
                "value": "@pipeline()?.TriggerEvent?.Value",
            },
        }],
        "variables": {"seen": {"type": "String"}},
    }
}
pipe_payload = base64.b64encode(json.dumps(pipe_def).encode()).decode()
pipe_req = urllib.request.Request(
    f"{FABRIC}/v1/workspaces/{workspace['id']}/dataPipelines",
    data=json.dumps({
        "displayName": "on-clicks",
        "definition": {"parts": [{
            "path": "pipeline-content.json",
            "payloadType": "InlineBase64",
            "payload": pipe_payload,
        }]},
    }).encode(),
    headers={"Authorization": "Bearer " + fabric_token, "Content-Type": "application/json"},
    method="POST",
)
with urllib.request.urlopen(pipe_req) as response:
    pipe_raw = response.read()
    pipe_code = response.status
    pipe_op = response.headers.get("x-ms-operation-id")
    pipe = json.loads(pipe_raw) if pipe_raw else {}
if pipe_code == 202 and pipe_op:
    for _ in range(60):
        op = request("GET", f"{FABRIC}/v1/operations/{pipe_op}", token=fabric_token)
        if op.get("status") == "Succeeded":
            pipe = request("GET", f"{FABRIC}/v1/operations/{pipe_op}/result", token=fabric_token)
            break
        time.sleep(0.1)
    else:
        raise SystemExit("pipeline create LRO did not succeed")
assert pipe.get("id"), pipe
trig = request(
    "POST",
    f"{FABRIC}/v1/workspaces/{workspace['id']}/reflexes/{reflex['id']}/triggers",
    {
        "displayName": "on-clicks",
        "eventType": "Microsoft.Fabric.Eventstream.EventReceived",
        "source": {"itemId": item_id},
        "action": {"itemId": pipe["id"], "jobType": "Pipeline"},
    },
    fabric_token,
)
assert trig.get("eventType") == "Microsoft.Fabric.Eventstream.EventReceived", trig
reflex_dest = request(
    "POST",
    f"{FABRIC}/v1/workspaces/{workspace['id']}/eventstreams/{item_id}/destinations",
    {"type": "Reflex", "itemId": reflex["id"]},
    fabric_token,
)
assert reflex_dest.get("type") == "Reflex", reflex_dest
print("reflex destination bound:", reflex["id"], "pipeline:", pipe["id"], flush=True)

payloads = [{"n": i, "src": "custom"} for i in range(5)]
produced = request(
    "POST",
    f"{FABRIC}/v1/workspaces/{workspace['id']}/eventstreams/{item_id}/sources/{datasource_id}/events",
    {"events": [{"key": f"k{i}", "value": json.dumps(p)} for i, p in enumerate(payloads)]},
    fabric_token,
)
assert produced.get("produced") == 5, produced
print("custom source produced:", produced, flush=True)

jobs = request(
    "GET",
    f"{FABRIC}/v1/workspaces/{workspace['id']}/items/{pipe['id']}/jobs/instances",
    token=fabric_token,
)
runs = jobs.get("value") or []
assert len(runs) == 5, jobs
assert all(r.get("invokeType") == "EventTriggered" for r in runs), runs
print("reflex destination jobs: PASS", flush=True)

storage_token = request("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials",
    "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET,
    "scope": "https://storage.azure.com/.default",
}, form=True)["access_token"]
delta_log_url = (
    f"{FABRIC}/onelake/{workspace['id']}/{lake['id']}"
    f"/Tables/clicks/_delta_log/{0:020d}.json"
)
req = urllib.request.Request(delta_log_url, headers={"Authorization": "Bearer " + storage_token})
with urllib.request.urlopen(req) as response:
    commit = response.read().decode()
saw_rows = saw_schema = False
for line in commit.splitlines():
    if not line.strip():
        continue
    action = json.loads(line)
    if "add" in action:
        stats = action["add"].get("stats")
        if isinstance(stats, str):
            stats = json.loads(stats)
        assert stats.get("numRecords") == 5, stats
        saw_rows = True
    if "metaData" in action:
        schema = json.loads(action["metaData"]["schemaString"])
        names = {field["name"] for field in schema["fields"]}
        assert {"n", "src"} <= names, names
        saw_schema = True
assert saw_rows and saw_schema, commit
print("lakehouse destination delta content: PASS", flush=True)

sys.path.insert(0, "/opt/spark_agent")
sys.path.insert(0, "/opt/spark/work-dir")
import eventstream_kafka  # noqa: E402, I001
from pyspark.sql import SparkSession  # noqa: E402

remote = os.environ.get("SPARK_REMOTE")
builder = SparkSession.builder.appName("fabric-emulator-eventstream")
spark = (builder.remote(remote) if remote else builder).getOrCreate()
if not remote:
    spark.sparkContext.setLogLevel("WARN")
assert eventstream_kafka.install(spark)

# Documented Fabric notebook snippet — no bootstrap servers in user code.
df_raw = spark.readStream.format("kafka").options(**{
    "eventstream.itemid": item_id,
    "eventstream.datasourceid": datasource_id,
}).load()
names = {field.name for field in df_raw.schema.fields}
assert {"key", "value", "topic", "partition", "offset"} <= names, names
assert "rate" not in str(df_raw.schema).lower()
print("kafka schema:", sorted(names), flush=True)

seen = []


def show_df(batch_df, batch_id):
    rows = batch_df.selectExpr(
        "CAST(key AS STRING) as key",
        "CAST(value AS STRING) as value",
        "topic", "partition", "offset",
    ).collect()
    seen.extend(rows)


query = (df_raw.writeStream.foreachBatch(show_df)
         .outputMode("append")
         .trigger(availableNow=True)
         .start())
query.awaitTermination(60)
assert not query.isActive
assert len(seen) == 5, [(r.key, r.value) for r in seen]
values = sorted(json.loads(r.value)["n"] for r in seen)
assert values == [0, 1, 2, 3, 4], values
assert all(r.topic.endswith(datasource_id) for r in seen), [r.topic for r in seen]
print("foreachBatch kafka rows: PASS", flush=True)

# OSS format("kafka") + bootstrap/subscribe. Bytes land on the engine
# (Sail: createDataFrame LocalRelation; JVM: spark-sql-kafka). Never rate.
topic = f"{item_id}.{datasource_id}"
df_oss = spark.read.format("kafka").options(**{
    "kafka.bootstrap.servers": "kafka:9092",
    "subscribe": topic,
    "startingOffsets": "earliest",
    "endingOffsets": "latest",
}).load()
oss_names = {field.name for field in df_oss.schema.fields}
assert {"key", "value", "topic", "partition", "offset"} <= oss_names, oss_names
assert "rate" not in str(df_oss.schema).lower()
decoded = df_oss.selectExpr("CAST(value AS STRING) as v").collect()
assert len(decoded) == 5, [r.v for r in decoded]
assert sorted(json.loads(r.v)["n"] for r in decoded) == [0, 1, 2, 3, 4]
print("native format(kafka) bootstrap/subscribe: PASS", flush=True)

# Unknown IDs fail loudly — never an empty rate stream.
try:
    spark.readStream.format("kafka").options(**{
        "eventstream.itemid": "00000000-0000-0000-0000-000000000000",
        "eventstream.datasourceid": datasource_id,
    }).load()
except Exception as exc:  # noqa: BLE001 — the wrap must raise
    assert "Eventstream" in str(exc) or "not available" in str(exc).lower() or "404" in str(exc), exc
else:
    raise SystemExit("unknown eventstream item id was accepted")
print("unknown item id fails loud: PASS", flush=True)

try:
    spark.readStream.format("kafka").option("eventstream.itemid", item_id).load()
except Exception as exc:  # noqa: BLE001
    assert "both eventstream" in str(exc)
else:
    raise SystemExit("partial eventstream options were accepted")
print("partial options fail loud: PASS", flush=True)

spark.stop()
print("EVENTSTREAM-KAFKA: PASS", flush=True)
