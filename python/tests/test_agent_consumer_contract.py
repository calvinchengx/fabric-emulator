"""The SQL shapes downstream consumers emit, asserted against the shared agent.

`emulator-spark-agent` is built and published HERE and consumed THERE:
databricks-emulator pulls the same image by digest, and its Warehouse SQL path
reaches `delta_ops` through statements this repo's own witnesses never send.
Nothing in fabric-emulator's CI stood between a change to that module and a
break in another repo — the image is published on tag, and the consumer finds
out on upgrade.

It has already cost one: `_CREATE_DELTA_LOCATION` required `USING` to sit
directly against the table name, which is what dbt-fabricspark emits, so every
witness here was green. databricks-emulator emits

    CREATE TABLE events (id INT, name STRING) USING delta LOCATION '…'

which did not match, so the location went unrecorded, and the MERGE two
statements later fell through to `resolve()`'s DESCRIBE DETAIL and died in
Sail's parser. Four agent releases shipped with it.


**The contract is `cases/agent-consumer-contract.json`.** Every case is a
statement shape a named consumer actually sends, cited to the file it comes
from, with the answer the agent owes it and the break it guards. A consumer adds
a case when it starts emitting a new shape; a change here that breaks one fails
in the repo that would ship the regression, not in the repo that would suffer it.

This file is the recognition half: it runs every case against `delta_ops` with
no engine, in milliseconds, on every PR. Recognition is the half that silently
degrades — an unmatched shape raises nothing at the point of the miss. Whether a
statement *executes* needs an engine: the cases that list
`e2e/agent-contract/run.py` in `executed_by` are run for real by that gate,
against the built image, before it is published, and
`test_the_e2e_gate_executes_exactly_the_cases_that_name_it` holds the two to the
same list.
"""
import importlib.util
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "spark_agent"))

import casefiles  # noqa: E402  (on the path via conftest.py)
import delta_ops as d  # noqa: E402

REPO = pathlib.Path(__file__).resolve().parents[2]
SUITE = "agent-consumer-contract"
E2E = "e2e/agent-contract/run.py"


@pytest.fixture(autouse=True)
def _clean_registry():
    d.forget_all()
    yield
    d.forget_all()


@pytest.mark.cases(SUITE)
def test_the_agent_honours_the_shape_a_consumer_sends(case):
    consumer, source = case["consumer"], case["source"]
    sql = casefiles.text(case["sql"])
    for schema, location in case.get("given", {}).items():
        d.remember_schema(schema, location)
    expect = case["expect"]
    if "match" in expect:
        got = d.match(sql)
        assert got is not None, (
            f"{consumer} sends this and the agent no longer claims it "
            f"({source}). It would fall through to the engine.")
        assert got[0] == expect["match"], (
            f"{consumer}: routed to {got[0]!r}, contract says {expect['match']!r} "
            f"({source})")
    else:
        table, location = expect["remember"]["table"], expect["remember"]["location"]
        # The engine executes it; the agent's job is to notice the path.
        assert d.match(sql) is None or d.match(sql)[0] == "ctas", (
            f"{consumer}: this is the engine's statement to run ({source})")
        d.remember_stated_delta_location(sql)
        assert d.known_location(table) == location, (
            f"{consumer} creates {table!r} this way and the agent did not "
            f"record its location ({source}). Nothing fails here — it fails at "
            f"the next MERGE/OPTIMIZE of that name, in resolve(), as a parse "
            f"error from an engine that has no DESCRIBE DETAIL.")


def test_every_case_cites_the_consumer_file_it_came_from():
    # A case without provenance cannot be checked against the consumer when the
    # consumer changes, which is the only thing keeping the contract honest.
    for case in casefiles.load(SUITE):
        assert "-emulator" in case["consumer"], case["id"]
        assert len(case["source"]) > 20, f"{case['id']} has no source"


def test_every_case_expects_exactly_one_answer():
    # A case with both answers, or neither, is not asserting anything the test
    # above can check, and would pass through whichever branch it fell into.
    for case in casefiles.load(SUITE):
        assert list(case["expect"]) in (["match"], ["remember"]), case["id"]


def test_the_e2e_gate_executes_exactly_the_cases_that_name_it():
    # Both directions. A case that names the gate but is not in its list would
    # read as executed and never be; a statement the gate runs that no case
    # names is a shape nobody cited, retyped where it can drift from the one
    # this file recognises — the duplication the case file exists to remove.
    # The gate checks at run time that it used every id it declares.
    spec = importlib.util.spec_from_file_location("agent_contract_run", REPO / E2E)
    assert spec and spec.loader
    run = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(run)
    assert set(run.LIVE) == casefiles.executed_by(casefiles.load(SUITE), E2E)
