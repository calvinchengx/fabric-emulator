"""Prove the data-quality gate is real.

A contract that never fails is not a contract. Poison silver with a duplicate,
negative-amount order, rebuild, and require dbt to REJECT it — then restore
silver and require gold to go green again.
"""
import os
import subprocess

import pandas as pd
from deltalake import DeltaTable, write_deltalake

from common import GOLD_PROJECT, load, log, storage_options, tables_uri, tds_connect

st = load()
opts = storage_options()
url = f"{tables_uri()}/silver_orders"
env = {**os.environ, "DBT_PROFILES_DIR": GOLD_PROJECT, "LAKEHOUSE_ID": st["lakehouse"]}


def rebuild():
    with tds_connect(st["lakehouse"]):  # re-reflect whatever silver now holds
        pass
    return subprocess.run(["dbt", "build"], cwd=GOLD_PROJECT, env=env).returncode


good = DeltaTable(url, storage_options=opts).to_pandas()
poisoned = pd.concat([good, good.head(1).assign(amount=-5.0)], ignore_index=True)
write_deltalake(url, poisoned, mode="overwrite", storage_options=opts)
try:
    assert rebuild() != 0, "dbt build passed on data that violates the gold contract"
    log("DQ gate verified: dbt build fails on a duplicate + negative-amount order")
finally:
    write_deltalake(url, good, mode="overwrite", storage_options=opts)
    assert rebuild() == 0, "gold did not return to green after restoring silver"
    log("silver restored; gold green again")
