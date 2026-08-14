"""Eventstream notebook API against a real Kafka topic — never `rate`.

Works on JVM Spark (OSS kafka source) and on Sail (emulator consume +
LocalRelation + local foreachBatch). Same notebook snippet either way.
"""
import json
import os
import sys
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

payloads = [{"n": i, "src": "custom"} for i in range(5)]
produced = request(
    "POST",
    f"{FABRIC}/v1/workspaces/{workspace['id']}/eventstreams/{item_id}/sources/{datasource_id}/events",
    {"events": [{"key": f"k{i}", "value": json.dumps(p)} for i, p in enumerate(payloads)]},
    fabric_token,
)
assert produced.get("produced") == 5, produced
print("custom source produced:", produced, flush=True)

sys.path.insert(0, "/opt/spark_agent")
sys.path.insert(0, "/opt/spark/work-dir")
import eventstream_kafka  # noqa: E402, I001
from pyspark.sql import SparkSession  # noqa: E402

remote = os.environ.get("SPARK_REMOTE")
builder = SparkSession.builder.appName("fabric-emulator-eventstream")
spark = (builder.remote(remote) if remote else builder).getOrCreate()
if remote:
    try:
        spark.conf.set("spark.sql.session.localRelationSizeLimit", str(64 * 1024 * 1024))
    except Exception:  # noqa: BLE001 — Connect conf is best-effort
        pass
else:
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
