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

import source_system as src
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
rc = subprocess.run(["dbt", "--no-partial-parse", "build"], cwd=PROJECT, env=env).returncode
build_secs = time.time() - t0
assert rc == 0, f"dbt build failed: exit {rc}"

with tds_connect(st["warehouse"], sql_tok) as c:
    cur = c.cursor()
    lines = cur.execute("SELECT COUNT(*) FROM fct_order_lines").fetchone()[0]
    people = cur.execute("SELECT COUNT(*) FROM dim_customer_360").fetchone()[0]
    products = cur.execute("SELECT COUNT(*) FROM dim_product").fetchone()[0]

    # Every count above is checked below, and none of them was before. They were
    # printed, which is not the same thing: a star that dropped half its POS
    # orders on a bad join, or a dimension that quietly became POS-only, prints
    # a smaller number and nothing else changes.
    by_source = dict(cur.execute(
        "SELECT source_system, COUNT(*) FROM fct_order_lines "
        "GROUP BY source_system").fetchall())

    # The identity count, from the table dim_customer_360 was NOT built from.
    # star_silver.py writes silver_customer_conformed by full-outer joining the
    # three sources and silver_customer_xref by unioning them — different code
    # paths over the same resolution, so an identity dropped or invented in
    # either shows up as a disagreement here. dim_customer_360 selects straight from
    # conformed, so comparing it against conformed would prove nothing.
    xref_identities = cur.execute(
        f"SELECT COUNT(DISTINCT customer_key) FROM [{st['lakehouse']}]"
        f".dbo.silver_customer_xref").fetchone()[0]

    # The referential break must still be broken. web_store.py emits lines
    # against products no catalogue carries; silver quarantines them, so they
    # must not have arrived in the dimension by way of dim_product's union with
    # what the facts reference. If they did, the star would be reporting revenue
    # for products that do not exist.
    unknown = cur.execute(
        "SELECT COUNT(*) FROM dim_product WHERE product_id IN ({})".format(
            ", ".join(f"'{p}'" for p in web.UNKNOWN_PRODUCT_IDS))).fetchone()[0]
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
    #
    # It is also CHECKED, which it was not before. The channel bug survived
    # because this number had no oracle: it read as proof that the example's
    # whole thesis had been demonstrated, and nothing anywhere would have
    # disagreed with any value it took. A wrong number that fails is a bug; a
    # wrong number that reads as proof is the thing this example is about.
    #
    # `wrong_key` counts the customers here that the RESOLUTION does not agree
    # are multi-source — a different step's answer, from silver_customer_
    # conformed's survivorship, rather than this query grading its own work.
    # Under the channel bug it would have been large, because a POS-only
    # customer who bought in store and on the web has source_count = 1.
    both, wrong_key = cur.execute("""
        SELECT COUNT(*),
               SUM(CASE WHEN d.source_count > 1 THEN 0 ELSE 1 END)
        FROM (
            SELECT customer_key FROM fct_order_lines
            GROUP BY customer_key
            HAVING COUNT(DISTINCT source_system) > 1
        ) f
        LEFT JOIN dim_customer_360 d ON d.customer_key = f.customer_key""").fetchone()
    # The same cohort, counted by naming the two systems instead of counting
    # distinct ones. A shape that cannot agree with the buggy one by accident:
    # it also fails if ERP ever starts contributing fact rows, which
    # COUNT(DISTINCT source_system) would absorb without a word.
    pos_and_web = cur.execute("""
        SELECT COUNT(*) FROM (
            SELECT customer_key FROM fct_order_lines
            GROUP BY customer_key
            HAVING MAX(CASE WHEN source_system = 'contoso_pos' THEN 1 ELSE 0 END) = 1
               AND MAX(CASE WHEN source_system = 'contoso_web' THEN 1 ELSE 0 END) = 1
        ) x""").fetchone()[0]

assert len({c for _, c in by_channel} ) > 1, f"the star has only one channel: {by_channel}"

# --- the three counts the log line used to report unchecked --------------------
#
# LINES, per source. The POS side travels through silver_customer_xref on an
# INNER join, so a customer who failed to resolve takes their orders out of the
# star with them — silently, and the remaining totals stay plausible. Counting
# the survivors against silver's own oracle is what fct_order_lines.sql says
# should happen; it said so next to a dbt schema test that was never written.
pos_lines = by_source.get("contoso_pos", 0)
web_lines = by_source.get("contoso_web", 0)
assert pos_lines == src.EXPECTED_SILVER_ORDERS, \
    (pos_lines, src.EXPECTED_SILVER_ORDERS,
     "POS orders vanished between silver and the star — the xref inner join")
assert web_lines == web.EXPECTED_WEB_CLEAN_LINES, \
    (web_lines, web.EXPECTED_WEB_CLEAN_LINES)
# And nothing else is in the fact. A third source_system appearing here would
# pass both counts above and change every aggregate in the star.
assert lines == pos_lines + web_lines, (lines, sorted(by_source))

# PEOPLE. Not a row count for its own sake: the claim dim_customer_360 makes
# is that it spans three systems rather than being a POS dimension wearing a
# general name, and that is only true if it holds MORE than POS ever knew.
assert people == xref_identities, \
    (people, xref_identities,
     "dim_customer_360 and silver_customer_xref disagree on how many people exist")
assert people > src.EXPECTED_SILVER_CUSTOMERS, \
    (people, src.EXPECTED_SILVER_CUSTOMERS,
     "dim_customer_360 holds no more people than POS alone — resolution added nobody")

# PRODUCTS. Both source systems transact the same eight catalogue ids, so the
# dimension is exactly the catalogue; the uncatalogued path in dim_product.sql
# is real but unexercised at this fixture size, and saying so here is better
# than a number that could be anything.
assert products == web.EXPECTED_WEB_PRODUCTS, (products, web.EXPECTED_WEB_PRODUCTS)
assert unknown == 0, \
    (f"{unknown} quarantined products reached dim_product: "
     f"{web.UNKNOWN_PRODUCT_IDS} are referential breaks, not products")

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

# The cross-source cohort is a CLAIM, so it gets assertions rather than a log
# line. Non-empty first: if resolution silently stopped working, every total
# above still balances and only this goes to zero.
assert both > 0, "no customer buys from two source systems — resolution did nothing"
assert int(wrong_key or 0) == 0, \
    (f"{wrong_key} customers counted as multi-source in the fact are not "
     f"multi-source in dim_customer_360 — the fact is counting something else")
assert both == pos_and_web, (both, pos_and_web,
                             "two shapes of the same question disagree")

log(f"gold star: dbt build green in {build_secs:.1f}s — {lines:,} order lines, "
    f"{people:,} customers, {products:,} products")
log("revenue by source and channel: " + ", ".join(
    f"{s}/{c}={float(v):,.2f}" for (s, c), v in sorted(by_channel.items())))
log(f"{both:,} customers bought from more than one SOURCE SYSTEM — a row that exists "
    f"only because identity was resolved across sources")

# --- what compare.py reads ----------------------------------------------------
# The star is the far end of the pipeline, so agreeing here is the strongest form
# of "the silver engine did not change the answer": everything between silver and
# this table had to agree too, or these numbers would not.
#
# Revenue is carried per (source_system, channel) rather than as one total. A
# single figure can match while the split behind it is wrong — which is exactly
# what happened when 'web' meant both a POS channel and a source system, and the
# grand total was the one number that still looked right.
import json  # noqa: E402
# --- the cutover, exercised ------------------------------------------------
# The build above created gold from nothing, which takes the materialization's
# first-build branch: one rename, no live table to protect. The interesting
# branch is the SECOND build, where a live table already exists and has to be
# swapped out from under readers.
#
# gold_star/macros/table_atomic_swap.sql puts both renames in one transaction
# so that swap is atomic. Without this rebuild that macro would ship untested —
# present in the project, never executed, and indistinguishable from working.
log("rebuilding gold to exercise the atomic cutover")
rc = subprocess.run(["dbt", "--no-partial-parse", "build"], cwd=PROJECT, env=env).returncode
assert rc == 0, f"dbt rebuild failed: exit {rc}"

with tds_connect(st["warehouse"], sql_tok) as c:
    cur = c.cursor()
    again = cur.execute("SELECT COUNT(*) FROM fct_order_lines").fetchone()[0]
    # The table survived the swap with its contents intact. A cutover that
    # left the backup in place, or committed a half-built temp table, shows up
    # here as a different number rather than as an error.
    assert again == lines, f"fct_order_lines has {again:,} rows after the swap, had {lines:,}"
    # The scaffolding is gone. A leftover __dbt_backup means the transaction
    # renamed the old table out and never dropped it, which is a slow leak of
    # one full copy of gold per rebuild.
    leftovers = [r[0] for r in cur.execute(
        "SELECT name FROM sys.tables WHERE name LIKE '%__dbt_backup' OR name LIKE '%__dbt_temp'").fetchall()]
    assert not leftovers, f"swap left scaffolding behind: {leftovers}"
log(f"atomic cutover: {again:,} rows intact, no scaffolding left")

import pathlib  # noqa: E402

_here = pathlib.Path(__file__).resolve().parent
_here.joinpath("gold_star_summary.json").write_text(json.dumps({
    "example": _here.name,
    "rows": {"fct_order_lines": lines, "dim_customer_360": people,
             "dim_product": products},
    "lines_by_source": by_source,
    "revenue_by_source_and_channel": {f"{s}/{c}": round(float(v), 2)
                                      for (s, c), v in sorted(by_channel.items())},
    "multi_source_customers": both,
}, indent=2))
