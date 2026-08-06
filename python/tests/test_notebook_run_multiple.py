"""`runMultiple` is the orchestration primitive; `run` is one blocking call.

A notebook that orchestrates notebooks is its own Fabric pattern — code using it
cannot be rewritten as a pipeline without changing what it is — so the DAG
semantics are asserted here rather than left to the one e2e that happens to
exercise a happy path.

The notebook runs themselves are stubbed: what is under test is the ORDER, the
dependency handling and the failure propagation, none of which needs a Spark
session to be wrong.
"""
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from notebookutils import notebook  # noqa: E402


@pytest.fixture
def ran(monkeypatch):
    """Record every notebook `runMultiple` invokes, in order."""
    calls = []

    def fake_run(name, timeoutSeconds=90, arguments=None, workspaceId=None, **kw):
        calls.append({"name": name, "timeout": timeoutSeconds, "args": arguments})
        if name.startswith("boom"):
            raise notebook.NotebookError(f"notebook {name!r} failed: on purpose")
        return "Completed"

    monkeypatch.setattr(notebook, "run", fake_run)
    return calls


def test_a_bare_list_runs_every_notebook(ran):
    out = notebook.runMultiple(["nb1", "nb2"])
    assert [c["name"] for c in ran] == ["nb1", "nb2"]
    assert out["nb1"]["status"] == "Completed"
    assert out["nb2"]["status"] == "Completed"


def test_dependencies_decide_the_order_not_the_listing(ran):
    # `b` is listed first but depends on `a`, so `a` must run first. Listing
    # order deciding execution order is the bug this shape exists to prevent.
    notebook.runMultiple({"activities": [
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
        {"name": "a", "path": "nbA"},
    ]})
    assert [c["name"] for c in ran] == ["nbA", "nbB"]


def test_order_within_a_level_is_the_order_given(ran):
    # Reproducibility over parallelism: a harness comparing two runs needs the
    # same sequence, and nothing here promises concurrency.
    notebook.runMultiple({"activities": [
        {"name": "x", "path": "nbX"},
        {"name": "y", "path": "nbY"},
        {"name": "z", "path": "nbZ", "dependencies": ["x", "y"]},
    ]})
    assert [c["name"] for c in ran] == ["nbX", "nbY", "nbZ"]


def test_path_defaults_to_the_activity_name(ran):
    notebook.runMultiple({"activities": [{"name": "solo"}]})
    assert ran[0]["name"] == "solo"


def test_args_and_timeout_reach_the_child_run(ran):
    notebook.runMultiple({"activities": [
        {"name": "a", "path": "nbA", "args": {"p1": "v"}, "timeoutPerCellInSeconds": 7},
    ]})
    assert ran[0]["args"] == {"p1": "v"}
    assert ran[0]["timeout"] == 7


def test_a_failure_is_reported_rather_than_raised(ran):
    # The caller wants every result, not the first exception: a DAG that stops
    # at the first failure hides what else would have worked.
    out = notebook.runMultiple(["boom1", "nb2"])
    assert out["boom1"]["status"] == "Failed"
    assert "on purpose" in out["boom1"]["error"]
    assert out["nb2"]["status"] == "Completed"


def test_dependents_of_a_failure_are_skipped_not_run(ran):
    out = notebook.runMultiple({"activities": [
        {"name": "a", "path": "boom-a"},
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
    ]})
    assert out["a"]["status"] == "Failed"
    # Skipped, and it says why. "did not run because a failed" and "ran and
    # failed" are different facts a caller acts on differently.
    assert out["b"]["status"] == "Skipped"
    assert "'a'" in out["b"]["error"]
    assert [c["name"] for c in ran] == ["boom-a"]


def test_a_skip_cascades_down_the_chain(ran):
    out = notebook.runMultiple({"activities": [
        {"name": "a", "path": "boom-a"},
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
        {"name": "c", "path": "nbC", "dependencies": ["b"]},
    ]})
    assert out["c"]["status"] == "Skipped"
    assert ran == [{"name": "boom-a", "timeout": 90, "args": None}]


def test_an_unrelated_branch_still_runs_after_a_failure(ran):
    out = notebook.runMultiple({"activities": [
        {"name": "a", "path": "boom-a"},
        {"name": "b", "path": "nbB", "dependencies": ["a"]},
        {"name": "c", "path": "nbC"},
    ]})
    assert out["c"]["status"] == "Completed"
    assert "nbC" in [c["name"] for c in ran]


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


def test_an_activity_without_a_name_is_refused(ran):
    with pytest.raises(notebook.NotebookError, match="'name'"):
        notebook.runMultiple({"activities": [{"path": "nbA"}]})


def test_a_dag_without_activities_is_refused(ran):
    with pytest.raises(notebook.NotebookError, match="activities"):
        notebook.runMultiple({"concurrency": 5})


def test_a_shape_that_is_neither_list_nor_dag_is_refused(ran):
    with pytest.raises(notebook.NotebookError, match="list of notebook names"):
        notebook.runMultiple("just-a-string")
