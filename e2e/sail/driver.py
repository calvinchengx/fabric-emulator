#!/usr/bin/env python3
"""S0 driver: real PySpark (Spark Connect client, no JVM anywhere) against
Sail, writing and reading Delta through fabric-emulator's OneLake plane.

Control plane: create workspace + lakehouse with a fabric-audience token.
Data plane: Sail executes the plans; its object_store speaks the az:// +
endpoint-override recipe to the emulator's Blob surface, including the
If-None-Match conditional PUT every Delta commit needs.
"""
import json
import os
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor

FABRIC = os.environ["FABRIC"]
ENTRA = os.environ["ENTRA"]
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"


def post_json(url, body, token=None):
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read() or b"{}")


def entra_token(scope):
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
        "client_secret": "daemon-app-secret",
        "scope": scope,
    }).encode()
    req = urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read())["access_token"]


# --- control plane: a workspace and a lakehouse to write into ---
fabric_token = entra_token("https://api.fabric.microsoft.com/.default")
ws = post_json(f"{FABRIC}/v1/workspaces", {"displayName": "sailws"}, fabric_token)
post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, fabric_token)
print(f"workspace: {ws['id']}")

# --- data plane: PySpark over Spark Connect to Sail ---
from pyspark.sql import SparkSession  # noqa: E402

for attempt in range(30):  # sail may still be starting
    try:
        spark = SparkSession.builder.remote(os.environ["SPARK_REMOTE"]).getOrCreate()
        spark.sql("SELECT 1").collect()
        break
    except Exception:
        if attempt == 29:
            raise
        time.sleep(2)
print("connected to sail")

url = "az://sailws/lake.Lakehouse/Tables/events"

# Rows via SQL VALUES, not createDataFrame: pyspark 4.2's local-relation path
# asks the server for spark.sql.session.localRelationSizeLimit and chokes on
# Sail 0.6.6's "3GB" string. VALUES keeps everything server-side (and is the
# better engine witness anyway).
df = spark.sql(
    "SELECT * FROM VALUES (1,'signup','eu'), (2,'purchase','us'), (3,'signup','us')"
    " AS t(id, kind, region)"
)
df.write.format("delta").mode("overwrite").save(url)
print("delta write OK")

back = spark.read.format("delta").load(url)
back.createOrReplaceTempView("events")
rows = spark.sql("SELECT kind, COUNT(*) AS n FROM events GROUP BY kind ORDER BY kind").collect()
got = {r["kind"]: r["n"] for r in rows}
assert got == {"purchase": 1, "signup": 2}, got
print(f"sql over delta OK: {got}")

# Append — a second Delta commit exercises the conditional-PUT log protocol.
spark.sql("SELECT * FROM VALUES (4,'purchase','eu') AS t(id, kind, region)") \
    .write.format("delta").mode("append").save(url)
n = spark.read.format("delta").load(url).count()
assert n == 4, n
print("delta append OK (4 rows)")

# --- deprecation-audit probes: turn "unverified vs delta-spark" into knowns ---

# Time travel by version. (The SQL `VERSION AS OF` form also works — see
# docs/engine-matrix.md; an earlier comment here called it a Sail gap.)
v0 = spark.read.format("delta").option("versionAsOf", 0).load(url).count()
assert v0 == 3, v0
print("time travel OK (versionAsOf 0 -> 3 rows)")

# MERGE INTO (copy-on-write): update one row, insert another. Finding: Sail
# resolves path-based delta.`az://…` for READS but not as a MERGE target —
# the target must be a catalog table, so register the location first.
spark.sql(f"CREATE TABLE events_t USING delta LOCATION '{url}'")
spark.sql("""
    MERGE INTO events_t AS t
    USING (SELECT * FROM VALUES (4,'refund','eu'), (5,'signup','ap') AS s(id, kind, region)) AS s
    ON t.id = s.id
    WHEN MATCHED THEN UPDATE SET t.kind = s.kind
    WHEN NOT MATCHED THEN INSERT *
""")
merged = {r["id"]: r["kind"] for r in spark.read.format("delta").load(url).collect()}
assert merged[4] == "refund" and merged[5] == "signup" and len(merged) == 5, merged
print("MERGE INTO OK (update + insert, via registered table)")

# Fabric-style abfss:// (Hadoop form, the URL shape unmodified production
# notebooks use). Sail parses container@account.dfs.fabric.microsoft.com and
# the endpoint override redirects the requests to the emulator — if this
# holds, no abfss->az shim is needed anywhere.
abfss = "abfss://sailws@onelake.dfs.fabric.microsoft.com/lake.Lakehouse/Tables/abfss_probe"
spark.sql("SELECT * FROM VALUES (1,'x') AS t(id, v)") \
    .write.format("delta").mode("overwrite").save(abfss)
assert spark.read.format("delta").load(abfss).count() == 1
print("abfss:// (Fabric Hadoop form) OK — no shim needed")

# --- executable compatibility boundary -------------------------------------

def expect_unavailable(name, operation):
    try:
        operation()
    except Exception as error:
        print(f"known gap confirmed: {name} ({type(error).__name__})")
        return
    raise AssertionError(f"Sail capability changed: {name} unexpectedly succeeded; update the parity matrix")


expect_unavailable("SparkContext / RDD", lambda: spark.sparkContext.parallelize([1, 2]).count())
expect_unavailable("Py4J JVM bridge / Java and Scala UDFs", lambda: spark._jvm.java.lang.System.nanoTime())
# Sail stores arbitrary Spark configuration, so this setting appears to work,
# but there is no JVM/classloader that could load the referenced JAR.
spark.conf.set("spark.jars", "/tmp/compat-probe.jar")
assert spark.conf.get("spark.jars") == "/tmp/compat-probe.jar"
print("known divergence confirmed: spark.jars is accepted but inert (no JVM classloader)")


def start_stream():
    query = (spark.readStream.format("rate").load().writeStream
             .format("memory").queryName("sail_stream_probe").start())
    query.awaitTermination(10)


expect_unavailable("Structured Streaming execution", start_stream)
expect_unavailable("OPTIMIZE", lambda: spark.sql("OPTIMIZE events_t").collect())
expect_unavailable("VACUUM", lambda: spark.sql("VACUUM events_t RETAIN 168 HOURS").collect())
cdf_probe = (spark.read.format("delta").option("readChangeFeed", "true")
             .option("startingVersion", 0).load(url))
cdf_probe.collect()
assert "_change_type" not in cdf_probe.columns, cdf_probe.columns
print("known divergence confirmed: CDF options are accepted but return a normal snapshot")

# Launch two overwrite commits from separate sessions at the same barrier.
# The OneLake conditional-create contract exposes the Delta log collision:
# exactly one writer commits and the other receives a transaction failure.
conflict_url = "az://sailws/lake.Lakehouse/Tables/concurrent_probe"
spark.range(1).write.format("delta").mode("overwrite").save(conflict_url)
barrier = threading.Barrier(2)


def concurrent_overwrite(value):
    session = SparkSession.builder.remote(os.environ["SPARK_REMOTE"]).create()
    try:
        barrier.wait(timeout=30)
        session.range(value, value + 10).write.format("delta").mode("overwrite").save(conflict_url)
        return "committed"
    except Exception as error:
        return f"rejected:{type(error).__name__}"
    finally:
        session.stop()


with ThreadPoolExecutor(max_workers=2) as executor:
    outcomes = list(executor.map(concurrent_overwrite, (100, 200)))
assert outcomes.count("committed") == 1, outcomes
assert sum(outcome.startswith("rejected:") for outcome in outcomes) == 1, outcomes
assert spark.read.format("delta").load(conflict_url).count() == 10
print(f"concurrent overwrite conflict rejected: {outcomes}")

# The engine's writes are real bytes in OneLake: read the first Delta commit
# back through the Blob surface ourselves (same account-prefixed path form).
storage_token = entra_token("https://storage.azure.com/.default")
req = urllib.request.Request(
    f"{FABRIC}/onelake/{ws['id']}/lake.Lakehouse/Tables/events/_delta_log/"
    "00000000000000000000.json",
    headers={"Authorization": "Bearer " + storage_token},
)
with urllib.request.urlopen(req, timeout=30) as r:
    first_commit = r.read().decode()
assert '"protocol"' in first_commit and '"metaData"' in first_commit, first_commit[:200]
print("delta log readable through the Blob surface (protocol + metaData present)")

# The conditional-create contract itself, with the interleaving as an INPUT
# rather than sampled for.
#
# The racing writers above can only observe a rejection when they happen to
# collide, and they legitimately may not (see that block). This asserts the
# same guarantee with no race at all: version 0 of the events table exists, so
# a put-if-absent against that exact path must be refused. It is the rejection
# half of the concurrency story, held to a deterministic standard, and it is
# the 409 the racing writers see when they DO collide.
#
# TestConcurrentDeltaCommitRace pins the same mechanism at unit level; this
# pins it end-to-end, through the Blob surface a real client reaches, with the
# token a real client mints.
conflict_probe = urllib.request.Request(
    f"{FABRIC}/onelake/{ws['id']}/lake.Lakehouse/Tables/events/_delta_log/"
    "00000000000000000000.json",
    method="PUT",
    data=b'{"commitInfo":{"operation":"SHOULD NOT LAND"}}',
    headers={"Authorization": "Bearer " + storage_token, "If-None-Match": "*"},
)
try:
    urllib.request.urlopen(conflict_probe, timeout=30)
    raise AssertionError("put-if-absent overwrote an existing Delta commit")
except urllib.error.HTTPError as refused:
    assert refused.code == 409, refused.code
print("put-if-absent on an existing commit refused with 409")

# And it refused WITHOUT damaging what was there. A 409 that still replaced the
# bytes would satisfy the status assertion and lose the commit anyway.
with urllib.request.urlopen(req, timeout=30) as r:
    assert r.read().decode() == first_commit, "the refused PUT modified the commit"
print("the refused PUT left the existing commit byte-identical")

print("PASS: sail e2e")
