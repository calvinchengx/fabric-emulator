"""Close the loop: read back what those runs produced — twice, two ways.

This is the half of the example that runs on YOUR machine rather than in the
`fab` container, and it is where the hybrid earns its keep.

  1. `fab ls` reports the tables. That is Microsoft's client agreeing the
     control plane knows about them.
  2. delta-rs reads the actual Delta bytes out of OneLake and counts rows. That
     is an independent reader agreeing the DATA is there.

Either alone is weak. A control plane can list a table it never wrote; a Delta
log can exist in storage that the catalogue never heard of. The counts asserted
below come from the fixture GENERATOR — it knows how many duplicates it planted
— so silver has to independently arrive at the same number.
"""
import source_system as src
from common import ensure_app, storage_options
from deltalake import DeltaTable

import fabctl as fab

ws, lake = fab.item_id(fab.WORKSPACE), fab.item_id(fab.LAKEHOUSE)

# --- 1. what Microsoft's client can see ------------------------------------
listing = fab.run("ls", f"{fab.LAKEHOUSE}/Tables")
for table in ("bronze_customers", "silver_customers"):
    assert table in listing, f"fab ls does not list {table}:\n{listing}"
fab.log(f"fab ls {fab.LAKEHOUSE}/Tables -> bronze_customers, silver_customers")

# --- 2. what an independent Delta reader can see ---------------------------
ensure_app("https://storage.azure.com", "Azure Storage")
opts = storage_options()
base = f"az://{ws}/{lake}/Tables"

bronze = DeltaTable(f"{base}/bronze_customers", storage_options=opts).to_pyarrow_table()
silver = DeltaTable(f"{base}/silver_customers", storage_options=opts).to_pyarrow_table()

assert bronze.num_rows == src.EXPECTED_BRONZE_CUSTOMERS, (
    f"bronze_customers has {bronze.num_rows:,} rows, "
    f"the generator planted {src.EXPECTED_BRONZE_CUSTOMERS:,}"
)
assert silver.num_rows == src.EXPECTED_SILVER_CUSTOMERS, (
    f"silver_customers has {silver.num_rows:,} rows, "
    f"the generator expects {src.EXPECTED_SILVER_CUSTOMERS:,} after dedupe"
)
fab.log(f"bronze_customers {bronze.num_rows:,} rows "
        f"-> silver_customers {silver.num_rows:,} "
        f"({bronze.num_rows - silver.num_rows:,} duplicates removed)")

# The conformance rule really ran: five spellings across three countries went in,
# three canonical codes came out. A row count alone would pass on a silver table
# that had only been copied.
countries = set(silver.column("country").to_pylist())
assert countries == src.EXPECTED_COUNTRIES, (
    f"silver countries {sorted(countries)} != {sorted(src.EXPECTED_COUNTRIES)}"
)
fab.log(f"country conformed to {sorted(countries)}")

emails = silver.column("email").to_pylist()
assert all(e == e.lower() for e in emails), "silver still carries mixed-case email"
fab.log(f"all {len(emails):,} emails case-folded")

# --- 3. the lineage the emulator recorded on its own -----------------------
# Read through `fab api`, so even the raw REST call goes out over Microsoft's
# client and its auth.
#
# The Copy edge is the one worth asserting: the emulator EXECUTED that activity,
# so the edge is something it witnessed rather than something a client asked it
# to believe. `producer` is the field that carries that distinction, and an
# example that ignored it would present a self-reported graph as evidence.
edges = fab.api_rows(f"workspaces/{ws}/lineage")
assert edges, "the workspace recorded no lineage at all"

producers = sorted({e.get("producer", "") for e in edges})
copy_edges = [e for e in edges
              if e.get("producer") == "Copy"
              and e.get("targetPath", "").endswith("bronze_customers")]
assert copy_edges, (
    f"no Copy edge into bronze_customers — producers seen: {producers}. "
    f"The emulator ran that activity itself, so its absence means the "
    f"observation path is broken, not that the data is missing."
)
fab.log(f"{len(edges)} lineage edge(s), producers {producers}")
fab.log(f"bronze_customers has a Copy edge — observed, not reported")
