"""Gold: the multi-source star, joined in the Warehouse by dbt-fabric.

`gold.py` builds the single-source star from POS alone — one dimension, one
fact, a daily aggregate. This builds the star the three sources actually
support: a customer dimension keyed by resolved identity, a product dimension
assembled from the only published catalogue plus everything the transactions
reference, a date dimension derived from real trading days, and an order-LINE
fact unioning both selling channels.

Gold is a Warehouse, so this is dbt-fabric over TDS — the same tool as the
simple example. The engine choice that differs between the four examples is
upstream, in bronze->silver; nothing materialises into a Warehouse except this.
"""
import os
import subprocess
import time

import web_store as web
from common import SQL_AUD, TDS_SERVER, load, log, tds_connect, token

st = load()
sql_tok = token(SQL_AUD)
PROJECT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "gold_star")

with open(os.path.join(PROJECT, "profiles.yml"), "w") as f:
    f.write(f"""contoso_gold_star:
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

env = {**os.environ, "DBT_PROFILES_DIR": PROJECT, "LAKEHOUSE_ID": st["lakehouse"]}
t0 = time.time()
rc = subprocess.run(["dbt", "build"], cwd=PROJECT, env=env).returncode
build_secs = time.time() - t0
assert rc == 0, f"dbt build failed: exit {rc}"

with tds_connect(st["warehouse"], sql_tok) as c:
    cur = c.cursor()
    lines = cur.execute("SELECT COUNT(*) FROM fct_order_lines").fetchone()[0]
    people = cur.execute("SELECT COUNT(*) FROM dim_customer").fetchone()[0]
    products = cur.execute("SELECT COUNT(*) FROM dim_product").fetchone()[0]
    by_channel = dict(cur.execute(
        "SELECT channel, SUM(amount) FROM fct_order_lines GROUP BY channel").fetchall())
    # A customer who bought in BOTH channels is the row that only exists because
    # resolution worked. If identity had stayed per-source, this count is zero
    # and every other total still looks correct.
    both = cur.execute("""
        SELECT COUNT(*) FROM (
            SELECT customer_key FROM fct_order_lines
            GROUP BY customer_key
            HAVING COUNT(DISTINCT channel) > 1
        ) x""").fetchone()[0]

assert len(by_channel) > 1, f"the star has only one channel: {by_channel}"
web_revenue = float(by_channel.get("web", 0))
assert abs(web_revenue - web.EXPECTED_WEB_REVENUE) < 0.01, \
    (web_revenue, web.EXPECTED_WEB_REVENUE)

log(f"gold star: dbt build green in {build_secs:.1f}s — {lines:,} order lines, "
    f"{people:,} customers, {products:,} products")
log("revenue by channel: " + ", ".join(
    f"{k}={float(v):,.2f}" for k, v in sorted(by_channel.items())))
log(f"{both:,} customers bought in more than one channel — a row that exists "
    f"only because identity was resolved across sources")
