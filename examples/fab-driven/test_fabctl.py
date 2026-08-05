"""Prove the job gate can go RED.

This file exists because of one measured fact: `fab job run` exits **0** on a
job that was cancelled or failed. Every assertion in this example about whether
something ran therefore rests on `check_completed`, and an assertion nobody has
watched fail is not an assertion — it is a decoration that has never been
disturbed.

So each test below feeds `check_completed` an outcome that MUST be rejected, and
fails if it is accepted. Run them without any stack up:

    uv run pytest test_fabctl.py -q
"""
import json

import fabctl
import pytest

# --- the gate --------------------------------------------------------------

def test_a_completed_run_passes():
    runs = [{"id": "j1", "status": "Completed", "startTimeUtc": "2026-08-04T10:00:00Z"}]
    assert fabctl.check_completed(runs, "x")["id"] == "j1"


@pytest.mark.parametrize("status", ["Cancelled", "Failed", "InProgress", "NotStarted"])
def test_any_non_completed_status_is_rejected(status):
    """The one `fab job run` returns 0 for, and three more besides.

    `Cancelled` is the exact outcome measured: a notebook was allowed to time
    out, fab cancelled it, and the process exited 0.
    """
    runs = [{"id": "j1", "status": status, "startTimeUtc": "2026-08-04T10:00:00Z"}]
    with pytest.raises(AssertionError) as e:
        fabctl.check_completed(runs, "silver.Notebook")
    assert status in str(e.value)


def test_no_runs_at_all_is_a_failure_not_a_pass():
    """The trap inside the trap.

    If `fab job run` never started anything, `run-list` comes back empty. A gate
    that returned quietly on an empty list would be the most convincing green
    tick in the example and would mean nothing whatsoever.
    """
    with pytest.raises(AssertionError) as e:
        fabctl.check_completed([], "bronze-ingest.DataPipeline")
    assert "NO job runs" in str(e.value)


def test_a_stale_completed_run_cannot_mask_a_fresh_failure():
    """Re-running a step that fails must not pass on last time's success.

    The items here are addressed by name and re-run in place, so `run-list`
    accumulates history. Reading any Completed row rather than the NEWEST one
    would make every step permanently green after its first success.
    """
    runs = [
        {"id": "old", "status": "Completed", "startTimeUtc": "2026-08-04T10:00:00Z"},
        {"id": "new", "status": "Failed", "startTimeUtc": "2026-08-04T11:00:00Z"},
    ]
    with pytest.raises(AssertionError) as e:
        fabctl.check_completed(runs, "silver.Notebook")
    assert "new" in str(e.value)


def test_newest_run_does_not_depend_on_list_order():
    """`fab job run-list` promises no ordering, so neither may we."""
    newest = {"id": "new", "status": "Completed", "startTimeUtc": "2026-08-04T11:00:00Z"}
    older = {"id": "old", "status": "Failed", "startTimeUtc": "2026-08-04T10:00:00Z"}
    assert fabctl.newest_run([newest, older])["id"] == "new"
    assert fabctl.newest_run([older, newest])["id"] == "new"


# --- reading fab's output --------------------------------------------------

def test_ansi_is_stripped_because_a_colour_escape_breaks_json():
    coloured = "\x1b[38;5;243m{\"status\": \"Completed\"}\x1b[0m\r\n"
    plain = fabctl.strip_ansi(coloured)
    assert json.loads(plain.strip())["status"] == "Completed"


@pytest.mark.parametrize("payload", [
    [{"status": "Completed"}],
    {"data": [{"status": "Completed"}]},
    {"value": [{"status": "Completed"}]},
    {"result": [{"status": "Completed"}]},
    {"text": "[{\"status\": \"Completed\"}]"},
])
def test_every_envelope_fab_uses_unwraps_to_the_same_rows(payload):
    """fab wraps results differently per command; callers want one shape."""
    assert fabctl.as_rows(payload) == [{"status": "Completed"}]


def test_the_envelope_fab_job_run_list_actually_returns():
    """Not a hypothetical shape — this is copied from a real run.

    The other envelopes above are defensive. This one is the measured output of
    `fab job run-list --output_format json` against the emulator, and it is the
    case the first version of unwrap() did not handle: a completed pipeline run
    was reported as `status None`, because the rows never left the envelope.
    """
    measured = {"data": [{
        "endTimeUtc": "2026-08-04T00:02:08Z",
        "id": "078e793a-d7a1-48f2-ab84-710622680b43",
        "invokeType": "Manual",
        "itemId": "d71fde10-555c-4f96-bac2-36e876bbb6b0",
        "jobType": "Pipeline",
        "startTimeUtc": "2026-08-04T00:02:08Z",
        "status": "Completed",
        "workspaceId": "110f61a7-0a5d-47d7-874d-bae759987b39",
    }]}
    run = fabctl.check_completed(fabctl.as_rows(measured), "bronze-ingest.DataPipeline")
    assert run["id"] == "078e793a-d7a1-48f2-ab84-710622680b43"


def test_a_single_object_becomes_one_row():
    assert fabctl.as_rows({"id": "j1", "status": "Completed"}) == [
        {"id": "j1", "status": "Completed"}
    ]


def test_nothing_at_all_becomes_no_rows_rather_than_an_error():
    """An empty listing must reach check_completed, which rejects it — it must
    not blow up here, where the message would be about parsing rather than
    about the job that never ran."""
    assert fabctl.as_rows(None) == []
    assert fabctl.as_rows([]) == []


def test_unwrap_terminates_on_a_self_nesting_payload():
    """Bounded, so a surprising envelope cannot hang the example."""
    nested = {"result": {"result": {"result": {"result": {"result": {"id": "deep"}}}}}}
    assert fabctl.unwrap(nested) is not None


# --- `fab api`, which nests one level deeper ------------------------------

# Copied verbatim from `fab api workspaces/<id>/lineage --output_format json`
# against the emulator. A raw passthrough returns the HTTP response as well as
# the body, and the generic unwrap() stopped at the wrapper — so readback.py saw
# one row whose `producer` was missing and reported "producers seen: ['']" for a
# graph that did carry a proper Copy edge.
MEASURED_API = {
    "timestamp": "2026-08-04T00:10:41.265206Z",
    "status": "Success",
    "command": "api",
    "result": {"data": [{"status_code": 200, "text": {"value": [{
        "id": "225f2fb3-46bb-4448-aec2-5a468d02e62d",
        "activityName": "IngestCustomers",
        "sourcePath": "Files/landing/customers.csv",
        "targetPath": "Tables/bronze_customers",
        "producer": "Copy",
        "sourceKind": "item",
    }]}}]},
}


def test_fab_api_rows_reach_the_records_not_the_http_wrapper():
    rows = fabctl.api_rows_of(MEASURED_API)
    assert len(rows) == 1
    assert rows[0]["producer"] == "Copy"
    assert rows[0]["targetPath"] == "Tables/bronze_customers"


@pytest.mark.parametrize("status", [400, 401, 403, 404, 500])
def test_a_failed_passthrough_raises_rather_than_returning_no_rows(status):
    """`fab api` exits 0 on a 4xx, exactly as `fab job run` does on a failure.

    Returning [] here would make a 403 indistinguishable from an empty graph,
    and readback.py would report 'the workspace recorded no lineage at all' —
    a true sentence about the wrong thing.
    """
    payload = {"result": {"data": [{"status_code": status, "text": {"value": []}}]}}
    with pytest.raises(AssertionError) as e:
        fabctl.api_rows_of(payload, "workspaces/x/lineage")
    assert str(status) in str(e.value)


def test_an_api_result_that_is_already_plain_still_works():
    """Not every fab version has to nest the same way for this to hold."""
    assert fabctl.api_rows_of({"value": [{"producer": "Copy"}]}) == [{"producer": "Copy"}]
