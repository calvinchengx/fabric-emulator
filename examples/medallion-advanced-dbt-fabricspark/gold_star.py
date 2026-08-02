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
    # BY (source_system, channel), not channel alone. "web" is a POS channel AND
    # the name of a separate source system, so grouping on the word merges
    # 50,871 POS web-channel orders into the web store's total. The collision is
    # real data, not a modelling slip, and the star keeps both meanings apart.
    by_channel = {(s, c): v for s, c, v in cur.execute(
        "SELECT source_system, channel, SUM(amount) FROM fct_order_lines "
        "GROUP BY source_system, channel").fetchall()}
    # A customer who bought from more than one SOURCE SYSTEM is the row that
    # only exists because resolution worked.
    #
    # This counted distinct CHANNEL until the same collision that inflated web
    # revenue was found above. POS alone has five channels, so a POS-only
    # customer who bought in store and on the web scored as multi-channel — and
    # the log line below then credited identity resolution for someone it never
    # had to resolve. The number was large and entirely wrong about its subject.
    # Source system is what the comment always meant.
    both = cur.execute("""
        SELECT COUNT(*) FROM (
            SELECT customer_key FROM fct_order_lines
            GROUP BY customer_key
            HAVING COUNT(DISTINCT source_system) > 1
        ) x""").fetchone()[0]

assert len({c for _, c in by_channel} ) > 1, f"the star has only one channel: {by_channel}"

# The web STORE's revenue — its own source system, not every line labelled web.
web_revenue = float(by_channel.get(("contoso_web", "web"), 0))
assert abs(web_revenue - web.EXPECTED_WEB_REVENUE) < 0.01, \
    (web_revenue, web.EXPECTED_WEB_REVENUE)

# And POS's own web channel is still there, distinct and non-empty. Asserting it
# exists is what stops a future "simplification" from folding the two back into
# one bucket and quietly restoring the 15,093,542.80 overcount.
pos_web = float(by_channel.get(("contoso_pos", "web"), 0))
assert pos_web > 0, ("POS web-channel revenue vanished — the two meanings of "
                     "'web' have been merged again", sorted(by_channel))

log(f"gold star: dbt build green in {build_secs:.1f}s — {lines:,} order lines, "
    f"{people:,} customers, {products:,} products")
log("revenue by source and channel: " + ", ".join(
    f"{s}/{c}={float(v):,.2f}" for (s, c), v in sorted(by_channel.items())))
log(f"{both:,} customers bought from more than one SOURCE SYSTEM — a row that exists "
    f"only because identity was resolved across sources")
