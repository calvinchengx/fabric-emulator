"""A2 on Sail: a real PySpark Connect client writes and reads a
Delta table through fabric-emulator's OneLake plane — the engine is LakeSail's
Sail, no JVM anywhere (docs/20-lakesail-engine.md).

Runs in a plain python container. Control-plane setup (seed the storage
resource app, create workspace + lakehouse) is plain REST over the container
network; the data path is PySpark Connect → Sail → our OneLake surface.
"""
import json
import sys
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"


def _req(method, url, body=None, token=None, form=False):
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
    # Timed, like every other wait in this suite: an untimed urlopen is the one
    # way a bounded poll loop still hangs for a whole CI job (see e2e/sail).
    with urllib.request.urlopen(req, timeout=60) as r:
        raw = r.read()
        return json.loads(raw) if raw else {}



fabric_token = _req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials", "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET, "scope": "https://api.fabric.microsoft.com/.default",
}, form=True)["access_token"]

ws = _req("POST", f"{FABRIC}/v1/workspaces", {"displayName": "sparkws"}, token=fabric_token)
_req("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, token=fabric_token)
ws_id = ws["id"]
print(f"workspace: {ws_id}", flush=True)

# 2. Real Spark API over Spark Connect — Sail executes; its object_store
# writes through the emulator's OneLake plane (endpoint override; storage
# token minted by the sail launcher). Same abfs:// URL as the JVM ABFS days.
import os  # noqa: E402
import time  # noqa: E402

from pyspark.sql import SparkSession  # noqa: E402

acct = "onelake.dfs.fabric.microsoft.com"
for _attempt in range(30):
    try:
        spark = SparkSession.builder.remote(os.environ["SPARK_REMOTE"]).getOrCreate()
        spark.sql("SELECT 1").collect()
        break
    except Exception:
        if _attempt == 29:
            raise
        time.sleep(2)
# Sail reports this limit as "3GB"; pyspark 4.2's createDataFrame does int()
# on it — override with an integer so plain createDataFrame works.
spark.conf.set("spark.sql.session.localRelationSizeLimit", str(64 * 1024 * 1024))

path = f"abfs://{ws_id}@{acct}/lake.Lakehouse/Tables/events"

df = spark.createDataFrame([(1, "a"), (2, "b"), (3, "c")], ["id", "name"])
df.write.format("delta").mode("overwrite").save(path)
print("spark delta write: OK", flush=True)

back = spark.read.format("delta").load(path)
rows = sorted((r["id"], r["name"]) for r in back.collect())
assert rows == [(1, "a"), (2, "b"), (3, "c")], rows
print(f"spark delta read-back: OK {rows}", flush=True)

# 3. A second real Delta commit (append) — exercises _delta_log put-if-absent.
spark.createDataFrame([(4, "d")], ["id", "name"]).write.format("delta").mode("append").save(path)
n = spark.read.format("delta").load(path).count()
assert n == 4, n
print(f"spark delta append: OK ({n} rows, 2 commits)", flush=True)

spark.stop()
print("SPARK-A2 E2E: PASS", flush=True)
sys.exit(0)
