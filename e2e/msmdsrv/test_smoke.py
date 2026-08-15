"""The half of the pump smoke that does not need Desktop."""
from __future__ import annotations

import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from smoke import FIELDS, KEY, json_rows

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent / "pbix-desktop"))
from verify import compare


def test_json_rows_match_expected_shape():
    payload = {
        "rows": [
            {"Customer[Country]": "GB", "[Revenue]": 25227674.699999996, "[PerUnit]": 101.72941714921689},
            {"Customer[Country]": "SG", "[Revenue]": 24819216.2, "[PerUnit]": 100.46638681994818},
            {"Customer[Country]": "US", "[Revenue]": 25135455.200000003, "[PerUnit]": 101.46679207657003},
        ]
    }
    expected = {
        "GB": {"Total Revenue": 25227674.699999996, "Revenue per Unit": 101.72941714921689},
        "SG": {"Total Revenue": 24819216.2, "Revenue per Unit": 100.46638681994818},
        "US": {"Total Revenue": 25135455.200000003, "Revenue per Unit": 101.46679207657003},
    }
    ok, lines = compare(expected, json_rows(payload), KEY, FIELDS)
    assert ok, lines


def test_json_rows_null_becomes_empty():
    rows = json_rows({"rows": [{"Customer[Country]": "US", "[Revenue]": None}]})
    assert rows[0]["[Revenue]"] == ""
