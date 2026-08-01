"""A7 — run the data contracts as gates, at every layer.

The contracts in `contracts/` have described this pipeline since they were
written, and until now nothing executed them. This step does, at each boundary:

    landing  -> contoso-pos, contoso-web, contoso-erp, reference
    silver   -> silver-sales
    gold     -> gold-sales

A gate that only reports is not a gate, so a violation raises. The last section
proves that by breaking a rule ON PURPOSE and requiring the failure: a check
that has never been seen to fail is a check nobody should trust.

Note which layer gets which kind of rule. Landing is stored verbatim, so its
contracts assert only ARRIVAL — a row count above zero, a unique capture
sequence. Asserting cleanliness there would be asserting something we have no
right to, because the vendor never promised it. Cleanliness becomes assertable
at silver, which is the first layer that promised anything.
"""
import pandas as pd
from deltalake import DeltaTable

import contract_gate
from common import load, log, storage_options, tables_uri, tds_connect

opts = storage_options()
base = tables_uri()


def read(table):
    return DeltaTable(f"{base}/{table}", storage_options=opts).to_pandas()


# --- landing: arrival only ---------------------------------------------------
# Read the bronze tables rather than the landed files: bronze is a faithful,
# unreshaped copy of what arrived, and it is already tabular. The one exception
# is the web feed, whose landing form is nested JSON that bronze flattened.
checked = 0
checked += contract_gate.validate(read("bronze_customers"), "landing-contoso-pos", "customers")
checked += contract_gate.validate(read("bronze_erp_changes"), "landing-contoso-erp", "changes")
checked += contract_gate.validate(read("bronze_fx_rates"), "reference-data", "fx_rates")
checked += contract_gate.validate(read("bronze_product_hierarchy"), "reference-data",
                                  "product_hierarchy")

# --- silver: the first layer that promised anything --------------------------
checked += contract_gate.validate(read("silver_customers"), "silver-sales", "silver_customers")
checked += contract_gate.validate(read("silver_orders"), "silver-sales", "silver_orders")

log(f"{checked} contract rules satisfied across landing and silver")

# --- gold: the rules that need a query engine --------------------------------
# gold-sales carries two `type: sql` rules, because ODCS's library has no
# referential-integrity metric (docs/30). Until now those were covered only BY
# PROXY — dbt runs equivalent tests in 08 — which meant the contract itself was
# never executed and could drift from the dbt models without anything noticing.
# They run here against the warehouse, over the same TDS the models were built
# through.
sql_checked = 0
with tds_connect(load()["warehouse"]) as conn:
    for element in ("dim_customer", "fct_orders", "fct_daily_revenue"):
        sql_checked += contract_gate.validate_sql(conn, "gold-sales", element)
log(f"{sql_checked} sql contract rule(s) executed against gold — "
    f"the contract now runs, rather than being covered by proxy")

# --- the gate must be able to FAIL -------------------------------------------
# Poison a copy in memory — the same shape of proof dq_gate.py gives for
# dbt, applied to the contracts. Two distinct violations, so this cannot pass
# by accidentally catching only one class of rule.
silver = read("silver_customers")

broken = silver.copy()
broken.loc[broken.index[:5], "country"] = "Republic of Nowhere"  # conformity
try:
    contract_gate.validate(broken, "silver-sales", "silver_customers", verbose=False)
    raise SystemExit("FAIL: the contract gate accepted an unconformed country")
except contract_gate.ContractViolation as e:
    assert "invalidValues[country]" in str(e), e
    log("gate rejects an unconformed country, as silver-sales requires")

broken = pd.concat([silver, silver.head(3)], ignore_index=True)  # uniqueness + rowCount
try:
    contract_gate.validate(broken, "silver-sales", "silver_customers", verbose=False)
    raise SystemExit("FAIL: the contract gate accepted duplicate customer_ids")
except contract_gate.ContractViolation as e:
    assert "duplicateValues" in str(e) or "unique[customer_id]" in str(e), e
    log("gate rejects duplicate customer_ids, as silver-sales requires")

log("contract gates are enforced, and demonstrably able to fail")
