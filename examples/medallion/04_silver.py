"""Silver: dedupe, conform, quarantine — the rules bronze deliberately does not
apply. Latest event wins per order, emails lowercased, country codes conformed,
malformed rows quarantined rather than dropped."""
import pandas as pd
from deltalake import DeltaTable, write_deltalake

import source_system as src
from common import log, storage_options, tables_uri

opts = storage_options()
base = tables_uri()

COUNTRY = {"US": "US", "USA": "US", "GB": "GB", "U.K.": "GB", "SG": "SG"}


def read(table):
    return DeltaTable(f"{base}/{table}", storage_options=opts).to_pandas()


c = read("bronze_customers").drop_duplicates(subset=["customer_id"], keep="last").copy()
c["email"] = c["email"].str.lower()
c["country"] = c["country"].str.upper().str.strip().map(lambda v: COUNTRY.get(v, v))
silver_customers = c[["customer_id", "name", "email", "country"]]

o = read("bronze_orders").sort_values("event_seq")
o = o.drop_duplicates(subset=["order_id"], keep="last").copy()  # latest event wins
bad = (o["quantity"] <= 0) | o["unit_price"].isna()
quarantine = o[bad].copy()
o = o[~bad].copy()
o["order_date"] = pd.to_datetime(o["order_date"])
o["amount"] = o["quantity"] * o["unit_price"]
silver_orders = o[["order_id", "customer_id", "order_date", "quantity",
                   "unit_price", "amount", "status"]]

write_deltalake(f"{base}/silver_customers", silver_customers, mode="overwrite",
                storage_options=opts)
write_deltalake(f"{base}/silver_orders", silver_orders, mode="overwrite",
                storage_options=opts)
write_deltalake(f"{base}/silver_quarantine_orders", quarantine, mode="overwrite",
                storage_options=opts)

assert len(silver_customers) == src.EXPECTED_SILVER_CUSTOMERS, len(silver_customers)
assert len(silver_orders) == src.EXPECTED_SILVER_ORDERS, len(silver_orders)
assert len(quarantine) == src.EXPECTED_QUARANTINED, len(quarantine)
assert set(silver_customers["country"]) == src.EXPECTED_COUNTRIES, set(silver_customers["country"])
assert silver_orders["order_id"].is_unique, "silver_orders still has duplicate order ids"
log(f"silver: {len(silver_customers)} customers, {len(silver_orders)} orders, "
    f"{len(quarantine)} quarantined")
