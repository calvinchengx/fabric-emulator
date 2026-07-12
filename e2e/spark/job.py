"""A2: real JVM Spark writes and reads a Delta table through fabric-emulator's
OneLake plane via the ABFS driver, authenticated to entra-emulator with a
custom token provider (v2 client-credentials for the Storage audience).

Runs inside the Spark container. Control-plane setup (seed the storage
resource app, create workspace + lakehouse) is plain REST over the container
network; the data path is real Spark → ABFS → our DFS surface.
"""
import json
import sys
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
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
    with urllib.request.urlopen(req) as r:
        raw = r.read()
        return json.loads(raw) if raw else {}


# 1. Seed a Storage resource app so client-credentials resolves the
#    https://storage.azure.com audience the ABFS token provider requests.
try:
    _req("POST", f"{ENTRA}/admin/api/apps",
         {"displayName": "Azure Storage", "appIdUri": "https://storage.azure.com", "isConfidential": False})
except urllib.error.HTTPError as e:
    if e.code not in (409,):  # already seeded is fine
        raise
print("seeded storage resource app", flush=True)

fabric_token = _req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials", "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET, "scope": "https://api.fabric.microsoft.com/.default",
}, form=True)["access_token"]

ws = _req("POST", f"{FABRIC}/v1/workspaces", {"displayName": "sparkws"}, token=fabric_token)
_req("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, token=fabric_token)
ws_id = ws["id"]
print(f"workspace: {ws_id}", flush=True)

# 2. Real Spark + Delta, ABFS pointed at the emulator via the container alias.
from pyspark.sql import SparkSession  # noqa: E402

acct = "onelake.dfs.fabric.microsoft.com"
spark = (SparkSession.builder.appName("fabric-emu-a2")
         .config("spark.sql.extensions", "io.delta.sql.DeltaSparkSessionExtension")
         .config("spark.sql.catalog.spark_catalog", "org.apache.spark.sql.delta.catalog.DeltaCatalog")
         .config("spark.hadoop.fs.azure.always.use.https", "false")
         .config(f"spark.hadoop.fs.azure.account.auth.type.{acct}", "Custom")
         .config(f"spark.hadoop.fs.azure.account.oauth.provider.type.{acct}",
                 "com.calvinchengx.fabricemu.EntraTokenProvider")
         .config("spark.hadoop.fs.azure.emu.token.endpoint", f"{ENTRA}/{TENANT}/oauth2/v2.0/token")
         .config("spark.hadoop.fs.azure.emu.client.id", CLIENT_ID)
         .config("spark.hadoop.fs.azure.emu.client.secret", CLIENT_SECRET)
         .config("spark.hadoop.fs.azure.emu.scope", "https://storage.azure.com/.default")
         .getOrCreate())
spark.sparkContext.setLogLevel("WARN")

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
