#!/usr/bin/env python3
"""Microsoft's `az` CLI drives Data Factory pipeline ACTIVITIES, and every one
is judged by what it did to the data.

WHY THIS EXISTS. Four parity rows — Delete, GetMetadata, Lookup, Validation —
were witnessed only by this repo's own Go tests, which means the contract was
graded by the same reading that implemented it. They looked unwitnessable
because the activity interpreter is internal, and it is. But an *interpreter*
being internal does not make the *contract* internal: a pipeline is created,
defined and run entirely over public Fabric REST, `queryactivityruns` returns
each activity's output, and OneLake shows what actually moved.

WHAT AN ACTIVITY WITNESS HAS TO DO, and the trap it exists to avoid. Every
check here asserts the activity's OUTPUT and the STATE OF THE DATA. Reading
back `Completed` proves only that nothing threw — a Delete that deleted
nothing, a Validation that blessed an absent file, and a Lookup that returned
an empty row set all report Completed. That is the shape this repo keeps
finding, so each check below reads OneLake or the downstream value.

The login is imported from e2e/az-rest rather than copied: cert harvesting, the
ARM stub `az login` insists on, and private-cloud registration are identical
here, and two copies would drift.
"""
from __future__ import annotations

import base64
import json
import sys
import time

sys.path.insert(0, "/")
import azrest  # noqa: E402  — the az-rest driver, mounted as a module
from azrest import az_rest, az_rest_raw, fail, onelake_read, onelake_write, v1  # noqa: E402


def new_workspace(name: str) -> str:
    return az_rest("post", v1("workspaces"), {"displayName": name})["id"]


def new_lakehouse(ws: str, name: str) -> str:
    return az_rest("post", v1(f"workspaces/{ws}/items"),
                   {"displayName": name, "type": "Lakehouse"})["id"]


def run_pipeline(ws: str, name: str, content: dict) -> list[dict]:
    """Create → define → run → wait → return the activity runs.

    The item is created bare and defined with `updateDefinition`, because a
    create that carries a definition answers 202 with no body and the id would
    have to be polled for. Two calls with an id beat one without.
    """
    pid = az_rest("post", v1(f"workspaces/{ws}/items"),
                  {"displayName": name, "type": "DataPipeline"})["id"]
    az_rest("post", v1(f"workspaces/{ws}/items/{pid}/updateDefinition"),
            {"definition": {"parts": [{
                "path": "pipeline-content.json",
                "payload": base64.b64encode(json.dumps(content).encode()).decode(),
                "payloadType": "InlineBase64"}]}})
    az_rest_raw("post", v1(f"workspaces/{ws}/items/{pid}/jobs/instances?jobType=Pipeline"),
                body="{}", headers=["Content-Type=application/json"])

    instance = None
    for _ in range(40):
        runs = az_rest("get", v1(f"workspaces/{ws}/items/{pid}/jobs/instances")).get("value") or []
        for r in runs:
            if r.get("status") in ("Completed", "Failed", "Deduped"):
                instance = r
                break
        if instance:
            break
        time.sleep(0.3)
    if not instance:
        fail(f"{name}: no job instance reached a terminal state")
    if instance["status"] != "Completed":
        detail = az_rest("post", v1(f"workspaces/{ws}/items/{pid}/jobs/instances/"
                                    f"{instance['id']}/queryactivityruns"), {}, allow_error=True)
        fail(f"{name}: job {instance['status']}, activity runs = {detail}")

    for _ in range(20):
        detail = az_rest("post", v1(f"workspaces/{ws}/items/{pid}/jobs/instances/"
                                    f"{instance['id']}/queryactivityruns"), {}, allow_error=True)
        if isinstance(detail, dict) and "value" in detail:
            return detail["value"] or []
        time.sleep(0.3)
    fail(f"{name}: queryactivityruns never returned a value envelope")


def output_of(runs: list[dict], activity: str) -> dict:
    for r in runs:
        if r.get("activityName") == activity:
            return r.get("output") or {}
    fail(f"activity {activity!r} not in runs: {[r.get('activityName') for r in runs]}")


def check_delete(ws: str) -> None:
    print("-- Delete: the file is gone, and the count is the real one")
    lh = new_lakehouse(ws, "del-lake")
    # No leading or trailing whitespace in the payload: `onelake_read` strips,
    # so a body ending in a newline reads back shorter than it was written and
    # the sibling check below fails looking exactly like a Delete that took the
    # wrong path. It did that once.
    sibling = "keep-me"
    for p in ("Files/tmp/junk.csv", "Files/tmp/keep/deep.csv"):
        onelake_write(f"{ws}/{lh}/{p}", sibling)

    runs = run_pipeline(ws, "pl-delete", {"properties": {"activities": [
        {"name": "Del", "type": "Delete", "typeProperties": {
            "location": {"itemId": lh, "path": "Files/tmp/junk.csv"}}}]}})
    got = output_of(runs, "Del").get("filesDeleted")
    if got != 1:
        fail(f"filesDeleted = {got!r}, want 1")

    # The claim is that it DELETED, so the file must be gone — and the sibling
    # must not be, or a Delete that wiped the tree would pass this too.
    gone = az_rest_raw_missing(f"{ws}/{lh}/Files/tmp/junk.csv")
    if not gone:
        fail("the file survived a Delete that reported filesDeleted=1")
    if onelake_read(f"{ws}/{lh}/Files/tmp/keep/deep.csv") != sibling:
        fail("Delete took a path it was not pointed at")
    print("   filesDeleted=1, target gone, sibling intact")


def az_rest_raw_missing(path: str) -> bool:
    """True when the OneLake path is absent. A read of a missing file exits
    non-zero, which `onelake_read` turns into a hard failure — here the absence
    IS the assertion, so the failure has to be caught rather than propagated."""
    try:
        onelake_read(path)
    except SystemExit:
        return True
    return False


def check_getmetadata(ws: str) -> None:
    print("-- GetMetadata: the fields describe the real file")
    lh = new_lakehouse(ws, "meta-lake")
    onelake_write(f"{ws}/{lh}/Files/a.bin", "hello world")

    runs = run_pipeline(ws, "pl-meta", {"properties": {"activities": [
        {"name": "Meta", "type": "GetMetadata", "typeProperties": {
            "fieldList": ["exists", "size", "itemType", "itemName"],
            "dataset": {"location": {"itemId": lh, "path": "Files/a.bin"}}}}]}})
    m = output_of(runs, "Meta")
    # size is asserted against the bytes actually written, not a constant: a
    # GetMetadata returning a plausible fixed number would pass a bare shape check.
    if (m.get("exists") is not True or m.get("itemType") != "File"
            or m.get("itemName") != "a.bin" or m.get("size") != len("hello world")):
        fail(f"metadata does not describe the file: {m}")
    print(f"   exists/itemType/itemName correct, size={m['size']} matches the bytes")


def check_lookup(ws: str) -> None:
    print("-- Lookup: rows read from OneLake, and one flows downstream")
    lh = new_lakehouse(ws, "lookup-lake")
    onelake_write(f"{ws}/{lh}/Files/ref.csv", "id,name\n1,alice\n2,bob\n")

    runs = run_pipeline(ws, "pl-lookup", {"properties": {
        "variables": {"who": {"type": "String"}},
        "activities": [
            {"name": "Lk", "type": "Lookup", "typeProperties": {
                "source": {"location": {"itemId": lh, "path": "Files/ref.csv"}}}},
            {"name": "Use", "type": "SetVariable",
             "dependsOn": [{"activity": "Lk", "dependencyConditions": ["Succeeded"]}],
             "typeProperties": {"variableName": "who",
                                "value": "@activity('Lk').output.firstRow.name"}},
        ]}})
    lk = output_of(runs, "Lk")
    if lk.get("count") != 2:
        fail(f"lookup count = {lk.get('count')!r}, want the 2 rows in the CSV")
    if (lk.get("firstRow") or {}).get("name") != "alice":
        fail(f"firstRow = {lk.get('firstRow')!r}")
    # The value must have MOVED. A Lookup whose output nothing consumes could
    # be fabricated from the file name; a downstream expression cannot.
    if output_of(runs, "Use").get("value") != "alice":
        fail(f"the looked-up value did not reach the downstream activity: "
             f"{output_of(runs, 'Use')}")
    print("   count=2, firstRow=alice, and alice arrived in the SetVariable")


def check_validation(ws: str) -> None:
    print("-- Validation: passes on data that is there, FAILS on data that is not")
    lh = new_lakehouse(ws, "val-lake")
    body = "id,name\n1,ada\n"
    onelake_write(f"{ws}/{lh}/Files/in/day.csv", body)

    runs = run_pipeline(ws, "pl-validation", {"properties": {"activities": [
        {"name": "Wait", "type": "Validation", "typeProperties": {
            "dataset": {"itemId": lh, "path": "Files/in/day.csv"},
            "timeout": "00:01:00"}}]}})
    out = output_of(runs, "Wait")
    if (out.get("exists") is not True or out.get("itemName") != "day.csv"
            or out.get("size") != len(body)):
        fail(f"validation output does not describe the path: {out}")

    # The half that matters. A Validation that passes on an ABSENT file hands
    # the pipeline a guard's blessing to read something that is not there, so
    # the negative case is the assertion — without it this check would pass
    # against an activity hardcoded to succeed.
    pid = az_rest("post", v1(f"workspaces/{ws}/items"),
                  {"displayName": "pl-validation-absent", "type": "DataPipeline"})["id"]
    absent = {"properties": {"activities": [
        {"name": "Wait", "type": "Validation", "typeProperties": {
            "dataset": {"itemId": lh, "path": "Files/in/never-written.csv"},
            "timeout": "00:00:02"}}]}}
    az_rest("post", v1(f"workspaces/{ws}/items/{pid}/updateDefinition"),
            {"definition": {"parts": [{
                "path": "pipeline-content.json",
                "payload": base64.b64encode(json.dumps(absent).encode()).decode(),
                "payloadType": "InlineBase64"}]}})
    az_rest_raw("post", v1(f"workspaces/{ws}/items/{pid}/jobs/instances?jobType=Pipeline"),
                body="{}", headers=["Content-Type=application/json"])
    for _ in range(60):
        got = az_rest("get", v1(f"workspaces/{ws}/items/{pid}/jobs/instances")).get("value") or []
        states = [r.get("status") for r in got]
        if "Failed" in states:
            print("   present → exists/size correct; absent → the job Failed, as it must")
            return
        if "Completed" in states:
            fail("Validation PASSED on a file that was never written — the guard "
                 "would bless a downstream read of nothing")
        time.sleep(0.5)
    fail(f"the absent-data pipeline never reached a terminal state: {states}")


def main() -> int:
    try:
        azrest.login()
        ws = new_workspace("pipeline-activities")
        check_delete(ws)
        check_getmetadata(ws)
        check_lookup(ws)
        check_validation(ws)
        print("\nPIPELINE ACTIVITIES E2E: PASS — az drove Delete, GetMetadata, "
              "Lookup and Validation, each judged by the data")
        return 0
    except SystemExit:
        raise
    finally:
        azrest.az("cloud", "set", "--name", "AzureCloud", check=False)
        azrest.az("cloud", "unregister", "--name", azrest.CLOUD, check=False)
        if azrest._ARM_STUB is not None:
            azrest._ARM_STUB.shutdown()


if __name__ == "__main__":
    sys.exit(main())
