"""Gold: Microsoft's real dbt-fabric builds the star in the Warehouse over TDS,
and `dbt build` runs the data-quality tests as part of the same graph.

Sources point at the LAKEHOUSE database (the SQL analytics endpoint reflects its
Delta); models build in the WAREHOUSE database. Both are databases on the same
engine, so dbt reads across them with three-part names.
"""
import json
import os
import pathlib
import subprocess
import time

import source_system as src
from common import GOLD_PROJECT, SQL_AUD, TDS_SERVER, load, log, tds_connect, token

st = load()
sql_tok = token(SQL_AUD)

# Warm the warehouse database the same way the lakehouse one was warmed.
last = None
for _ in range(40):
    try:
        with tds_connect(st["warehouse"], sql_tok, timeout=15) as c:
            assert c.cursor().execute("SELECT 1").fetchone()[0] == 1
        break
    except Exception as e:  # noqa: BLE001
        last = e
        time.sleep(3)
else:
    raise SystemExit(f"warehouse warmup failed: {last}")

# The token is pre-minted, so dbt never runs MSAL. encrypt=false matches the TDS
# front, which terminates FedAuth without TLS.
with open(os.path.join(GOLD_PROJECT, "profiles.yml"), "w") as f:
    f.write(f"""contoso_gold:
  target: dev
  outputs:
    dev:
      type: fabric
      driver: "ODBC Driver 18 for SQL Server"
      server: "{TDS_SERVER}"
      database: "{st['warehouse']}"
      schema: "dbo"
      authentication: "ActiveDirectoryAccessToken"
      access_token: "{sql_tok}"
      access_token_expires_on: 0
      encrypt: false
      trust_cert: true
      threads: 1
""")

env = {**os.environ, "DBT_PROFILES_DIR": GOLD_PROJECT, "LAKEHOUSE_ID": st["lakehouse"]}
t0 = time.time()
rc = subprocess.run(["dbt", "build"], cwd=GOLD_PROJECT, env=env).returncode
build_secs = time.time() - t0
assert rc == 0, f"dbt build failed: exit {rc}"

with tds_connect(st["warehouse"], sql_tok) as c:
    revenue = c.cursor().execute("SELECT SUM(revenue) FROM fct_daily_revenue").fetchone()[0]
    orders = c.cursor().execute("SELECT COUNT(*) FROM fct_orders").fetchone()[0]
    customers = c.cursor().execute("SELECT COUNT(*) FROM dim_customer").fetchone()[0]
    days = c.cursor().execute("SELECT COUNT(*) FROM fct_daily_revenue").fetchone()[0]
assert abs(float(revenue) - src.EXPECTED_REVENUE) < 0.01, revenue
assert int(orders) == src.EXPECTED_SILVER_ORDERS, orders
log(f"gold: dbt build green, revenue={float(revenue):.2f} over {orders} orders")

# A machine-readable summary of what this engine produced, so the Lakehouse
# build in examples/medallion-spark can be compared against it without either
# example having to import the other's code. See that example's 07_compare.py.
summary = {
    "engine": "dbt-fabric",
    "target": "Warehouse (T-SQL over TDS)",
    "compute": "SQL Server sidecar",
    "build_seconds": round(build_secs, 2),
    "rows": {"dim_customer": int(customers), "fct_orders": int(orders),
             "fct_daily_revenue": int(days)},
    "revenue": round(float(revenue), 2),
    # Where this dialect needed the emulator to adapt the statement on the wire
    # (docs/29-tsql-parity.md). The Spark path needs none of these, and that
    # difference is the portability cost the comparison is there to show.
    "dialect_adaptations": [
        "CTAS -> SELECT ... INTO (T8): Fabric Warehouse's CREATE TABLE AS SELECT "
        "is not vanilla SQL Server syntax",
        "nested CTE flattening (T6): dbt-fabric wraps every test body in "
        "`with test_main_sql as (...)`, which vanilla SQL Server rejects",
    ],
}
pathlib.Path(GOLD_PROJECT).parent.joinpath("gold_summary.json").write_text(
    json.dumps(summary, indent=2))
log(f"wrote gold_summary.json (dbt build took {build_secs:.1f}s)")
