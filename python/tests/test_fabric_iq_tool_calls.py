"""cases/fabric-iq-tool-calls.json is well-formed for both of its runners.

The Go test (internal/api/fabriciq_cases_test.go) runs every case and the e2e
driver (e2e/mcp-fabriciq/driver.py) the ones that name it. Neither can tell a
case it skipped for being malformed from one that passed, so the shape is held
here, where a bad row is refused by id before either runner reads it.
"""
import casefiles  # on the path via conftest.py
import pytest

SUITE = "fabric-iq-tool-calls"
TOOLS = {"DiscoverArtifacts", "ResolveFabricItem", "GetReportMetadata",
         "GetSemanticModelSchema", "ValueSearch", "ExecuteQuery"}
ROLES = {"owner", "viewer"}
IDS = {"workspace", "model", "report"}
KEYS = {"id", "why", "as", "tool", "args", "expect", "refused", "executed_by"}


def _placeholders(value):
    import re
    if isinstance(value, str):
        return set(re.findall(r"\{(\w+)\}", value))
    if isinstance(value, list):
        return set().union(*map(_placeholders, value)) if value else set()
    if isinstance(value, dict):
        return _placeholders(list(value.values()))
    return set()


@pytest.mark.cases(SUITE)
def test_each_case_is_one_call_with_one_kind_of_answer(case):
    assert set(case) <= KEYS, f"unknown keys {set(case) - KEYS}"
    assert case["as"] in ROLES
    assert case["tool"] in TOOLS
    assert isinstance(case["args"], dict)
    assert ("expect" in case) != ("refused" in case), "either expect or refused, not both"
    if "refused" in case:
        assert isinstance(case["refused"], str) and case["refused"].strip()
    else:
        assert case["expect"], "an empty expect asserts nothing"
        for e in case["expect"]:
            # Evaluated against nothing, a well-formed expectation fails on the
            # value; a malformed one fails on its own shape.
            why = casefiles.unmet({}, e)
            assert why is not None and "exactly one of" not in why and "knows" not in why, why
    unknown = _placeholders(case) - IDS
    assert not unknown, f"placeholders no runner fills: {unknown}"


def test_the_suite_has_a_case_for_every_tool():
    cases = casefiles.load(SUITE)
    assert {c["tool"] for c in cases} == TOOLS
