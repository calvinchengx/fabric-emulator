"""Bronze: append landing verbatim into Delta, with lineage columns. Bronze
keeps everything — duplicates and the malformed row included."""
import io
import json

import pandas as pd
from deltalake import write_deltalake

import source_system as src
from common import (FABRIC, S, STORAGE_AUD, load, log, storage_options, tables_uri, token)

st = load()
opts = storage_options()


def read_landing(name):
    path = f"Files/landing/contoso_pos/{st['landing_date']}/{name}"
    r = S.get(f"{FABRIC}/onelake/{st['workspace']}/{st['lakehouse']}/{path}",
              headers={"Authorization": "Bearer " + token(STORAGE_AUD)})
    r.raise_for_status()
    return path, r.content


def stamp(df, source_path):
    df["_source_path"] = source_path
    df["_landing_date"] = st["landing_date"]
    return df


path, raw = read_landing("customers.csv")
customers = stamp(pd.read_csv(io.BytesIO(raw)), path)
path, raw = read_landing("orders.jsonl")
orders = stamp(pd.DataFrame(json.loads(l) for l in raw.decode().splitlines()), path)

base = tables_uri()
write_deltalake(f"{base}/bronze_customers", customers, mode="append", storage_options=opts)
write_deltalake(f"{base}/bronze_orders", orders, mode="append", storage_options=opts)

assert len(customers) == src.EXPECTED_BRONZE_CUSTOMERS, len(customers)
assert len(orders) == src.EXPECTED_BRONZE_ORDERS, len(orders)
log(f"bronze: {len(customers)} customer rows, {len(orders)} order events")
