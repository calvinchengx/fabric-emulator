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
import time
import urllib.parse
import urllib.request

FABRIC = os.environ["FABRIC"]
ENTRA = os.environ["ENTRA"]
TENANT = "11111111-1111-1111-1111-111111111111"


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
        "client_id": "cccccccc-0000-0000-0000-000000000002",
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
    except Exception as e:
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

print("PASS: sail e2e")
