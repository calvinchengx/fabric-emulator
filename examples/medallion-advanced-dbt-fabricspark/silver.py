"""Silver, declaratively: Microsoft's dbt-fabricspark builds it with Spark SQL,
over the emulator's Livy High-Concurrency surface with Sail behind it.

    dbt-fabricspark -> fabric-emulator (Fabric REST + Livy HC) -> agent -> Sail

This is the step that makes this example different from
`../medallion-pyspark`, and it is the ONLY one. Both examples land the same
bytes, both build gold into the Warehouse with dbt-fabric, and both publish the
same semantic model. What differs is who transforms bronze into silver:
imperative PySpark there, declarative dbt models here.

That is a real choice a Fabric team makes, and it is confined to the
Lakehouse-to-Lakehouse half of the medallion on purpose. Gold is a Warehouse;
dbt-fabricspark materialises into a Lakehouse and has no path to one. An example
that built gold here would be demonstrating something Fabric cannot do.

Sources resolve BY NAME with no help from this step. Opening a Livy session
against a lakehouse registers its `Tables/` in the Spark catalog, as Fabric's
metastore does — this example used to hand-declare each table with
`CREATE TABLE ... USING delta LOCATION ...` and no longer has to.
"""
import json
import os
import pathlib
import ssl
import subprocess
import time

import source_system as src
from common import FABRIC, FABRIC_AUD, load, log, token

HERE = pathlib.Path(__file__).resolve().parent
PROJECT = HERE / "silver_dbt"
st = load()

tok = token(FABRIC_AUD)
lakehouse_name = st["lakehouse_name"]

# dbt-fabricspark's `authentication: int_tests` mode takes a pre-minted bearer
# instead of running its own MSAL flow — the same injection the TDS path does
# with SQL_COPT_SS_ACCESS_TOKEN, and the reason no interactive login happens.
(PROJECT / "profiles.yml").write_text(f"""contoso_silver_spark:
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
        name: "contoso-silver-spark"
""")

# dbt-fabricspark talks to the emulator over HTTPS with `requests`, which
# verifies the chain — and the stack serves a SELF-SIGNED cert no system trust
# store knows. The other steps use the shared `common.S` session with
# verification off, but dbt owns its own client, so the only lever is
# REQUESTS_CA_BUNDLE. Fetch the cert the server is actually presenting and trust
# exactly that, rather than disabling verification globally: the point of the
# emulator serving TLS at all is that clients exercise the real code path.
host, _, port = FABRIC.split("://", 1)[1].partition(":")
ca = PROJECT / ".emulator-ca.pem"
ca.write_text(ssl.get_server_certificate((host, int(port or 443))))

from livy_query import query  # noqa: E402 — after the profile is written

env = {**os.environ, "DBT_PROFILES_DIR": str(PROJECT), "LAKEHOUSE_NAME": lakehouse_name,
       "REQUESTS_CA_BUNDLE": str(ca), "SSL_CERT_FILE": str(ca)}
t0 = time.time()
rc = subprocess.run(["dbt", "build"], cwd=PROJECT, env=env).returncode
build_secs = time.time() - t0
assert rc == 0, f"dbt build failed: exit {rc}"

# Read silver back through the same Livy surface dbt used, so the numbers are
# the engine's own answer rather than a client-side recomputation.
n_customers = int(query(f"SELECT COUNT(*) FROM {lakehouse_name}.silver_customers")[0][0])
n_orders = int(query(f"SELECT COUNT(*) FROM {lakehouse_name}.silver_orders")[0][0])
n_quarantine = int(query(
    f"SELECT COUNT(*) FROM {lakehouse_name}.silver_quarantine_orders")[0][0])
countries = {r[0] for r in query(
    f"SELECT DISTINCT country FROM {lakehouse_name}.silver_customers")}
n_missing_email = int(query(
    f"SELECT COUNT(*) FROM {lakehouse_name}.silver_customers WHERE email = ''")[0][0])

# The same oracle the PySpark example asserts against — not a relaxed version.
# Two engines asked for the same transform must produce the same answer, or the
# comparison in compare.py is measuring nothing.
assert n_customers == src.EXPECTED_SILVER_CUSTOMERS, n_customers
assert n_orders == src.EXPECTED_SILVER_ORDERS, n_orders
assert n_quarantine == src.EXPECTED_QUARANTINED, n_quarantine
assert countries == src.EXPECTED_COUNTRIES, countries
assert n_missing_email > 0, "the missing-email cohort vanished"

log(f"silver (dbt-fabricspark): {n_customers:,} customers, {n_orders:,} orders, "
    f"{n_quarantine:,} quarantined — built in {build_secs:.1f}s")

summary = {
    "engine": "dbt-fabricspark",
    "target": "Lakehouse (Spark SQL over Livy HC)",
    "compute": "Sail (Rust Spark Connect, no JVM)",
    "build_seconds": round(build_secs, 2),
    "rows": {"silver_customers": n_customers, "silver_orders": n_orders,
             "silver_quarantine_orders": n_quarantine},
    # Empty, and that is the finding rather than an omission: Spark SQL needed
    # no statement rewriting on the wire. The Warehouse half of both examples
    # does (docs/29-tsql-parity.md, T6 and T8).
    "dialect_adaptations": [],
}
(HERE / "silver_summary.json").write_text(json.dumps(summary, indent=2))
