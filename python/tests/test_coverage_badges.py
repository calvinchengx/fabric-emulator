"""Badges must not flatter, and must not invent a number they were not given.

`scripts/coverage_badges.py` publishes what the README claims about this repo.
Two failure modes matter more than the rest, because both make the project look
better than it is and neither is visible from the badge itself:

  * a missing measurement rendered as a number (a leg with no SQL Server has no
    comparable Go figure, and must say so rather than publish the lower one);
  * a threshold that paints a mediocre percentage green while the repo enforces
    a 90% floor.

The witness ratio is counted rather than hardcoded, so these also pin that
`_gated` — which records why a witness may skip — is not mistaken for a claim.
"""
import json
import sys
from pathlib import Path

import importlib.util
import pytest

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "coverage_badges", REPO / "scripts" / "coverage_badges.py")
cb = importlib.util.module_from_spec(spec)
sys.modules["coverage_badges"] = cb
spec.loader.exec_module(cb)


# --- a missing number must never render as a number -----------------------

def test_absent_coverage_is_not_a_number():
    doc = cb.pct_badge("go coverage", None)
    assert doc["message"] == "n/a"
    assert doc["color"] == "lightgrey"


def test_zero_is_a_number_not_an_absence():
    """0% is a measurement and must not be confused with 'not measured'."""
    doc = cb.pct_badge("go coverage", 0.0)
    assert doc["message"] == "0.0%"
    assert doc["color"] == "red"


# --- thresholds ------------------------------------------------------------

@pytest.mark.parametrize("pct,expected", [
    (91.2, "brightgreen"), (90.0, "brightgreen"),
    (89.9, "green"), (80.0, "green"),
    (72.6, "yellowgreen"), (70.0, "yellowgreen"),
    (69.9, "yellow"), (60.0, "yellow"),
    (59.9, "orange"), (40.0, "orange"),
    (39.9, "red"), (0.0, "red"),
])
def test_colour_scale(pct, expected):
    assert cb.colour(pct) == expected


def test_the_repo_floor_is_not_painted_green():
    """The Go floor is 90 and the Python floor is 70. A percentage below its
    own floor must not read as healthy."""
    assert cb.colour(75.0) not in ("brightgreen", "green")


# --- shields contract ------------------------------------------------------

def test_documents_carry_the_shields_schema_version():
    for doc in (cb.pct_badge("x", 50.0), cb.witness_badge(), cb.e2e_badge()):
        assert doc["schemaVersion"] == 1
        assert set(doc) == {"schemaVersion", "label", "message", "color"}


# --- witnesses -------------------------------------------------------------

def write_witnesses(tmp_path, payload):
    p = tmp_path / "witnesses.json"
    p.write_text(json.dumps(payload), encoding="utf-8")
    return p


def test_gated_is_not_counted_as_a_claim(tmp_path):
    """`_gated` records why a witness may skip. Counting it would inflate both
    halves of the ratio and hide a genuinely unwitnessed claim."""
    p = write_witnesses(tmp_path, {
        "_gated": {"go:TestX": "needs SQL Server"},
        "a": {"witnesses": ["ci:x"]},
        "b": {"witnesses": ["ci:y"]},
    })
    assert cb.witness_counts(p) == (2, 2)


def test_a_claim_with_no_witness_is_counted_but_not_witnessed(tmp_path):
    p = write_witnesses(tmp_path, {
        "a": {"witnesses": ["ci:x"]},
        "b": {"witnesses": []},
        "c": {},
    })
    assert cb.witness_counts(p) == (1, 3)


def test_incomplete_witnessing_is_not_green(tmp_path):
    """Anything short of all-of-them is the interesting case."""
    p = write_witnesses(tmp_path, {"a": {"witnesses": ["ci:x"]}, "b": {}})
    doc = cb.witness_badge(p)
    assert doc["message"] == "1/2"
    assert doc["color"] == "orange"


def test_complete_witnessing_is_green(tmp_path):
    p = write_witnesses(tmp_path, {"a": {"witnesses": ["ci:x"]}})
    assert cb.witness_badge(p)["color"] == "brightgreen"


def test_no_claims_at_all_is_not_green(tmp_path):
    """An empty map would otherwise divide to a vacuous 0/0 success."""
    p = write_witnesses(tmp_path, {"_gated": {}})
    assert cb.witness_badge(p)["color"] == "orange"


def test_the_real_witness_map_is_complete():
    """The committed map has a witness for every claim — the same invariant
    check_witnesses.py --strict enforces, stated as the number we publish."""
    witnessed, total = cb.witness_counts()
    assert total > 0
    assert witnessed == total


# --- e2e suite count -------------------------------------------------------

def test_e2e_count_excludes_the_non_suite_jobs(tmp_path):
    ci = tmp_path / "ci.yml"
    ci.write_text(
        "jobs:\n"
        "  test:\n    steps: []\n"
        "  witnesses:\n    steps: []\n"
        "  example-parity:\n    steps: []\n"
        "  engine-matrix:\n    steps: []\n"
        "  portal:\n    steps: []\n"
        "  medallion:\n    steps: []\n"
        "  warehouse-tds:\n    steps: []\n",
        encoding="utf-8")
    assert cb.e2e_suite_count(ci) == 2


def test_e2e_count_is_read_from_the_workflow_not_hardcoded():
    """A hand-maintained list is the thing that goes stale and overstates the
    fleet; this must track the workflow."""
    assert cb.e2e_suite_count() > 10


# --- end to end ------------------------------------------------------------

def test_main_writes_all_four_documents(tmp_path, monkeypatch):
    out = tmp_path / "badges"
    monkeypatch.setattr(sys, "argv",
                        ["coverage_badges.py", "--out", str(out),
                         "--go", "91.2", "--python", "72.6"])
    assert cb.main() == 0
    names = {p.name for p in out.iterdir()}
    assert names == {"coverage-go.json", "coverage-python.json",
                     "witnesses.json", "e2e-suites.json"}
    doc = json.loads((out / "coverage-go.json").read_text())
    assert doc["message"] == "91.2%"


def test_main_without_numbers_publishes_na_not_zero(tmp_path, monkeypatch):
    out = tmp_path / "badges"
    monkeypatch.setattr(sys, "argv", ["coverage_badges.py", "--out", str(out)])
    assert cb.main() == 0
    for name in ("coverage-go.json", "coverage-python.json"):
        assert json.loads((out / name).read_text())["message"] == "n/a"
