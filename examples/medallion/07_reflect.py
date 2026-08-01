"""Reflect silver into the lakehouse SQL analytics endpoint.

Connecting to the lakehouse database makes the emulator read each Tables/<t>
Delta and reflect it into the SQL engine — so silver becomes queryable T-SQL,
read-only, refreshed on every connect.
"""
import time

import source_system as src
from common import SQL_AUD, ensure_app, load, log, tds_connect, token

st = load()
ensure_app(SQL_AUD, "Azure SQL")
sql_tok = token(SQL_AUD)

# The first connect makes the emulator create and start the per-item database on
# the sidecar, which can be slow — retry until it is online.
last = None
for attempt in range(40):
    try:
        with tds_connect(st["lakehouse"], sql_tok, timeout=15) as c:  # connect = reflect
            rows = c.cursor().execute(
                "SELECT status, COUNT(*) AS n, SUM(amount) AS revenue "
                "FROM silver_orders GROUP BY status ORDER BY status").fetchall()
        break
    except Exception as e:  # noqa: BLE001 — transient while the database comes online
        last = e
        time.sleep(3)
else:
    raise SystemExit(f"lakehouse reflection failed after retries: {last}")

total = sum(float(r[2]) for r in rows)
count = sum(int(r[1]) for r in rows)
assert count == src.EXPECTED_SILVER_ORDERS, rows
assert abs(total - src.EXPECTED_REVENUE) < 0.01, rows
log(f"reflection: {[tuple(r) for r in rows]} (attempt {attempt + 1})")
