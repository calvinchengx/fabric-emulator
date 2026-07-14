# Semantic-model golden fixture + seed flow

The last golden reference to pin before any DAX/semantic-model engine work: a
small, hand-computable model that mirrors the SemPy + Great Expectations
tutorial, plus the exact `executeQueries` request/response the engine must
satisfy. See [../../docs/16-semantic-model-references.md](../../docs/16-semantic-model-references.md)
for why this contract (not XMLA) is the tractable one, and
[../../third_party/powerbi-rest-swagger/](../../third_party/powerbi-rest-swagger/)
for the vendored golden OpenAPI.

## Files

- `fixtures/retail.bim` — the golden **TMSL** model (Store / Time / Sales +
  measures). This is exactly what the SemanticModel item's definition holds.
- `fixtures/seed_data.json` — the table rows (import data; real Fabric would
  Direct-Lake these from OneLake Delta). Kept tiny so goldens are hand-computed.
- `fixtures/golden_queries.json` — the **DAX oracle**: `(query → executeQueries rows)`
  pairs. macOS has no live DAX engine (Power BI Desktop / DAX Studio are
  Windows-only), so these are computed by hand — arithmetic below.
- `run.py` — the e2e: stands up entra + fabric, publishes the model + data as a
  SemanticModel item, mints a Power BI-audience token, and POSTs each golden DAX
  query to `executeQueries`, asserting the rows match the oracle.

## The model, and why it's shaped this way

Measures are plain `SUM` + `DIVIDE` over `UnitsThisYear` / `UnitsLastYear`
**columns** rather than time-intelligence, so the whole fixture is
hand-computable and the DAX subset stays small — while faithfully reproducing
the tutorial's query shapes and its one failing asset. `Sales` relates to
`Store` (StoreId) and `Time` (MonthKey).

## Hand-computed goldens (the oracle)

Two `Sales` rows per `Time` group; grouped by `Time[FiscalYear]`,`[FiscalMonth]`:

| MonthKey | FiscalYear/Month | Units (Σ) | ThisYear (Σ) | LastYear (Σ) | Ratio = TY ÷ LY |
|---|---|---|---|---|---|
| 201301 | FY2013 / Jan | 30000+30000 = **60000** | 500+500 = **1000** | **1000** | **1.0** |
| 201304 | FY2013 / Apr | 32500+32500 = **65000** | 600+600 = **1200** | **800** | **1.5** |
| 201401 | FY2014 / Jan | 35000+35000 = **70000** | 450+450 = **900** | **1000** | **0.9** |
| 201404 | FY2014 / Apr | 40000+40000 = **80000** | 900+900 = **1800** | **1000** | **1.8** ← out of band |

`EVALUATE 'Store'` → the 4 Store rows verbatim.

## The `executeQueries` contract (from the vendored swagger)

```
POST /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/executeQueries
Authorization: Bearer <Power BI-audience token>
{ "queries": [ { "query": "EVALUATE ..." } ] }
```
- `groupId` = workspace id, `datasetId` = SemanticModel item id (the only URL params).
- Response `200`: `{ "results": [ { "tables": [ { "rows": [ … ] } ] } ] }`, one result per query. Column keys: `Table[Column]` and `[Measure Name]` — as in `golden_queries.json`.

## DAX subset the engine must implement (scoped to this fixture)

- `EVALUATE <table>` — return all rows/columns of a table.
- `SUMMARIZECOLUMNS(groupCol, …, "Name", <expr>, …)` — group-by + named expressions.
- Measure references `[TotalUnits]`, and their definitions.
- `SUM(Table[Column])`, `DIVIDE(a, b)`.
- Relationship traversal for filter context (`Sales` filtered by `Time` / `Store`).

Out of scope for the first cut (documented deferrals):
- The **DMV asset** (`$SYSTEM.DISCOVER_STORAGE_TABLES`, `RIVIOLATION_COUNT`) is a
  schema rowset, not DAX `EVALUATE` — a separate handler; our clean model yields
  all-zero violation counts.
- Time-intelligence, `CALCULATE` filter modifiers, and the full DAX library.

## Great Expectations mapping (mirrors the tutorial)

The GX workflow runs unchanged over the `executeQueries` rows; only the data
source swaps (Power BI model → this endpoint) and the numeric bounds are fit to
the small fixture:

| GX suite (tutorial) | Expectation | Over our data | Result |
|---|---|---|---|
| Retail Store | `valid_zip5(PostalCode)` + `row_count_between(1,10)` | 4 valid zips | ✅ pass |
| Retail Measure | `column_values_between(TotalUnits, min=50000)` | 60k–80k | ✅ pass |
| Retail DAX | `column_values_between([Total Units Ratio], 0.8, 1.5)` | 1.0/1.5/0.9/**1.8** | ❌ fails on FY2014/Apr — mirrors the tutorial |
| Retail DMV | `values_in_set(RIVIOLATION_COUNT, [0])` | all 0 | ✅ pass (handler pending) |

## Run

```sh
python3 e2e/semantic-model/run.py
```
Passes today: real Power BI-audience token → `executeQueries` → the three DAX
golden queries return rows matching `golden_queries.json`. The Great Expectations
layer (`e2e/great-expectations/`) validates these same rows.
