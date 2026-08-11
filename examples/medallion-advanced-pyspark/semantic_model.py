"""Publish gold as a SemanticModel item (TMSL + rows) and query it over the
Power BI executeQueries wire — the readiness check for Power BI clients.

In real Fabric the rows would arrive by Direct Lake; the emulator seeds them as
a `data.json` definition part, exported here straight from warehouse gold.
"""
import json

import source_system as src
from common import (
    FABRIC,
    FABRIC_AUD,
    PBI_AUD,
    S,
    create_item,
    ensure_app,
    fabric_headers,
    load,
    log,
    report_lineage,
    save,
    token,
)

st = load()
H = fabric_headers()

# The gold tables are read by the MODEL, not by this step. What used to happen
# here — SELECT every row over TDS and ship them inside the definition as a
# `data.json` part — was the least portable line in the example: `data.json` is
# the emulator's own inline row snapshot and real Fabric has no such part. So the
# one artifact a BI consumer actually reads was the one thing that could not be
# deployed to a tenant.
#
# A DIRECT LAKE partition is what Fabric uses instead: the model names the item's
# OneLake location and an entity within it, and the engine reads the rows itself.
# That works on both targets — a warehouse persists to OneLake as Delta on real
# Fabric, and the emulator reads the equivalent rows from the warehouse it serves
# (docs/46-artifact-persistence.md).
#
# The step still verifies the numbers, further down, by querying the model over
# executeQueries — which is a stronger check than shipping rows and asserting the
# rows you shipped.

model = {
    "name": "ContosoRevenue",
    # 1604, not 1550: Direct Lake needs it, and the emulator enforces that the
    # way Fabric does rather than reading the partition anyway. The bump is the
    # cost of the model reading gold itself instead of carrying a copy.
    "compatibilityLevel": 1604,
    "model": {
        "culture": "en-US",
        # The shared expression every Direct Lake partition points at.
        #
        # `onelake.dfs.fabric.microsoft.com` is written LITERALLY, not resolved per
        # target, and that is deliberate: it is Fabric's one OneLake host, identical
        # on every tenant, and the emulator parses the workspace/item out of it
        # rather than fetching it. So this expression is byte-identical on both
        # targets — the ids are the only thing that differs, as ever (docs/21). The
        # notebook definitions address OneLake the same way.
        "expressions": [{"name": "GoldWarehouse", "kind": "m", "expression": (
            'let Source = AzureStorage.DataLake("'
            f'https://onelake.dfs.fabric.microsoft.com/{st["workspace"]}/{st["warehouse"]}'
            '", [HierarchicalNavigation=true]) in Source')}],
        "tables": [
            {"name": "Customer", "columns": [
                {"name": "CustomerId", "dataType": "string", "sourceColumn": "customer_id"},
                {"name": "Name", "dataType": "string", "sourceColumn": "name"},
                {"name": "Country", "dataType": "string", "sourceColumn": "country"}],
             "partitions": [{"name": "Customer", "mode": "directLake", "source": {
                 "type": "entity", "entityName": "dim_customer", "schemaName": "dbo",
                 "expressionSource": "GoldWarehouse"}}]},
            {"name": "Revenue", "columns": [
                {"name": "OrderDate", "dataType": "string", "sourceColumn": "order_date"},
                {"name": "Country", "dataType": "string", "sourceColumn": "country"},
                {"name": "Orders", "dataType": "int64", "sourceColumn": "orders"},
                {"name": "Units", "dataType": "int64", "sourceColumn": "units"},
                {"name": "Revenue", "dataType": "double", "sourceColumn": "revenue"}],
             "measures": [
                 {"name": "Total Revenue", "expression": "SUM(Revenue[Revenue])"},
                 {"name": "Total Units", "expression": "SUM(Revenue[Units])"},
                 {"name": "Revenue per Unit",
                  "expression": "DIVIDE([Total Revenue], [Total Units])"}],
             "partitions": [{"name": "Revenue", "mode": "directLake", "source": {
                 "type": "entity", "entityName": "fct_daily_revenue", "schemaName": "dbo",
                 "expressionSource": "GoldWarehouse"}}]}],
        "relationships": [
            {"name": "Revenue_Customer", "fromTable": "Revenue", "fromColumn": "Country",
             "toTable": "Customer", "toColumn": "Country"}]},
}


# One resolver for the 202, and it lives in common.py. This script used to carry
# its own copy: a hard-indexed `x-ms-operation-id` and a flat one-second poll. A
# SemanticModel create always carries a definition, so it takes the async branch
# every run, against a service that states its own pace in `Retry-After`.
dataset = create_item("ContosoRevenue", "SemanticModel", {"model.bim": json.dumps(model)})
save(dataset=dataset)

# --- query it exactly as a Power BI REST client (or SemPy) would --------------
ensure_app(PBI_AUD, "Power BI Service")
pt = token(PBI_AUD)
dax = ('EVALUATE SUMMARIZECOLUMNS(Customer[Country], '
       '"Revenue", [Total Revenue], "PerUnit", [Revenue per Unit])')
r = S.post(f"{FABRIC}/v1.0/myorg/groups/{st['workspace']}/datasets/{dataset}/executeQueries",
           headers={"Authorization": "Bearer " + pt}, json={"queries": [{"query": dax}]})
assert r.status_code == 200, f"executeQueries: {r.status_code} {r.text}"
rows = r.json()["results"][0]["tables"][0]["rows"]
assert rows, r.text

total = sum(row["[Revenue]"] for row in rows)
assert abs(total - src.EXPECTED_REVENUE) < 0.01, rows
countries = {row["Customer[Country]"] for row in rows}
assert countries == src.EXPECTED_COUNTRIES, countries
log(f"semantic model {dataset}: DAX over executeQueries -> {rows}")

# A Power BI-audience token is required: the control-plane token must be refused.
r = S.post(f"{FABRIC}/v1.0/myorg/groups/{st['workspace']}/datasets/{dataset}/executeQueries",
           headers={"Authorization": "Bearer " + token(FABRIC_AUD)},
           json={"queries": [{"query": dax}]})
assert r.status_code == 401, f"wrong-audience token was accepted: {r.status_code}"
log("executeQueries rejects a non-Power BI audience token (401)")

# The last hop of the graph: gold → the semantic model Power BI reads.
#
# This model is an IMPORT model — the rows above were selected over TDS and
# embedded in the definition — so the emulator sees the bytes with no history
# attached and will not invent one. A Direct Lake model would need no help
# here: its binding names its source, and the emulator records that itself.
# This one says what it read, which is the same contract a notebook engine
# uses when it reports its own I/O.
#
# One movement per model table, because that is how the two SELECTs above run:
# Revenue is fct_daily_revenue and Customer is dim_customer, and neither table
# was built from the other's source.
_st = load()
report_lineage("semantic_model", [
    ([(_st["warehouse"], "Tables/fct_daily_revenue")], [(_st["dataset"], "Tables/Revenue")]),
    ([(_st["warehouse"], "Tables/dim_customer")], [(_st["dataset"], "Tables/Customer")]),
])
