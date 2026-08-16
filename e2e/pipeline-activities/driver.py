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
import ssl
import sys
import time
import urllib.request

FABRIC_BASE = "https://api.fabric.microsoft.com"

sys.path.insert(0, "/")
import azrest  # noqa: E402  — the az-rest driver, mounted as a module
from azrest import (  # noqa: E402
    az_rest,
    az_rest_raw,
    fail,
    onelake_read,
    onelake_write,
    v1,
)  # noqa: E402


def new_workspace(name: str) -> str:
    return az_rest("post", v1("workspaces"), {"displayName": name})["id"]


def new_lakehouse(ws: str, name: str) -> str:
    return az_rest("post", v1(f"workspaces/{ws}/items"),
                   {"displayName": name, "type": "Lakehouse"})["id"]


def define(ws: str, pid: str, content: dict) -> None:
    """Attach a pipeline definition to an existing item."""
    az_rest("post", v1(f"workspaces/{ws}/items/{pid}/updateDefinition"),
            {"definition": {"parts": [{
                "path": "pipeline-content.json",
                "payload": base64.b64encode(json.dumps(content).encode()).decode(),
                "payloadType": "InlineBase64"}]}})


def run_pipeline(ws: str, name: str, content: dict) -> list[dict]:
    """Create → define → run → wait → return the activity runs.

    The item is created bare and defined with `updateDefinition`, because a
    create that carries a definition answers 202 with no body and the id would
    have to be polled for. Two calls with an id beat one without.
    """
    pid = az_rest("post", v1(f"workspaces/{ws}/items"),
                  {"displayName": name, "type": "DataPipeline"})["id"]
    define(ws, pid, content)
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
    # `raise` rather than `fail(...)` at a tail position: `fail` is NoReturn,
    # but it is imported from a module mounted at runtime, so the checker sees
    # Unknown and reads this function as able to fall off the end returning None.
    raise SystemExit(f"FAIL: {name}: queryactivityruns never returned a value envelope")


def output_of(runs: list[dict], activity: str) -> dict:
    for r in runs:
        if r.get("activityName") == activity:
            return r.get("output") or {}
    # Same reason as run_pipeline above: an imported NoReturn is Unknown here.
    raise SystemExit(f"FAIL: activity {activity!r} not in runs: "
                     f"{[r.get('activityName') for r in runs]}")


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
    define(ws, pid, absent)
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


def names_of(runs: list[dict]) -> list[str | None]:
    """`str | None`, not `str`: an activity record without a name is a real
    shape the checker is right to insist on, and silently dropping it would
    make a run look shorter than it was."""
    return [r.get("activityName") for r in runs]


def check_execute_pipeline(ws: str) -> None:
    print("-- ExecutePipeline: the CHILD's effect is what proves it ran")
    src, dst = new_lakehouse(ws, "ep-src"), new_lakehouse(ws, "ep-dst")
    onelake_write(f"{ws}/{src}/Files/in", "carried")

    # The child is referenced BY NAME, so the parent has to resolve it. Its
    # effect is a byte move, not a status: a parent that reported Completed
    # having invoked nothing would pass any check that read only the parent.
    child_id = az_rest("post", v1(f"workspaces/{ws}/items"),
                       {"displayName": "worker", "type": "DataPipeline"})["id"]
    define(ws, child_id, {"properties": {"activities": [
        {"name": "Move", "type": "Copy", "typeProperties": {
            "source": {"location": {"itemId": src, "path": "Files/in"}},
            "sink": {"location": {"itemId": dst, "path": "Files/out"}}}}]}})

    run_pipeline(ws, "parent", {"properties": {"activities": [
        {"name": "Call", "type": "ExecutePipeline",
         "typeProperties": {"pipeline": {"referenceName": "worker"}}}]}})
    if onelake_read(f"{ws}/{dst}/Files/out") != "carried":
        fail("the child pipeline's Copy never landed — ExecutePipeline reported "
             "success without invoking it")
    print("   parent → worker by name → bytes at the child's sink")


def check_foreach(ws: str) -> None:
    print("-- ForEach: every item runs, sequential and parallel alike")
    for seq, batch in ((True, None), (False, 3)):
        tp = {"items": "@createArray('a','b','c')", "isSequential": seq,
              "activities": [{"name": "Body", "type": "SetVariable", "typeProperties": {
                  "variableName": "seen", "value": "@toUpper(item())"}}]}
        if batch:
            tp["batchCount"] = batch
        runs = run_pipeline(ws, f"pl-foreach-{'seq' if seq else 'par'}", {"properties": {
            "variables": {"seen": {"type": "String"}}, "activities": [
                {"name": "loop", "type": "ForEach", "typeProperties": tp}]}})
        bodies = [n for n in names_of(runs) if n == "Body"]
        # THREE iterations, not one: a ForEach that ran its body once and
        # reported Completed is the failure this counts against.
        if len(bodies) != 3:
            fail(f"isSequential={seq}: body ran {len(bodies)}x, want 3 — runs={names_of(runs)}")
    print("   3 iterations both sequential and with batchCount=3")


def check_control_flow(ws: str) -> None:
    print("-- Control flow: only the taken branch runs, and Until terminates")
    runs = run_pipeline(ws, "pl-if-switch", {"properties": {
        "parameters": {"n": {"type": "Integer", "defaultValue": 10},
                       "mode": {"type": "String", "defaultValue": "b"}},
        "variables": {"i": {"type": "Integer", "defaultValue": 0}},
        "activities": [
            {"name": "branch", "type": "IfCondition", "typeProperties": {
                "expression": {"value": "@greater(pipeline().parameters.n, 5)",
                               "type": "Expression"},
                "ifTrueActivities": [{"name": "big", "type": "SetVariable",
                                      "typeProperties": {"variableName": "i", "value": "1"}}],
                "ifFalseActivities": [{"name": "small", "type": "SetVariable",
                                       "typeProperties": {"variableName": "i", "value": "2"}}]}},
            {"name": "sw", "type": "Switch", "typeProperties": {
                "on": {"value": "@pipeline().parameters.mode", "type": "Expression"},
                "cases": [
                    {"value": "a", "activities": [{"name": "ca", "type": "SetVariable",
                     "typeProperties": {"variableName": "i", "value": "3"}}]},
                    {"value": "b", "activities": [{"name": "cb", "type": "SetVariable",
                     "typeProperties": {"variableName": "i", "value": "4"}}]}],
                "defaultActivities": [{"name": "cd", "type": "SetVariable",
                                       "typeProperties": {"variableName": "i", "value": "5"}}]}},
        ]}})
    seen = names_of(runs)
    # The NEGATIVE half carries the claim: a runner that executed every branch
    # would satisfy "big ran" and "cb ran" while being completely wrong.
    for taken, skipped in (("big", "small"), ("cb", "ca"), ("cb", "cd")):
        if taken not in seen:
            fail(f"{taken} did not run: {seen}")
        if skipped in seen:
            fail(f"{skipped} ran too — the untaken branch was executed: {seen}")

    runs = run_pipeline(ws, "pl-until", {"properties": {
        "variables": {"i": {"type": "Integer", "defaultValue": 0}},
        "activities": [{"name": "until", "type": "Until", "typeProperties": {
            "expression": {"value": "@greaterOrEquals(variables('i'), 3)", "type": "Expression"},
            "activities": [{"name": "inc", "type": "SetVariable", "typeProperties": {
                "variableName": "i", "value": "@add(variables('i'),1)"}}]}}]}})
    incs = [n for n in names_of(runs) if n == "inc"]
    if len(incs) != 3:
        fail(f"Until ran its body {len(incs)}x, want exactly 3 before the "
             f"condition held: {names_of(runs)}")
    print("   IfCondition and Switch took one branch each; Until stopped at 3")


def check_retry_policy(ws: str) -> None:
    print("-- Retry policy: the attempts are real and recorded once")
    pid = az_rest("post", v1(f"workspaces/{ws}/items"),
                  {"displayName": "pl-retry", "type": "DataPipeline"})["id"]
    define(ws, pid, {"properties": {"activities": [
        {"name": "RunNb", "type": "TridentNotebook", "policy": {"retry": 2},
         "typeProperties": {"notebookId": "missing"}}]}})
    az_rest_raw("post", v1(f"workspaces/{ws}/items/{pid}/jobs/instances?jobType=Pipeline"),
                body="{}", headers=["Content-Type=application/json"])
    inst = None
    for _ in range(60):
        got = az_rest("get", v1(f"workspaces/{ws}/items/{pid}/jobs/instances")).get("value") or []
        inst = next((r for r in got if r.get("status") in ("Completed", "Failed")), None)
        if inst:
            break
        time.sleep(0.5)
    if not inst:
        fail("the retry pipeline never reached a terminal state")
    if inst["status"] != "Failed":
        fail(f"an activity pointing at a missing notebook reported {inst['status']}")
    detail = az_rest("post", v1(f"workspaces/{ws}/items/{pid}/jobs/instances/"
                                f"{inst['id']}/queryactivityruns"), {}, allow_error=True)
    runs = detail.get("value") or []
    # ONE record carrying retryAttempt=2, not three records: the retry is a
    # property of the attempt, and three rows would misreport the history.
    if len(runs) != 1:
        fail(f"expected 1 activity record after retries, got {len(runs)}: {names_of(runs)}")
    if runs[0].get("retryAttempt") != 2:
        fail(f"retryAttempt = {runs[0].get('retryAttempt')!r}, want 2")
    print("   job Failed, one record, retryAttempt=2")


def check_web_and_webhook(ws: str) -> None:
    print("-- Web + WebHook: a real call, and a park that only a callback releases")
    # Plain HTTP to a real service, not the emulator: see target.py for why.
    runs = run_pipeline(ws, "pl-web", {"properties": {"activities": [
        {"name": "Ping", "type": "WebActivity", "typeProperties": {
            "url": "http://target:8080/ping.json", "method": "GET"}}]}})
    out = output_of(runs, "Ping")
    body = out.get("body") if isinstance(out.get("body"), dict) else out
    if (body or {}).get("pong") is not True:
        fail(f"the Web activity's output does not carry the target's response: {out}")
    print("   Web: GET /ping.json → pong=true in the activity output")

    # WebHook. The receiver answers 200 and does NOT call back, so the activity
    # must PARK. That the job is still running is half the claim; the other half
    # is that our callback releases it and its body reaches the output.
    pid = az_rest("post", v1(f"workspaces/{ws}/items"),
                  {"displayName": "pl-webhook", "type": "DataPipeline"})["id"]
    define(ws, pid, {"properties": {"activities": [
        {"name": "Hook", "type": "WebHook", "typeProperties": {
            "url": "http://target:8080/hook", "method": "POST",
            "body": {"job": "nightly"}, "timeout": "00:05:00"}}]}})
    az_rest_raw("post", v1(f"workspaces/{ws}/items/{pid}/jobs/instances?jobType=Pipeline"),
                body="{}", headers=["Content-Type=application/json"])

    def instance():
        got = az_rest("get", v1(f"workspaces/{ws}/items/{pid}/jobs/instances")).get("value") or []
        return got[0] if got else None

    # urllib, not `az rest`: reading the receiver's own state is test
    # scaffolding, and az would attach an Azure token to a plain-HTTP endpoint
    # that neither wants nor understands one. The WITNESS is the emulator
    # calling `target`; how the driver inspects the receiver afterwards is not
    # part of the claim.
    cb, captured = "", {}
    for _ in range(40):
        try:
            with urllib.request.urlopen("http://target:8080/captured", timeout=5) as r:
                captured = json.loads(r.read().decode() or "{}")
        except OSError:
            captured = {}
        cb = captured.get("callBackUri") or ""
        if cb:
            break
        time.sleep(0.5)
    if not cb:
        fail(f"the WebHook never delivered a callBackUri to the receiver; "
             f"receiver state = {captured}")

    inst = instance()
    if inst and inst.get("status") in ("Completed", "Failed"):
        fail(f"the job reached {inst['status']} while the webhook was parked — "
             f"nothing had called back yet, so it must still be running")

    # TWO things the emulator's contract dictates here, and the first version
    # of this got both wrong by reaching for `az rest`:
    #
    #  1. `callBackUri` is a PATH, not an absolute URL — deliberately, because
    #     the emulator does not know which base a caller reached it on and a
    #     wrong absolute would be worse than none (internal/api/webhookactivity.go).
    #     The receiver prefixes the base it already used.
    #  2. The callback route takes NO bearer: an external receiver has no Fabric
    #     token, and possession of the exact URI is the authentication. `az rest`
    #     would attach one.
    #
    # So this is a plain POST with the emulator's own CA trusted — which is
    # exactly what a real receiver would do.
    ctx = ssl.create_default_context(cafile=azrest.AZ_ENV["REQUESTS_CA_BUNDLE"])
    callback_url = cb if cb.startswith("http") else FABRIC_BASE + cb
    req = urllib.request.Request(
        callback_url, data=json.dumps({"approved": True}).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=30, context=ctx) as r:
        if r.status != 200:
            fail(f"callback returned {r.status}")
    for _ in range(60):
        inst = instance()
        if inst and inst.get("status") in ("Completed", "Failed"):
            break
        time.sleep(0.5)
    if not inst or inst.get("status") != "Completed":
        fail(f"the callback did not release the parked webhook: {inst}")
    detail = az_rest("post", v1(f"workspaces/{ws}/items/{pid}/jobs/instances/"
                                f"{inst['id']}/queryactivityruns"), {}, allow_error=True)
    hook = output_of(detail.get("value") or [], "Hook")
    if hook.get("approved") is not True:
        fail(f"the callback body did not reach the activity output: {hook}")
    print("   WebHook: parked until the callback, and the callback body arrived")


def main() -> int:
    try:
        azrest.login()
        ws = new_workspace("pipeline-activities")
        check_delete(ws)
        check_getmetadata(ws)
        check_lookup(ws)
        check_validation(ws)
        check_execute_pipeline(ws)
        check_foreach(ws)
        check_control_flow(ws)
        check_retry_policy(ws)
        check_web_and_webhook(ws)
        print("\nPIPELINE ACTIVITIES E2E: PASS — az drove Delete, GetMetadata, "
              "Lookup, Validation, ExecutePipeline, ForEach, control flow, "
              "retry policy and Web — each judged by the data")
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
