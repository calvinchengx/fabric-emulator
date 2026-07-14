"""The Great Expectations layer — the tutorial's actual subject.

Runs in a venv with real `great_expectations` + `pandas`. Reads each of the
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

import pandas as pd
import great_expectations as gx

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


def validator(df):
    ctx = gx.get_context()
    n = next(_names)
    src = ctx.sources.add_pandas(f"src{n}")
    asset = src.add_dataframe_asset(f"asset{n}")
    br = asset.build_batch_request(dataframe=df)
    return ctx.get_validator(batch_request=br, create_expectation_suite_with_name=f"suite{n}")


# DAX per asset — single-sourced from the semantic-model golden fixture.
golden = json.load(open(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                     "..", "semantic-model", "fixtures", "golden_queries.json")))
dax = {q["name"]: q["dax"] for q in golden["queries"]}

results = []

# Retail Store Suite: row count in range + valid 5-digit zip.
v = validator(query(dax["Store Asset"]))
results.append(("Retail Store", "row_count_between(1,10)",
                v.expect_table_row_count_to_be_between(min_value=1, max_value=10).success))
results.append(("Retail Store", "valid_zip5(PostalCode)",
                v.expect_column_values_to_match_regex("PostalCode", r"^\d{5}$").success))

# Retail Measure Suite: TotalUnits above threshold.
v = validator(query(dax["Total Units Asset"]))
results.append(("Retail Measure", "TotalUnits >= 50000",
                v.expect_column_values_to_be_between("TotalUnits", min_value=50000).success))

# Retail DAX Suite: the YoY ratio must be within band — the tutorial's failing asset.
v = validator(query(dax["Total Units YoY Asset"]))
ratio = v.expect_column_values_to_be_between("Total Units Ratio", min_value=0.8, max_value=1.5)
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
