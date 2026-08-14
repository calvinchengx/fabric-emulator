"""Apache Spark 3.5 / Delta 3.2 compatibility witness for Fabric Runtime 1.3."""
import json
import urllib.error
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
ACCOUNT = "onelake.dfs.fabric.microsoft.com"


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


try:
    request("POST", f"{ENTRA}/admin/api/apps", {
        "displayName": "Azure Storage",
        "appIdUri": "https://storage.azure.com",
        "isConfidential": False,
    })
except urllib.error.HTTPError as error:
    if error.code != 409:
        raise

fabric_token = request("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
    "grant_type": "client_credentials",
    "client_id": CLIENT_ID,
    "client_secret": CLIENT_SECRET,
    "scope": "https://api.fabric.microsoft.com/.default",
}, form=True)["access_token"]
workspace = request("POST", f"{FABRIC}/v1/workspaces", {"displayName": "spark-jvm-ws"}, fabric_token)
request("POST", f"{FABRIC}/v1/workspaces/{workspace['id']}/lakehouses", {"displayName": "lake"}, fabric_token)

from pyspark.sql import SparkSession  # noqa: E402

spark = (SparkSession.builder.appName("fabric-emulator-jvm-oracle")
         .config("spark.sql.extensions", "io.delta.sql.DeltaSparkSessionExtension")
         .config("spark.sql.catalog.spark_catalog", "org.apache.spark.sql.delta.catalog.DeltaCatalog")
         .config("spark.hadoop.fs.azure.always.use.https", "false")
         .config(f"spark.hadoop.fs.azure.account.auth.type.{ACCOUNT}", "Custom")
         .config(f"spark.hadoop.fs.azure.account.oauth.provider.type.{ACCOUNT}",
                 "com.calvinchengx.fabricemu.EntraTokenProvider")
         .config("spark.hadoop.fs.azure.emu.token.endpoint", f"{ENTRA}/{TENANT}/oauth2/v2.0/token")
         .config("spark.hadoop.fs.azure.emu.client.id", CLIENT_ID)
         .config("spark.hadoop.fs.azure.emu.client.secret", CLIENT_SECRET)
         .config("spark.hadoop.fs.azure.emu.scope", "https://storage.azure.com/.default")
         .getOrCreate())
spark.sparkContext.setLogLevel("WARN")

path = f"abfs://{workspace['id']}@{ACCOUNT}/lake.Lakehouse/Tables/events"
spark.createDataFrame([(1, "a"), (2, "b"), (3, "c")], ["id", "name"]) \
    .write.format("delta").mode("overwrite").save(path)
spark.createDataFrame([(4, "d")], ["id", "name"]) \
    .write.format("delta").mode("append").save(path)
rows = sorted((row.id, row.name) for row in spark.read.format("delta").load(path).collect())
assert rows == [(1, "a"), (2, "b"), (3, "c"), (4, "d")], rows
print("common DataFrame + Delta workload: PASS", flush=True)

# Capabilities available in Fabric's JVM runtime but absent from Sail.
assert spark.sparkContext.parallelize([1, 2, 3]).sum() == 6

# THE ORACLE FOR THE Sail `sc` FACADE. python/spark_agent/rdd_contract.py lists
# the idioms the facade implements and the answer each should give; the facade's
# unit tests assert them against the facade, and these lines assert the SAME
# SNIPPETS against real Apache Spark. Neither half is sufficient alone: unit
# tests prove the facade matches the contract, and this proves the contract
# matches Spark. Without this, the expected values would be a claim about
# whoever wrote the facade.
# Mounted beside this file by docker-compose.yml, so spark-submit's own script
# directory resolves it. The previous version computed a HOST path
# (`parents[2]/python/spark_agent`) that does not exist inside the container,
# so this import raised ModuleNotFoundError and the whole oracle never ran —
# discovered only by dispatching the weekly workflow by hand.
import rdd_contract  # noqa: E402

for _label, _snippet, _expected in rdd_contract.CASES:
    _got = eval(_snippet, {"sc": spark.sparkContext})  # noqa: S307 — the contract is source text
    assert _got == _expected, f"{_label}: real Spark gave {_got!r}, contract says {_expected!r}"
for _label, _snippet in rdd_contract.VOID_CASES:
    assert eval(_snippet, {"sc": spark.sparkContext}) is None, _label  # noqa: S307
# And what Spark REFUSES, because a contract that only lists what works cannot
# catch the facade being MORE PERMISSIVE than the tenant — which is exactly the
# defect this half found on its first real run.
for _label, _snippet in rdd_contract.REFUSED_CASES:
    try:
        eval(_snippet, {"sc": spark.sparkContext})  # noqa: S307
    except Exception:  # noqa: BLE001 — any refusal is the contract; the type is Spark's
        pass
    else:
        raise AssertionError(f"{_label}: real Spark ACCEPTED what the contract says it refuses")
print(f"sc-facade contract vs real Spark: {len(rdd_contract.CASES)} cases PASS", flush=True)
assert spark._jvm.java.lang.Class.forName("com.calvinchengx.fabricemu.EntraTokenProvider") is not None
print("SparkContext/RDD + JVM/JAR bridge: PASS", flush=True)

query = (spark.readStream.format("rate").option("rowsPerSecond", 1).load()
         .writeStream.format("memory").queryName("stream_probe").trigger(once=True).start())
query.awaitTermination(60)
assert not query.isActive
print("Structured Streaming: PASS", flush=True)

spark.sql(f"VACUUM delta.`{path}` RETAIN 168 HOURS").collect()
print("Delta VACUUM: PASS", flush=True)

cdf_path = f"abfs://{workspace['id']}@{ACCOUNT}/lake.Lakehouse/Tables/cdf_probe"
(spark.createDataFrame([(1, "before")], ["id", "value"])
 .write.format("delta").option("delta.enableChangeDataFeed", "true").mode("overwrite").save(cdf_path))
spark.createDataFrame([(2, "after")], ["id", "value"]).write.format("delta").mode("append").save(cdf_path)
changes = (spark.read.format("delta").option("readChangeFeed", "true")
           .option("startingVersion", 0).load(cdf_path).count())
assert changes >= 2, changes
print("Delta CDF: PASS", flush=True)

spark.stop()
print("SPARK-JVM COMPAT: PASS", flush=True)
