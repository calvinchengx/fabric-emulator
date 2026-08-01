"""Interactive checkpoint — eyeball the layers in VS Code Data Wrangler.

Run these cells in the VS Code Interactive Window (Shift+Enter on a `# %%`
cell), then in the Variables panel right-click a DataFrame ->
"View Value in Data Wrangler".

Not part of run_all.py: this step is for looking, not asserting.
"""
# %%
from deltalake import DeltaTable

from common import storage_options, tables_uri

opts = storage_options()
base = tables_uri()

# %% bronze vs silver, side by side
bronze_orders = DeltaTable(f"{base}/bronze_orders", storage_options=opts).to_pandas()
silver_orders = DeltaTable(f"{base}/silver_orders", storage_options=opts).to_pandas()
silver_customers = DeltaTable(f"{base}/silver_customers", storage_options=opts).to_pandas()
quarantine = DeltaTable(f"{base}/silver_quarantine_orders", storage_options=opts).to_pandas()

# What to check in Data Wrangler's profiling column:
#   bronze_orders    255,000 rows -> silver_orders 247,500: the redelivered
#                    events collapsed and the malformed ones were quarantined
#   silver_customers 100,000 rows x 101 columns; country is exactly
#                    {US, GB, SG} — all fifteen raw spellings conformed
#   silver_orders    amount has no nulls; order_date is a datetime
#
# At this size the profiler is describing a real distribution rather than a
# handful of hand-written rows, which is when its histograms and null counts
# start being worth reading.
print(f"bronze={len(bronze_orders)} silver={len(silver_orders)} "
      f"quarantined={len(quarantine)} customers={len(silver_customers)}")
