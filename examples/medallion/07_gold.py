"""Gold: Microsoft's real dbt-fabric builds the star in the Warehouse over TDS,
and `dbt build` runs the data-quality tests as part of the same graph.

Sources point at the LAKEHOUSE database (the SQL analytics endpoint reflects its
Delta); models build in the WAREHOUSE database. Both are databases on the same
engine, so dbt reads across them with three-part names.
"""
import os
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
rc = subprocess.run(["dbt", "build"], cwd=GOLD_PROJECT, env=env).returncode
assert rc == 0, f"dbt build failed: exit {rc}"

with tds_connect(st["warehouse"], sql_tok) as c:
    revenue = c.cursor().execute("SELECT SUM(revenue) FROM fct_daily_revenue").fetchone()[0]
    orders = c.cursor().execute("SELECT COUNT(*) FROM fct_orders").fetchone()[0]
assert abs(float(revenue) - src.EXPECTED_REVENUE) < 0.01, revenue
assert int(orders) == src.EXPECTED_SILVER_ORDERS, orders
log(f"gold: dbt build green, revenue={float(revenue):.2f} over {orders} orders")
