"""`runMultiple` is the orchestration primitive; `run` is one blocking call.

A notebook that orchestrates notebooks is its own Fabric pattern — code using it
cannot be rewritten as a pipeline without changing what it is — so the DAG
semantics are asserted here rather than left to the one e2e that happens to
exercise a happy path.

The notebook runs themselves are stubbed at `_run_detail`: what is under test is
the ORDER, the dependency handling, the failure propagation and the result
shape, none of which needs a Spark session to be wrong.

The contract asserted here is Microsoft's, from
learn.microsoft.com/fabric/data-engineering/notebookutils/notebookutils-notebook-run:
`run` returns the child's exit value, `runMultiple` returns
`{name: {"exitVal", "exception"}}`, and a DAG with any failure RAISES
`RunMultipleFailedException` carrying those results on `.result`.
"""
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from notebookutils import notebook  # noqa: E402
from notebookutils.common.exceptions import RunMultipleFailedException  # noqa: E402


@pytest.fixture
def ran(monkeypatch):
    """Record every notebook `runMultiple` invokes, in order.

    Stubs `_run_detail`, not `run`: `run` is now a projection of it, so stubbing
    the projection would leave the exit value untestable — the very thing that
    was wrong.
    """
    calls = []

    def fake_detail(path, timeout_seconds=None, per_cell_seconds=None,
                    arguments=None, workspace=None):
        calls.append({"name": path, "timeout": timeout_seconds,
                      "per_cell": per_cell_seconds, "args": arguments,
                      "workspace": workspace})
        if path.startswith("boom"):
            raise notebook.NotebookError(f"notebook {path!r} failed: on purpose")
        return (f"exit-of-{path}", "Completed", 3)

    monkeypatch.setattr(notebook, "_run_detail", fake_detail)
    return calls


def run_ok(dag, **kw):
    """runMultiple, asserting it did NOT raise — for all-succeeding DAGs."""
    return notebook.runMultiple(dag, **kw)


def run_failing(dag, **kw):
    """runMultiple for a DAG expected to fail; returns the partial results."""
    with pytest.raises(RunMultipleFailedException) as ei:
        notebook.runMultiple(dag, **kw)
    return ei.value.result


# --- shapes in ---------------------------------------------------------------

def test_a_bare_list_runs_every_notebook(ran):
    out = run_ok(["nb1", "nb2"])
    assert [c["name"] for c in ran] == ["nb1", "nb2"]
    assert out["nb1"]["exitVal"] == "exit-of-nb1"
    assert out["nb2"]["exception"] is None


def test_dependencies_decide_the_order_not_the_listing(ran):
    # `b` is listed first but depends on `a`, so `a` must run first. Listing
    # order deciding execution order is the bug this shape exists to prevent.
    run_ok({"activities": [
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
        {"name": "a", "path": "nbA"},
    ]})
    assert [c["name"] for c in ran] == ["nbA", "nbB"]


def test_order_within_a_level_is_the_order_given(ran):
    # Reproducibility over parallelism: a harness comparing two runs needs the
    # same sequence. Fabric defaults concurrency to 3x CPU cores; running
    # sequentially is a documented divergence, asserted here so it stays one.
    run_ok({"activities": [
        {"name": "x", "path": "nbX"},
        {"name": "y", "path": "nbY"},
        {"name": "z", "path": "nbZ", "dependencies": ["x", "y"]},
    ]})
    assert [c["name"] for c in ran] == ["nbX", "nbY", "nbZ"]


def test_path_defaults_to_the_activity_name(ran):
    run_ok({"activities": [{"name": "solo"}]})
    assert ran[0]["name"] == "solo"


def test_args_reach_the_child_run(ran):
    run_ok({"activities": [
        {"name": "a", "path": "nbA", "args": {"p1": "v"}},
    ]})
    assert ran[0]["args"] == {"p1": "v"}


def test_the_workspace_field_reaches_the_child_run(ran):
    # Fabric's field is `workspace` and takes a NAME or an id.
    run_ok({"activities": [{"name": "a", "path": "nbA", "workspace": "Analytics"}]})
    assert ran[0]["workspace"] == "Analytics"


def test_workspace_id_is_accepted_as_an_alias(ran):
    run_ok({"activities": [{"name": "a", "path": "nbA", "workspaceId": "ws-guid"}]})
    assert ran[0]["workspace"] == "ws-guid"


# --- the per-cell timeout ----------------------------------------------------

def test_the_timeout_is_passed_as_a_per_cell_budget(ran):
    # THE UNIT BUG. `timeoutPerCellInSeconds` is per CELL; passing it as a
    # whole-notebook deadline gave a 10-cell notebook 90s here and 900s on
    # Fabric, so a legitimately slow DAG failed locally and passed in
    # production. `_run_detail` multiplies by the real cell count; this asserts
    # the value arrives as the per-cell budget rather than a total.
    run_ok({"activities": [
        {"name": "a", "path": "nbA", "timeoutPerCellInSeconds": 7},
    ]})
    assert ran[0]["per_cell"] == 7
    assert ran[0]["timeout"] is None


def test_the_per_cell_timeout_defaults_to_ninety(ran):
    run_ok({"activities": [{"name": "a", "path": "nbA"}]})
    assert ran[0]["per_cell"] == 90


# --- failure: raises, and carries the partial results ------------------------

def test_a_failure_raises_and_the_results_ride_on_the_exception(ran):
    # Fabric's documented pattern is try/except RunMultipleFailedException with
    # `ex.result`. Returning quietly means that except branch never runs
    # locally and a caller never learns an activity failed.
    out = run_failing(["boom1", "nb2"])
    assert isinstance(out["boom1"]["exception"], notebook.NotebookError)
    assert "on purpose" in str(out["boom1"]["exception"])
    # The DAG did not stop: the independent activity still ran and succeeded.
    assert out["nb2"]["exception"] is None
    assert out["nb2"]["exitVal"] == "exit-of-nb2"


def test_the_exception_message_names_which_activities_failed(ran):
    with pytest.raises(RunMultipleFailedException, match="boom1"):
        notebook.runMultiple(["boom1", "nb2"])


def test_a_successful_dag_does_not_raise(ran):
    assert run_ok(["nb1"])["nb1"]["exception"] is None


def test_dependents_of_a_failure_are_skipped_not_run(ran):
    out = run_failing({"activities": [
        {"name": "a", "path": "boom-a"},
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
    ]})
    assert out["a"]["status"] == "Failed"
    # Skipped, and it says why. "did not run because a failed" and "ran and
    # failed" are different facts a caller acts on differently.
    assert out["b"]["status"] == "Skipped"
    assert "'a'" in str(out["b"]["exception"])
    assert [c["name"] for c in ran] == ["boom-a"]


def test_a_skip_cascades_down_the_chain(ran):
    out = run_failing({"activities": [
        {"name": "a", "path": "boom-a"},
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
        {"name": "c", "path": "nbC", "dependencies": ["b"]},
    ]})
    assert out["c"]["status"] == "Skipped"
    assert [c["name"] for c in ran] == ["boom-a"]


def test_an_unrelated_branch_still_runs_after_a_failure(ran):
    out = run_failing({"activities": [
        {"name": "a", "path": "boom-a"},
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
        {"name": "c", "path": "nbC"},
    ]})
    assert out["c"]["exception"] is None
    assert "nbC" in [c["name"] for c in ran]


def test_every_activity_gets_a_result_even_when_skipped(ran):
    # A short dict is indistinguishable from a DAG that never held those
    # activities, so absence is never how a skip is reported.
    out = run_failing({"activities": [
        {"name": "a", "path": "boom-a"},
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
        {"name": "c", "path": "nbC", "dependencies": ["b"]},
    ]})
    assert set(out) == {"a", "b", "c"}
    assert all("exitVal" in r and "exception" in r for r in out.values())


# --- the DAG-level wall clock ------------------------------------------------

def test_the_dag_timeout_stops_later_activities_with_a_reason(ran, monkeypatch):
    # An injected clock, not a real sleep: a test that waits out a wall clock
    # is a test nobody runs.
    # Call 1 sets the deadline, call 2 admits `a`, everything after is past it.
    seen = {"n": 0}

    def clock():
        seen["n"] += 1
        return 0 if seen["n"] <= 2 else 10_000

    monkeypatch.setattr(notebook.time, "time", clock)
    out = run_failing({"activities": [
        {"name": "a", "path": "nbA"},
        {"name": "b", "path": "nbB"},
    ], "timeoutInSeconds": 60})
    assert out["a"]["exception"] is None
    assert out["b"]["status"] == "Skipped"
    assert "timeoutInSeconds" in str(out["b"]["exception"])
    assert [c["name"] for c in ran] == ["nbA"]


# --- refusals ----------------------------------------------------------------

def test_a_cycle_is_refused_rather_than_hanging(ran):
    # Hanging and silently dropping activities are the two failure modes a
    # caller cannot debug from outside the call.
    with pytest.raises(notebook.NotebookError, match="cycle"):
        notebook.runMultiple({"activities": [
            {"name": "a", "dependencies": ["b"]},
            {"name": "b", "dependencies": ["a"]},
        ]})
    assert ran == []


def test_a_dependency_that_is_not_in_the_dag_is_refused(ran):
    with pytest.raises(notebook.NotebookError, match="not in the DAG"):
        notebook.runMultiple({"activities": [{"name": "a", "dependencies": ["ghost"]}]})
    assert ran == []


def test_a_duplicate_activity_name_is_refused(ran):
    # Names key the results and are what dependencies point at, so a repeat
    # dropped one result and made any dependency on it ambiguous.
    with pytest.raises(notebook.NotebookError, match="duplicate activity name"):
        notebook.runMultiple({"activities": [
            {"name": "a", "path": "nbA"},
            {"name": "a", "path": "nbB"},
        ]})
    assert ran == []


def test_an_activity_without_a_name_is_refused(ran):
    with pytest.raises(notebook.NotebookError, match="'name'"):
        notebook.runMultiple({"activities": [{"path": "nbA"}]})


def test_a_dag_without_activities_is_refused(ran):
    with pytest.raises(notebook.NotebookError, match="activities"):
        notebook.runMultiple({"concurrency": 5})


def test_a_shape_that_is_neither_list_nor_dag_is_refused(ran):
    with pytest.raises(notebook.NotebookError, match="list of notebook names"):
        notebook.runMultiple("just-a-string")


# --- validateDAG -------------------------------------------------------------

def test_validate_dag_accepts_a_well_formed_dag(ran):
    assert notebook.validateDAG({"activities": [
        {"name": "a", "path": "nbA"},
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
    ]}) is True
    assert ran == [], "validateDAG must not run anything"


@pytest.mark.parametrize("dag,match", [
    ({"activities": [{"name": "a", "dependencies": ["b"]},
                     {"name": "b", "dependencies": ["a"]}]}, "cycle"),
    ({"activities": [{"name": "a", "dependencies": ["ghost"]}]}, "not in the DAG"),
    ({"activities": [{"name": "a"}, {"name": "a"}]}, "duplicate"),
    ({"activities": [{"path": "nbA"}]}, "'name'"),
])
def test_validate_dag_refuses_the_same_shapes_run_multiple_does(dag, match, ran):
    with pytest.raises(notebook.NotebookError, match=match):
        notebook.validateDAG(dag)
