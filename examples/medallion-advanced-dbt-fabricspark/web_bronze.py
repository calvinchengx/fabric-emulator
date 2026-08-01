"""A1 — bronze for the web channel: flatten the nested export, keep everything.

Read back from LANDING, not from the source module: landing is the record of
what the vendor sent, and every layer above it reads the record rather than the
sender.

The only reshaping is structural — an order's `lines` array becomes one row per
line, because Delta is tabular and JSON is not. Nothing is cleaned: the
cancelled order stays, the line pointing at a product that does not exist stays,
and `placed_at` stays a string with its original offset. Those are silver's
decisions to make, and silver has not run yet.

The step also pins down the OVERLAP with Contoso POS. A2 has to resolve two
customer sets into one, and a resolution step is only meaningful if the answer
was written down first.
"""
import json

import pandas as pd
from deltalake import DeltaTable, write_deltalake

import web_store as web
from common import FABRIC, S, STORAGE_AUD, load, log, storage_options, tables_uri, token

st = load()
ws, lake = st["workspace"], st["lakehouse"]
opts = storage_options()
base = tables_uri()
st_tok = token(STORAGE_AUD)


def landed(name):
    path = f"Files/landing/contoso_web/{st['web_landing_date']}/{name}"
    r = S.get(f"{FABRIC}/onelake/{ws}/{lake}/{path}",
              headers={"Authorization": "Bearer " + st_tok})
    assert r.status_code == 200, f"read {path}: {r.status_code} {r.text}"
    return json.loads(r.content)


customers = landed("customers.json")
products = landed("products.json")
orders = landed("orders.json")

assert len(customers) == web.EXPECTED_WEB_CUSTOMERS, len(customers)
assert len(products) == web.EXPECTED_WEB_PRODUCTS, len(products)
assert len(orders) == web.EXPECTED_WEB_ORDERS, len(orders)

# Flatten orders -> order lines. Order-level attributes ride down onto each
# line; nothing is dropped, so bronze can still reconstruct the order.
lines = pd.DataFrame([
    {"web_order_id": o["web_order_id"], "email": o["email"],
     "placed_at": o["placed_at"], "order_status": o["status"],
     "line_no": ln["line_no"], "product_id": ln["product_id"],
     "quantity": ln["quantity"], "unit_price": ln["unit_price"]}
    for o in orders for ln in o["lines"]])
assert len(lines) == web.EXPECTED_WEB_LINES, len(lines)

for name, frame in [("bronze_web_customers", pd.DataFrame(customers)),
                    ("bronze_web_products", pd.DataFrame(products)),
                    ("bronze_web_order_lines", lines)]:
    write_deltalake(f"{base}/{name}", frame, mode="overwrite", storage_options=opts)
    log(f"bronze: {name} ({len(frame)} rows)")

# --- the fixture's own arithmetic, checked once ------------------------------
# Not silver's job — this only confirms the oracle in web_store.py is
# self-consistent, the same role source_system.py's EXPECTED_* play.
known = {p["product_id"] for p in products}
unknown_product = ~lines["product_id"].isin(known)
assert int(unknown_product.sum()) == web.EXPECTED_WEB_UNKNOWN_PRODUCT_LINES
cancelled = lines["order_status"] == "cancelled"
assert lines[cancelled]["web_order_id"].nunique() == web.EXPECTED_WEB_CANCELLED_ORDERS
clean = lines[~cancelled & ~unknown_product]
assert len(clean) == web.EXPECTED_WEB_CLEAN_LINES, len(clean)
revenue = float((clean["quantity"] * clean["unit_price"]).sum())
assert abs(revenue - web.EXPECTED_WEB_REVENUE) < 0.01, revenue

# --- the designed overlap with Contoso POS -----------------------------------
pos = DeltaTable(f"{base}/bronze_customers", storage_options=opts).to_pandas()

# A share of POS addresses arrive capitalised as the customer typed them
# ("Ben.Okafor@Example.com"). Case-folding is therefore not cosmetic — without
# it those customers are silently two people each. Assert the raw forms do NOT
# all match, so the normalisation below is doing real work.
raw_pos = set(pos["email"].dropna())
web_emails = {c["email"].strip().lower() for c in customers}
assert raw_pos & web_emails != web_emails, "fixture no longer exercises case-folding"

# POS holds no email at all for part of its customer base: those people are not
# merely unmatched here, they are unmatchable on this key. A2 has to account for
# them rather than lose them.
missing_email = pos["email"].isna() | (pos["email"].astype(str).str.strip() == "")
assert int(missing_email.sum()) > 0, "the unmatchable-in-POS cohort vanished"

pos_emails = {e.strip().lower() for e in pos.loc[~missing_email, "email"]}

# Counts, not literal sets: at this scale the property under test is the SIZE of
# each cohort, not which addresses happen to land in it.
shared = pos_emails & web_emails
web_only = web_emails - pos_emails
assert len(shared) == web.EXPECTED_SHARED_EMAIL_COUNT, len(shared)
assert len(web_only) == web.EXPECTED_WEB_ONLY_EMAIL_COUNT, len(web_only)

pos_people = int(pos["customer_id"].nunique())
distinct = pos_people + len(web_only)

log(f"overlap: {len(shared):,} shared, {len(web_only):,} web-only, "
    f"{int(missing_email.sum()):,} unmatchable in POS -> {distinct:,} distinct people")
