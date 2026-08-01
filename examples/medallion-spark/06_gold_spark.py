"""Gold, the Lakehouse way: Microsoft's dbt-fabricspark builds the star with
Spark SQL, over the emulator's Livy High-Concurrency surface with Sail behind it.

    dbt-fabricspark -> fabric-emulator (Fabric REST + Livy HC) -> SQL agent -> Sail

This is the same star the Warehouse example builds, and deliberately so — the
comparison in 07 is only meaningful if both sides were asked for the same thing.
What differs is everything underneath:

  * **No reflection step.** The Warehouse path has to copy every Delta table
    into a SQL engine before dbt can see it (`07_reflect.py` there). Spark reads
    the Delta files silver already wrote. There is no equivalent step here
    because there is no copy.
  * **No second database.** There is a lakehouse and nothing else; the Warehouse
    path needs a Warehouse item alongside the Lakehouse and reads across the two
    with three-part names.
  * **No dialect adaptation.** dbt-fabric's `table` materialization emits
    Fabric/Synapse CTAS that vanilla SQL Server rejects, so the emulator
    rewrites it on the wire (docs/29, T8), and dbt's test bodies need nested-CTE
    flattening (T6). Spark SQL needs none of that.
"""
import pathlib as _pathlib
import sys as _sys

# The shared halves of this pipeline — endpoints, tokens, state, and the seeded
# fixture — live in the Warehouse example, because both paths ingest the same
# data from the same source system. Importing them beats copying a 700-line
# generator that would then have to be kept identical by hand.
_sys.path.insert(0, str(_pathlib.Path(__file__).resolve().parent.parent / "medallion"))

import json
import os
import pathlib
import ssl
import subprocess
import time

import source_system as src
from common import FABRIC, FABRIC_AUD, load, log, token

HERE = pathlib.Path(__file__).resolve().parent
PROJECT = HERE / "gold"
st = load()

tok = token(FABRIC_AUD)
lakehouse_name = st["lakehouse_name"]

# dbt-fabricspark's `authentication: int_tests` mode takes a pre-minted bearer
# instead of running its own MSAL flow — the same injection the TDS path does
# with SQL_COPT_SS_ACCESS_TOKEN, and the reason no interactive login happens.
(PROJECT / "profiles.yml").write_text(f"""contoso_gold_spark:
  target: dev
  outputs:
    dev:
      type: fabricspark
      method: livy
      livy_mode: fabric
      authentication: int_tests
      accessToken: "{tok}"
      endpoint: "{FABRIC}/v1"
      workspaceid: "{st['workspace']}"
      lakehouseid: "{st['lakehouse']}"
      lakehouse: "{lakehouse_name}"
      schema: "{lakehouse_name}"
      threads: 1
      connect_retries: 3
      connect_timeout: 60
      spark_config:
        name: "medallion-spark-gold"
""")

# dbt-fabricspark talks to the emulator over HTTPS with `requests`, which
# verifies the chain — and the developer stack serves a SELF-SIGNED cert that no
# system trust store knows. The other steps use the shared `common.S` session
# with verification off, but dbt owns its own client, so the only lever is
# REQUESTS_CA_BUNDLE. Fetch the cert the server is actually presenting and trust
# exactly that, rather than disabling verification globally: the point of the
# emulator serving TLS at all is that clients exercise the real code path.
host, _, port = FABRIC.split("://", 1)[1].partition(":")
ca = PROJECT / ".emulator-ca.pem"
ca.write_text(ssl.get_server_certificate((host, int(port or 443))))

from livy_query import query  # noqa: E402 — after the path shim above

# Register silver in the Spark catalog before dbt looks for it.
#
# On real Fabric a Lakehouse's `Tables/` are already catalog tables: attach a
# notebook and `SELECT * FROM silver_customers` just works, because Fabric keeps
# the metastore in step with the folder. This stack has no metastore — Sail sees
# object storage and nothing else — so the tables have to be declared, and doing
# it here rather than pretending otherwise is the honest version.
#
# That gap is worth naming as a parity gap and not a setup chore: it is the one
# place this path needs a step real Fabric does not.
onelake = f"abfs://{st['workspace']}@onelake.dfs.fabric.microsoft.com/{st['lakehouse']}"
query(f"CREATE SCHEMA IF NOT EXISTS {lakehouse_name}")
for tbl in ("silver_customers", "silver_orders"):
    query(f"DROP TABLE IF EXISTS {lakehouse_name}.{tbl}")
    query(f"CREATE TABLE {lakehouse_name}.{tbl} USING delta "
          f"LOCATION '{onelake}/Tables/{tbl}'")
log(f"registered silver in the Spark catalog as {lakehouse_name}.silver_* "
    f"(real Fabric does this for you — see the note in this step)")

env = {**os.environ, "DBT_PROFILES_DIR": str(PROJECT), "LAKEHOUSE_NAME": lakehouse_name,
       "REQUESTS_CA_BUNDLE": str(ca), "SSL_CERT_FILE": str(ca)}
t0 = time.time()
rc = subprocess.run(["dbt", "build"], cwd=PROJECT, env=env).returncode
build_secs = time.time() - t0
assert rc == 0, f"dbt build failed: exit {rc}"

# Read the star back through the same Livy surface dbt used, so the numbers are
# the engine's own answer rather than a client-side recomputation.
revenue = float(query(f"SELECT SUM(revenue) FROM {lakehouse_name}.fct_daily_revenue")[0][0])
orders = int(query(f"SELECT COUNT(*) FROM {lakehouse_name}.fct_orders")[0][0])
customers = int(query(f"SELECT COUNT(*) FROM {lakehouse_name}.dim_customer")[0][0])
days = int(query(f"SELECT COUNT(*) FROM {lakehouse_name}.fct_daily_revenue")[0][0])

assert abs(revenue - src.EXPECTED_REVENUE) < 0.01, (revenue, src.EXPECTED_REVENUE)
assert orders == src.EXPECTED_SILVER_ORDERS, orders
log(f"gold (Spark): dbt build green, revenue={revenue:,.2f} over {orders:,} orders")

summary = {
    "engine": "dbt-fabricspark",
    "target": "Lakehouse (Spark SQL over Livy HC)",
    "compute": "Sail (Rust Spark Connect, no JVM)",
    "build_seconds": round(build_secs, 2),
    "rows": {"dim_customer": customers, "fct_orders": orders,
             "fct_daily_revenue": days},
    "revenue": round(revenue, 2),
    # Empty, and that is the finding rather than an omission.
    "dialect_adaptations": [],
}
(HERE / "gold_summary.json").write_text(json.dumps(summary, indent=2))
log(f"wrote gold_summary.json (dbt build took {build_secs:.1f}s)")
