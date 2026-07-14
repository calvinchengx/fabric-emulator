# Great Expectations e2e

Real [Great Expectations](https://greatexpectations.io/) validating Fabric
semantic-model data through the emulator — the subject of the [SemPy + GX
tutorial](https://learn.microsoft.com/en-us/fabric/data-science/tutorial-great-expectations).

`run.py` stands up entra + fabric, publishes the golden model
([../semantic-model](../semantic-model/)), mints a Power BI-audience token, and
runs the tutorial's Expectation Suites (in a venv) against the emulator's
`executeQueries` endpoint. The pass/fail pattern mirrors the tutorial:

| Suite | Expectation | Result |
|---|---|---|
| Retail Store | row count in range | ✅ PASS |
| Retail Store | valid 5-digit `PostalCode` | ✅ PASS |
| Retail Measure | `TotalUnits ≥ 50000` | ✅ PASS |
| Retail DAX | `Total Units Ratio` in `[0.8, 1.5]` | ❌ **FAIL** — `1.8` (FY2014/Apr), exactly as in the tutorial |

Run: `python3 e2e/great-expectations/run.py`

## Documented adaptations from the literal tutorial

The GX *workflow* (Data Context → Data Assets → Expectation Suites → validation)
runs unchanged. Two things differ, both because of engine boundaries the emulator
is honest about:

1. **Data source** — the tutorial's GX Fabric Data Source reads a Power BI
   semantic model over **XMLA** via native `ADOMD.NET`, which can't be pointed at
   the emulator (see [../../docs/18-semantic-model-references.md](../../docs/18-semantic-model-references.md)).
   Here GX reads the same shape of data from the emulator's `executeQueries` REST
   endpoint over a lakehouse-shaped model. The Expectations are identical.
2. **Valid-zip check** — uses the built-in `expect_column_values_to_match_regex`
   (`^\d{5}$`) instead of the third-party
   `great_expectations_zipcode_expectations` plugin's `expect_column_values_to_be_valid_zip5`.
   Same assertion, no extra dependency.

3. **DMV asset** — the tutorial's fourth asset (`$SYSTEM.DISCOVER_STORAGE_TABLES`
   / `RIVIOLATION_COUNT`) is a schema rowset, not DAX. It is deferred (a separate
   handler); the model is clean, so it would pass trivially.
