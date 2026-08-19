"""The Great Expectations layer — the tutorial's actual subject.

Runs in the locked uv environment with real `great_expectations` + `pandas`. Reads each of the
tutorial's assets from the emulator's `executeQueries` endpoint (the DAX golden
queries), loads the rows into a DataFrame, and runs the tutorial's Expectation
Suites. The pass/fail pattern must mirror the tutorial: Store / Measure pass,
the YoY-ratio DAX asset fails (a value out of the 0.8-1.5 band).

Adaptations from the literal tutorial (documented in README): the data source is
the emulator's executeQueries endpoint over a lakehouse-shaped model instead of
a Power BI semantic model over XMLA (no native ADOMD.NET client), and the valid-
zip check uses a built-in regex expectation instead of the third-party
`great_expectations_zipcode_expectations` plugin.
"""
import itertools
import json
import os
import re
import urllib.request

import great_expectations as gx
import great_expectations.expectations as gxe
import pandas as pd

FABRIC = os.environ["FABRIC_URL"]
WS = os.environ["WS"]
DS = os.environ["DATASET"]
TOKEN = os.environ["PBI_TOKEN"]
_names = itertools.count()


def friendly(col):
    """`Store[PostalCode]` / `[TotalUnits]` → `PostalCode` / `TotalUnits`."""
    m = re.search(r"\[([^\]]+)\]", col)
    return m.group(1) if m else col


def query(dax):
    url = f"{FABRIC}/v1.0/myorg/groups/{WS}/datasets/{DS}/executeQueries"
    req = urllib.request.Request(
        url, data=json.dumps({"queries": [{"query": dax}]}).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN}, method="POST")
    with urllib.request.urlopen(req) as r:
        resp = json.load(r)
    rows = resp["results"][0]["tables"][0]["rows"]
    df = pd.DataFrame(rows)
    df.columns = [friendly(c) for c in df.columns]
    return df


def batch(df):
    """A GE 1.x Batch over `df`, ready to validate single Expectations.

    Ported from 0.18's `ctx.get_validator(...)` + `v.expect_*()`, which 1.0
    removed: `context.sources` is now `context.data_sources`, an asset yields a
    BATCH DEFINITION rather than a batch request, and expectations are objects
    passed to `batch.validate(...)` instead of methods on a validator. The
    tutorial's shape is unchanged — one batch per asset, one result per
    expectation — because `validate()` returns the same
    ExpectationValidationResult, with `.success` and `partial_unexpected_list`.
    """
    ctx = gx.get_context()
    n = next(_names)
    bd = (ctx.data_sources.add_pandas(f"src{n}")
          .add_dataframe_asset(f"asset{n}")
          .add_batch_definition_whole_dataframe(f"batch{n}"))
    return bd.get_batch(batch_parameters={"dataframe": df})


# DAX per asset — single-sourced from the semantic-model golden fixture.
golden = json.load(open(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                     "..", "semantic-model", "fixtures", "golden_queries.json")))
dax = {q["name"]: q["dax"] for q in golden["queries"]}

results = []

# Retail Store Suite: row count in range + valid 5-digit zip.
b = batch(query(dax["Store Asset"]))
results.append(("Retail Store", "row_count_between(1,10)",
                b.validate(gxe.ExpectTableRowCountToBeBetween(min_value=1, max_value=10)).success))
results.append(("Retail Store", "valid_zip5(PostalCode)",
                b.validate(gxe.ExpectColumnValuesToMatchRegex(column="PostalCode", regex=r"^\d{5}$")).success))

# Retail Measure Suite: TotalUnits above threshold.
b = batch(query(dax["Total Units Asset"]))
results.append(("Retail Measure", "TotalUnits >= 50000",
                b.validate(gxe.ExpectColumnValuesToBeBetween(column="TotalUnits", min_value=50000)).success))

# Retail DAX Suite: the YoY ratio must be within band — the tutorial's failing asset.
b = batch(query(dax["Total Units YoY Asset"]))
ratio = b.validate(gxe.ExpectColumnValuesToBeBetween(
    column="Total Units Ratio", min_value=0.8, max_value=1.5))
results.append(("Retail DAX", "Total Units Ratio in [0.8, 1.5]", ratio.success))

for suite, exp, ok in results:
    print(f"{'PASS' if ok else 'FAIL'}  {suite}: {exp}", flush=True)

passed = {(s, e): ok for s, e, ok in results}
assert passed[("Retail Store", "row_count_between(1,10)")] is True
assert passed[("Retail Store", "valid_zip5(PostalCode)")] is True
assert passed[("Retail Measure", "TotalUnits >= 50000")] is True
# The YoY asset fails, exactly as in the tutorial — and 1.8 is the offending value.
assert passed[("Retail DAX", "Total Units Ratio in [0.8, 1.5]")] is False, "YoY ratio should fail"
unexpected = [float(x) for x in ratio.result.get("partial_unexpected_list", [])]
assert 1.8 in unexpected, f"expected 1.8 in the unexpected values, got {unexpected}"

print("\nGREAT-EXPECTATIONS E2E: PASS (Store/Measure pass; YoY ratio fails as in the tutorial)", flush=True)
