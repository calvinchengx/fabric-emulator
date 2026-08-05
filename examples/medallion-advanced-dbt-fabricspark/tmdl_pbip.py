"""A8 — serialise the semantic model as TMDL, and lay out a .pbip project.

Step 10 published the model as **TMSL** — one `model.bim` JSON document, which
is what the REST API has always taken. This publishes the same model as
**TMDL**: a folder of indentation-structured text files, which is what Power BI
Desktop writes and what a `.pbip` project on disk actually contains.

Two things get proven here, and they are different:

  1. **The emulator accepts TMDL.** The item is created with `.tmdl` definition
     parts and no `model.bim` at all, then queried over `executeQueries`. If the
     DAX answers match step 10's, the emulator parsed TMDL into the same model.
  2. **A `.pbip` project is written to disk**, in the layout Power BI Desktop
     opens.

**What this does NOT do, stated plainly:** Power BI Desktop cannot connect to
this semantic model. Desktop reaches a published model over **XMLA via native
ADOMD.NET**, which the emulator does not serve and does not claim to — see
docs/18-semantic-model-references.md. The project written here carries its data
**imported**, so Desktop opens it and renders a real report against real rows,
offline. That is a genuinely useful artifact, and it is not a live connection.
"""
import base64
import json
import pathlib
import time

import source_system as src
from common import (
    FABRIC,
    PBI_AUD,
    S,
    ensure_app,
    fabric_headers,
    load,
    log,
    save,
    tds_connect,
    token,
)

st = load()
H = fabric_headers()
OUT = pathlib.Path(__file__).resolve().parent / "pbip"

# --- rows: the same gold export step 10 makes -------------------------------
with tds_connect(st["warehouse"]) as c:
    cur = c.cursor().execute(
        "SELECT order_date, country, orders, units, revenue FROM fct_daily_revenue")
    fact = [{"OrderDate": str(r[0])[:10], "Country": r[1], "Orders": int(r[2]),
             "Units": int(r[3]), "Revenue": float(r[4])} for r in cur.fetchall()]
    cur = c.cursor().execute("SELECT customer_id, name, country FROM dim_customer")
    dim = [{"CustomerId": r[0], "Name": r[1], "Country": r[2]} for r in cur.fetchall()]
assert fact and dim, (len(fact), len(dim))

# --- TMDL ---------------------------------------------------------------------
# Tabs, not spaces: TMDL is indentation-structured and Power BI writes tabs.
MODEL_TMDL = """model ContosoRevenueTMDL
\tculture: en-US
\tcompatibilityLevel: 1550
"""

CUSTOMER_TMDL = """table Customer

\tcolumn CustomerId
\t\tdataType: string
\t\tsourceColumn: CustomerId

\tcolumn Name
\t\tdataType: string
\t\tsourceColumn: Name

\tcolumn Country
\t\tdataType: string
\t\tsourceColumn: Country
"""

# The measures carry their DAX verbatim, including one written across several
# lines — that continuation is the part of the grammar a single-line fixture
# would never exercise.
REVENUE_TMDL = """table Revenue

\tcolumn OrderDate
\t\tdataType: string
\t\tsourceColumn: OrderDate

\tcolumn Country
\t\tdataType: string
\t\tsourceColumn: Country

\tcolumn Orders
\t\tdataType: int64
\t\tsourceColumn: Orders

\tcolumn Units
\t\tdataType: int64
\t\tsourceColumn: Units

\tcolumn Revenue
\t\tdataType: double
\t\tsourceColumn: Revenue

\tmeasure 'Total Revenue' = SUM(Revenue[Revenue])
\t\tformatString: #,0.00

\tmeasure 'Total Units' = SUM(Revenue[Units])

\tmeasure 'Revenue per Unit' =
\t\t\tDIVIDE([Total Revenue], [Total Units])
"""

RELATIONSHIPS_TMDL = """relationship Revenue_Customer
\tfromColumn: Revenue.Country
\ttoColumn: Customer.Country
"""

parts_src = {
    "definition/model.tmdl": MODEL_TMDL,
    "definition/tables/Customer.tmdl": CUSTOMER_TMDL,
    "definition/tables/Revenue.tmdl": REVENUE_TMDL,
    "definition/relationships.tmdl": RELATIONSHIPS_TMDL,
    # Rows ride in the same data.json the TMSL path uses: TMDL serialises the
    # MODEL, not its data, exactly as it does in a real .pbip.
    "data.json": json.dumps({"Customer": dim, "Revenue": fact}),
}


def part(path, text):
    return {"path": path, "payloadType": "InlineBase64",
            "payload": base64.b64encode(text.encode()).decode()}


r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/items", headers=H, json={
    "displayName": "contoso-revenue-tmdl", "type": "SemanticModel",
    "definition": {"parts": [part(p, t) for p, t in parts_src.items()]}})
if r.status_code in (200, 201):
    dataset = r.json()["id"]
else:
    assert r.status_code == 202, f"create TMDL model: {r.status_code} {r.text}"
    op = r.headers["Location"].rsplit("/", 1)[-1]
    for _ in range(60):
        status = S.get(f"{FABRIC}/v1/operations/{op}", headers=H).json()["status"]
        if status in ("Succeeded", "Failed"):
            break
        time.sleep(0.5)
    assert status == "Succeeded", status
    dataset = S.get(f"{FABRIC}/v1/operations/{op}/result", headers=H).json()["id"]
log(f"semantic model {dataset} created from TMDL parts — no model.bim in the definition")

# --- the proof: DAX over a model the emulator parsed from TMDL ---------------
ensure_app(PBI_AUD, "Power BI Service")
pt = token(PBI_AUD)
dax = ("EVALUATE SUMMARIZECOLUMNS(Customer[Country], "
       "\"Revenue\", [Total Revenue], \"PerUnit\", [Revenue per Unit])")
r = S.post(f"{FABRIC}/v1.0/myorg/groups/{st['workspace']}/datasets/{dataset}/executeQueries",
           headers={"Authorization": "Bearer " + pt}, json={"queries": [{"query": dax}]})
assert r.status_code == 200, f"executeQueries: {r.status_code} {r.text}"
rows = r.json()["results"][0]["tables"][0]["rows"]
assert rows, r.text

total = sum(row["[Revenue]"] for row in rows)
assert abs(total - src.EXPECTED_REVENUE) < 0.01, (total, src.EXPECTED_REVENUE)
countries = {row["Customer[Country]"] for row in rows}
assert countries == src.EXPECTED_COUNTRIES, countries
log(f"DAX over the TMDL model agrees with the TMSL one: {total:,.2f} "
    f"across {sorted(countries)}")

# --- the .pbip project on disk ------------------------------------------------
# The layout Power BI Desktop opens. `.pbip` is the entry point; the model and
# the report are sibling folders beside it.
model_dir = OUT / "ContosoRevenue.SemanticModel"
report_dir = OUT / "ContosoRevenue.Report"
(model_dir / "definition" / "tables").mkdir(parents=True, exist_ok=True)
report_dir.mkdir(parents=True, exist_ok=True)

for rel, text in parts_src.items():
    if not rel.endswith(".tmdl"):
        continue
    dest = model_dir / rel
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_text(text)

(model_dir / ".platform").write_text(json.dumps({
    "$schema": "https://developer.microsoft.com/json-schemas/fabric/gitIntegration/platformProperties/2.0.0/schema.json",
    "metadata": {"type": "SemanticModel", "displayName": "ContosoRevenue"},
    "config": {"version": "2.0", "logicalId": dataset}}, indent=2))

(report_dir / "definition.pbir").write_text(json.dumps({
    "version": "1.0",
    "datasetReference": {"byPath": {"path": "../ContosoRevenue.SemanticModel"}}},
    indent=2))

(OUT / "ContosoRevenue.pbip").write_text(json.dumps({
    "version": "1.0",
    "artifacts": [{"report": {"path": "ContosoRevenue.Report"}}],
    "settings": {"enableAutoRecovery": True}}, indent=2))

# The rows, beside the model, so the project is self-contained offline.
(model_dir / "data.json").write_text(parts_src["data.json"])

log(f"wrote .pbip project to {OUT.relative_to(pathlib.Path.cwd()) if OUT.is_relative_to(pathlib.Path.cwd()) else OUT}")
log("Power BI Desktop opens ContosoRevenue.pbip locally with this data IMPORTED — "
    "it does NOT connect to the emulator (that needs XMLA; see docs/18)")

save(tmdl_dataset=dataset)
