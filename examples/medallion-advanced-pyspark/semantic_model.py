"""Publish gold as a SemanticModel item (TMSL + rows) and query it over the
Power BI executeQueries wire — the readiness check for Power BI clients.

In real Fabric the rows would arrive by Direct Lake; the emulator seeds them as
a `data.json` definition part, exported here straight from warehouse gold.
"""
import base64
import json
import time

import source_system as src
from common import (
    FABRIC,
    FABRIC_AUD,
    PBI_AUD,
    S,
    ensure_app,
    fabric_headers,
    load,
    log,
    report_lineage,
    save,
    tds_connect,
    token,
)

st = load()
H = fabric_headers()

# --- rows: export gold from the warehouse over TDS ---------------------------
with tds_connect(st["warehouse"]) as c:
    cur = c.cursor().execute(
        "SELECT order_date, country, orders, units, revenue FROM fct_daily_revenue")
    fact = [{"OrderDate": str(r[0])[:10], "Country": r[1], "Orders": int(r[2]),
             "Units": int(r[3]), "Revenue": float(r[4])} for r in cur.fetchall()]
    cur = c.cursor().execute("SELECT customer_id, name, country FROM dim_customer")
    dim = [{"CustomerId": r[0], "Name": r[1], "Country": r[2]} for r in cur.fetchall()]
assert fact and dim, (fact, dim)

model = {
    "name": "ContosoRevenue",
    "compatibilityLevel": 1550,
    "model": {
        "culture": "en-US",
        "tables": [
            {"name": "Customer", "columns": [
                {"name": "CustomerId", "dataType": "string", "sourceColumn": "CustomerId"},
                {"name": "Name", "dataType": "string", "sourceColumn": "Name"},
                {"name": "Country", "dataType": "string", "sourceColumn": "Country"}]},
            {"name": "Revenue", "columns": [
                {"name": "OrderDate", "dataType": "string", "sourceColumn": "OrderDate"},
                {"name": "Country", "dataType": "string", "sourceColumn": "Country"},
                {"name": "Orders", "dataType": "int64", "sourceColumn": "Orders"},
                {"name": "Units", "dataType": "int64", "sourceColumn": "Units"},
                {"name": "Revenue", "dataType": "double", "sourceColumn": "Revenue"}],
             "measures": [
                 {"name": "Total Revenue", "expression": "SUM(Revenue[Revenue])"},
                 {"name": "Total Units", "expression": "SUM(Revenue[Units])"},
                 {"name": "Revenue per Unit",
                  "expression": "DIVIDE([Total Revenue], [Total Units])"}]}],
        "relationships": [
            {"name": "Revenue_Customer", "fromTable": "Revenue", "fromColumn": "Country",
             "toTable": "Customer", "toColumn": "Country"}]},
}
data = {"Customer": dim, "Revenue": fact}


def part(path, obj):
    return {"path": path, "payloadType": "InlineBase64",
            "payload": base64.b64encode(json.dumps(obj).encode()).decode()}


r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/items", headers=H, json={
    "displayName": "ContosoRevenue", "type": "SemanticModel",
    "definition": {"parts": [part("model.bim", model), part("data.json", data)]}})
assert r.status_code in (201, 202), f"publish model: {r.status_code} {r.text}"
if r.status_code == 201:
    dataset = r.json()["id"]
else:
    op = r.headers["x-ms-operation-id"]
    for _ in range(60):
        status = S.get(f"{FABRIC}/v1/operations/{op}", headers=H).json()["status"]
        if status in ("Succeeded", "Failed"):
            break
        time.sleep(1)
    assert status == "Succeeded", f"publish operation {status}"
    dataset = S.get(f"{FABRIC}/v1/operations/{op}/result", headers=H).json()["id"]
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
