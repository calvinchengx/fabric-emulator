"""`notebookutils.notebook.run` returns the child's EXIT VALUE.

It returned the terminal job status ("Completed") until now, which is a
different answer to a different question. Microsoft's reference is explicit:

    The `run()` method returns the exact string passed to
    `notebookutils.notebook.exit(value)` in the child notebook. If `exit()`
    isn't called in the child notebook, an empty string ("") is returned.

so a parent doing `if run("load") == "ok":` took the wrong branch here and the
right one on Fabric — the failure direction that a local green run hides.

The exit value was never missing from the emulator: the engine posts it, the
service stores it, and `…/jobs/instances/{jid}/notebookRun` serves it. Nothing
asked for it. These tests drive the real request sequence with the HTTP layer
stubbed, so the URLs and the polling are under test too.
"""
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from notebookutils import notebook  # noqa: E402

WS = "ws-1"
ITEM = "item-1"
JOB = "job-1"


class FakeService:
    """The emulator's job surface, enough of it to run a notebook.

    `cells` is what the job's parsed run reports, which is what turns a
    per-cell timeout into a notebook deadline.
    """

    def __init__(self, exit_value="", status="Completed", cells=3,
                 failure_reason=None, detail_raises=False, poll_statuses=None):
        self.exit_value = exit_value
        self.status = status
        self.cells = cells
        self.failure_reason = failure_reason
        self.detail_raises = detail_raises
        self.poll_statuses = list(poll_statuses or [])
        self.calls = []

    def request(self, method, url, *, token=None, body=None, headers=None, raw=False):
        self.calls.append((method, url, body))
        if method == "GET" and url.endswith("/items?type=Notebook"):
            return {"value": [{"displayName": "nb", "id": ITEM},
                              {"displayName": "child", "id": ITEM}]}
        if method == "POST" and "jobs/instances?jobType=RunNotebook" in url:
            return (202, {"Location": f"https://x/v1/workspaces/{WS}/items/{ITEM}"
                                      f"/jobs/instances/{JOB}"}, b"")
        if method == "GET" and url.endswith("/notebookRun"):
            if self.detail_raises:
                raise RuntimeError("no run detail for this job")
            return {"status": self.status, "exitValue": self.exit_value,
                    "cells": [{"index": i} for i in range(self.cells)]}
        if method == "GET" and f"/jobs/instances/{JOB}" in url:
            status = self.poll_statuses.pop(0) if self.poll_statuses else self.status
            out = {"status": status}
            if self.failure_reason:
                out["failureReason"] = self.failure_reason
            return out
        raise AssertionError(f"unexpected request: {method} {url}")

    def urls(self, suffix):
        return [u for _m, u, _b in self.calls if u.endswith(suffix)]


@pytest.fixture
def service(monkeypatch):
    def install(is_real=False, **kw):
        svc = FakeService(**kw)
        monkeypatch.setattr(notebook, "request", svc.request)
        monkeypatch.setattr(notebook.credentials, "getToken", lambda _a: "tok")
        monkeypatch.setattr(notebook, "config",
                            lambda: type("C", (), {"fabric_url": "https://x",
                                                   "workspace_id": WS,
                                                   "lakehouse_id": "lake-1",
                                                   "is_real": is_real})())
        monkeypatch.setattr(notebook.time, "sleep", lambda _s: None)
        return svc
    return install


# --- the return value --------------------------------------------------------

def test_run_returns_the_childs_exit_value(service):
    service(exit_value="42")
    assert notebook.run("nb") == "42"


def test_run_returns_an_empty_string_when_the_child_never_exited(service):
    # Documented explicitly, and NOT None: code doing `int(run(...) or 0)` and
    # code doing `run(...) == ""` both depend on it.
    service(exit_value="")
    assert notebook.run("nb") == ""


def test_run_returns_a_json_payload_untouched(service):
    # Exit values are always strings; a caller that puts JSON in one must get
    # the same bytes back, not a parsed or re-serialised copy.
    service(exit_value='{"rows": 4, "table": "events"}')
    assert notebook.run("nb") == '{"rows": 4, "table": "events"}'


def test_run_reads_the_exit_value_from_the_run_detail_endpoint(service):
    svc = service(exit_value="v")
    notebook.run("nb")
    assert svc.urls("/notebookRun"), "the exit value lives on …/notebookRun"


# --- failure and timeout -----------------------------------------------------

def test_a_failed_notebook_raises_with_its_reason(service):
    service(status="Failed", failure_reason="cell 2 blew up")
    with pytest.raises(notebook.NotebookError, match="cell 2 blew up"):
        notebook.run("nb")


def test_a_notebook_that_never_finishes_raises_on_the_deadline(service, monkeypatch):
    service(poll_statuses=["Running"] * 50)
    ticks = {"n": 0}

    def clock():
        ticks["n"] += 1
        return 0 if ticks["n"] <= 2 else 10_000

    monkeypatch.setattr(notebook.time, "time", clock)
    with pytest.raises(notebook.NotebookError, match="did not finish within"):
        notebook.run("nb", 90)


def test_a_notebook_polls_until_it_reaches_a_terminal_state(service):
    svc = service(poll_statuses=["Running", "Running", "Completed"], exit_value="done")
    assert notebook.run("nb") == "done"
    assert len([u for _m, u, _b in svc.calls if u.endswith(JOB)]) == 3


def test_an_unreadable_run_detail_does_not_fail_a_completed_run(service):
    # A run whose detail cannot be read still completed; turning that into an
    # error would fail runs that worked.
    service(detail_raises=True)
    assert notebook.run("nb") is None


def test_the_real_target_says_WHY_the_exit_value_is_missing(service, recwarn):
    """`None` is honest but silent, and silence is the harm.

    A caller who reads the value gets a falsy answer with no account of itself,
    so they debug their notebook — which is working — instead of learning that a
    REST-submitted run cannot carry an exit value at all. The message has to
    name a pattern that DOES work, because "not supported" with no alternative
    is a dead end.

    A warning and not a raise: plenty of orchestration submits notebooks and
    never reads the return value, and failing those runs to report a limitation
    they are not hitting would be worse than the problem.
    """
    service(is_real=True, detail_raises=True)
    assert notebook.run("nb") is None

    msgs = [str(w.message) for w in recwarn if issubclass(w.category, RuntimeWarning)]
    assert msgs, "the real target reported no exit value and said nothing about it"
    text = msgs[0]
    assert "REST" in text, text
    assert "Delta table" in text, "the message names no working alternative"
    assert "inside a Fabric notebook" in text.lower() or "INSIDE a Fabric notebook" in text, text
    assert "retry" in text, "a reader will try a retry unless told it cannot help"


def test_the_emulator_does_not_explain_away_a_transient(service, recwarn):
    """Against the emulator the endpoint EXISTS, so failing to read it is a
    transient worth surfacing on its own terms — not a target limitation. A
    warning here would train people to ignore a real symptom."""
    service(is_real=False, detail_raises=True)
    assert notebook.run("nb") is None
    assert not [w for w in recwarn if issubclass(w.category, RuntimeWarning)], \
        "the emulator blamed a target limitation for a transient read failure"


def test_an_unavailable_exit_value_is_none_not_empty_string(service):
    """`None` (could not be obtained) and `""` (exited with no value) are
    DIFFERENT FACTS, and this used to return `""` for both.

    Real Fabric has no run-detail endpoint — measured 2026-08-11, it answers
    `404 EntityNotFound` — so on a tenant EVERY run took the second branch and
    looked like a notebook that had deliberately exited empty. A caller reading
    `""` concludes the notebook ran and returned nothing; the truth was that
    the value was unobtainable. `or ""` / `or 0` fallbacks behave identically
    either way, so nothing that ignored the distinction breaks.
    """
    service(detail_raises=True)
    assert notebook.run("nb") is None

    service(exit_value="", cells=1)
    assert notebook.run("nb") == ""


# --- arguments and aliases ---------------------------------------------------

def submitted(svc):
    """The executionData the child job was created with."""
    body = next(b for _m, u, b in svc.calls if "jobType=RunNotebook" in u)
    return (body or {}).get("executionData", {})


def test_arguments_are_sent_as_execution_data_parameters(service):
    svc = service()
    notebook.run("nb", 90, {"input": 20})
    assert submitted(svc)["parameters"] == {"input": {"value": 20, "type": "string"}}


def test_no_arguments_and_no_lakehouse_sends_no_body(service, monkeypatch):
    svc = service()
    monkeypatch.setattr(notebook, "config",
                        lambda: type("C", (), {"fabric_url": "https://x",
                                               "workspace_id": WS,
                                               "lakehouse_id": "",
                                               "is_real": False})())
    notebook.run("nb")
    assert next(b for _m, u, b in svc.calls if "jobType=RunNotebook" in u) is None


# --- the reference-run lakehouse rule ----------------------------------------

def test_the_parent_lakehouse_is_sent_so_the_rule_can_fire(service):
    # Without this the service cannot tell a reference run from a bare job
    # submission, so Fabric's mismatch check could never apply and a DAG with a
    # mis-bound child passed here while being blocked in production.
    svc = service()
    notebook.run("nb")
    assert submitted(svc)["parentLakehouseId"] == "lake-1"


def test_the_root_notebook_is_sent_so_builtin_resolves_against_it(service, monkeypatch):
    """`builtin/` means the ROOT notebook's folder, not the running one.

    Nothing sent a root before this, so `rootNotebookId` was never set anywhere
    in the tree and a referenced child resolved `nbResPath` to its OWN
    resources — a notebook reading different files depending on how it was
    started. Measured in e2e/conformance: removing this line makes the child
    read its own copy of the same filename.
    """
    monkeypatch.setattr(notebook.runtime, "context",
                        {"currentNotebookId": "nb-root", "currentWorkspaceId": WS})
    svc = service()
    notebook.run("nb")
    exec_data = submitted(svc)
    assert exec_data["rootNotebookId"] == "nb-root"
    assert exec_data["rootWorkspaceId"] == WS


def test_a_nested_reference_forwards_the_root_it_was_given(service, monkeypatch):
    """The subtlety, and the reason this is not just `currentNotebookId`.

    A parent that is ITSELF a child must pass on the root it received. Reading
    the current notebook unconditionally would make every generation its own
    root, and the grandchild would see the middle notebook's resources —
    breaking exactly the case the rule exists for.
    """
    monkeypatch.setattr(notebook.runtime, "context",
                        {"rootNotebookId": "nb-root", "rootWorkspaceId": WS,
                         "currentNotebookId": "nb-middle", "currentWorkspaceId": WS})
    svc = service()
    notebook.run("nb")
    assert submitted(svc)["rootNotebookId"] == "nb-root"


def test_use_root_default_lakehouse_travels_out_of_the_arguments(service):
    # Fabric puts the flag in `arguments`; it configures the RUN, so forwarding
    # it to the child as a parameter would set a variable the notebook never
    # declared.
    svc = service()
    notebook.run("nb", 90, {"input": 1, "useRootDefaultLakehouse": True})
    exec_data = submitted(svc)
    assert exec_data["useRootDefaultLakehouse"] is True
    assert exec_data["parameters"] == {"input": {"value": 1, "type": "string"}}
    assert "useRootDefaultLakehouse" not in exec_data["parameters"]


def test_the_bypass_flag_is_absent_unless_asked_for(service):
    svc = service()
    notebook.run("nb", 90, {"input": 1})
    assert "useRootDefaultLakehouse" not in submitted(svc)


def test_the_legacy_keyword_names_still_work(service):
    # This module took `timeoutSeconds=` and `workspaceId=` before Fabric's own
    # spelling was checked, and code in this repository passes them.
    service(exit_value="ok")
    assert notebook.run("nb", timeoutSeconds=30, workspaceId=WS) == "ok"


def test_an_unknown_notebook_is_refused_by_name(service):
    service()
    with pytest.raises(notebook.NotebookError, match="not found in workspace"):
        notebook.run("no-such-notebook")


# --- the per-cell timeout budget ---------------------------------------------

def never_finishes(monkeypatch):
    """A clock that starts at 0 and jumps far past any deadline.

    The timeout message names the deadline the code computed, so a run that
    never completes is how that number becomes observable.
    """
    ticks = {"n": 0}

    def clock():
        ticks["n"] += 1
        return 0 if ticks["n"] <= 2 else 10 ** 9

    monkeypatch.setattr(notebook.time, "time", clock)


def test_a_per_cell_budget_is_multiplied_by_the_real_cell_count(service, monkeypatch):
    # THE UNIT BUG, at the layer that fixes it. 4 cells x 30s/cell = 120s of
    # budget; the pre-fix code granted 30s total and failed a notebook Fabric
    # would have finished. The deadline is asserted through the message that
    # reports it, so this fails if the multiplication is dropped.
    service(cells=4, poll_statuses=["Running"] * 50)
    never_finishes(monkeypatch)
    with pytest.raises(notebook.NotebookError, match=r"did not finish within 120s"):
        notebook._run_detail("nb", per_cell_seconds=30)


def test_the_cell_count_floors_at_one_so_the_budget_is_never_zero(service, monkeypatch):
    # A zero-cell notebook must not be handed a zero-second deadline, which
    # would fail instantly on a run that has nothing to wait for.
    service(cells=0, poll_statuses=["Running"] * 50)
    never_finishes(monkeypatch)
    with pytest.raises(notebook.NotebookError, match=r"did not finish within 30s"):
        notebook._run_detail("nb", per_cell_seconds=30)


def test_an_unknowable_cell_count_uses_the_whole_notebook_ceiling(service, monkeypatch):
    """NOT `per_cell x 1`, which is what this asserted before.

    A cell count that cannot be read is unknown, not one. Returning 1 gave a
    twelve-cell notebook a one-cell deadline on real Fabric — where the detail
    endpoint does not exist, so EVERY run took that path — and produced a
    spurious timeout for a notebook that was working. The same notebook passed
    against the emulator with the full budget: emulator-green, tenant-broken.
    """
    service(detail_raises=True, poll_statuses=["Running"] * 50)
    never_finishes(monkeypatch)
    with pytest.raises(notebook.NotebookError,
                       match=rf"did not finish within {notebook._TIMEOUT_WITHOUT_CELL_COUNT}s"):
        notebook._run_detail("nb", per_cell_seconds=30)


def test_the_ceiling_is_longer_than_any_per_cell_budget_would_give(service):
    """The ceiling must not be shorter than the multiplied budget it replaces,
    or the fix reintroduces the bug for large notebooks."""
    assert notebook._TIMEOUT_WITHOUT_CELL_COUNT > notebook._DEFAULT_TIMEOUT_PER_CELL * 12


def test_run_detail_reports_the_exit_value_status_and_cell_count(service):
    service(exit_value="v", cells=5)
    assert notebook._run_detail("nb") == ("v", "Completed", 5)


def test_an_explicit_timeout_wins_over_the_per_cell_budget(service, monkeypatch):
    # `run` takes a whole-notebook timeout and `runMultiple` a per-cell one;
    # they are different units and must not be conflated.
    service(cells=4, poll_statuses=["Running"] * 50)
    never_finishes(monkeypatch)
    with pytest.raises(notebook.NotebookError, match=r"did not finish within 5s"):
        notebook._run_detail("nb", timeout_seconds=5, per_cell_seconds=30)
