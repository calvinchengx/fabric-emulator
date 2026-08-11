"""The examples' long-running-operation resolution, against the 202 shapes both targets send.

WHY THIS IS TESTED HERE. `create_item` used to carry a SECOND copy of
`post_and_wait`'s 202 handling, and the copy was the older one: it indexed
`x-ms-operation-id` with no fallback and slept a flat second regardless of what
the service asked for. These tests pin the two behaviours that copy lacked.

The two halves fail differently, and it is worth being exact about which is live:

  * **`Retry-After`, live on BOTH targets.** The emulator sets the header on
    its 202 (`internal/api/api.go`), and a definition-bearing create has ALWAYS
    been an LRO here (`internal/api/items.go`, the `body.Definition == nil`
    branch). `create_item` always sends a definition, so every example run takes
    the async path and the old copy polled once a second against a service that
    had stated a pace. Against a real tenant, whose documented sample answers
    `Retry-After: 20`, that is polling ~20x more often than asked on an API
    family that documents `429 Too Many Requests`.

  * **The missing-header `KeyError`, latent.** Fabric documents all three
    headers on the INITIATING 202, and the emulator sets them, so the hard index
    finds its key today on both targets. It is fragility rather than a live
    break: a resolver that requires a convenience header is asserting a promise
    the reference makes for the initiating call only, and `Location` is the part
    actually guaranteed.

Tested without an emulator because the failure is in how the CLIENT reads a
response shape, so a stub pins it precisely and CI needs no stack.
"""
import importlib
import os
import pathlib
import sys

import pytest

REPO = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "python" / "fabric-target"))
sys.path.insert(0, str(REPO / "examples" / "contoso-fixtures"))

import fabric_target  # noqa: E402


class FakeResponse:
    def __init__(self, payload, status_code=200, headers=None):
        self._payload = payload
        self.status_code = status_code
        self.headers = headers or {}
        self.text = str(payload)

    def json(self):
        return self._payload

    def raise_for_status(self):
        pass


class LROSession:
    """A create that answers 202, then an operation that runs before it succeeds.

    `pending` controls how many polls report Running, so a test can assert the
    waiter actually waited rather than reading a terminal status first time.
    """

    def __init__(self, accept_headers, pending=1, result=None, retry_after=None):
        self.accept_headers = accept_headers
        self.pending = pending
        self.result = result or {"id": "created-1"}
        self.retry_after = retry_after
        self.polls = 0

    def post(self, url, **_):
        return FakeResponse(None, status_code=202, headers=self.accept_headers)

    def get(self, url, **_):
        if url.endswith("/result"):
            return FakeResponse(self.result)
        self.polls += 1
        status = "Running" if self.polls <= self.pending else "Succeeded"
        headers = {"Retry-After": self.retry_after} if self.retry_after else {}
        return FakeResponse({"status": status}, headers=headers)


def load_common(monkeypatch, tmp_path):
    for k in ("FABRIC_TARGET", "FABRIC_WORKSPACE", "TDS_SERVER", "AZURE_CLIENT_SECRET",
              "AZURE_CLIENT_ID", "AZURE_TENANT_ID", "FABRIC_VAULT_URL",
              "AZURE_KEY_VAULT_URL", "PIPELINE_STATE"):
        monkeypatch.delenv(k, raising=False)
    for k in [k for k in os.environ if k.startswith("NOTEBOOKUTILS_")]:
        monkeypatch.delenv(k, raising=False)
    monkeypatch.setenv("FABRIC_TARGET", "emulator")
    monkeypatch.setenv("PIPELINE_STATE", str(tmp_path / "state.json"))
    monkeypatch.setattr(fabric_target, "_az_logged_in", lambda: True)
    fabric_target._cached = None
    sys.modules.pop("common", None)
    common = importlib.import_module("common")
    (tmp_path / "state.json").write_text('{"workspace": "ws-1"}')
    monkeypatch.setattr(common, "fabric_headers", lambda: {})
    return common


def no_sleeping(monkeypatch, common):
    """Record what the waiter asked to sleep, without spending it."""
    slept = []
    import time
    monkeypatch.setattr(time, "sleep", lambda s: slept.append(s))
    return slept


LOCATION_ONLY = {"Location": "https://api.fabric.microsoft.com/v1/operations/op-42/"}
BOTH = {"Location": "https://api.fabric.microsoft.com/v1/operations/op-42",
        "x-ms-operation-id": "op-42"}


def test_202_without_the_convenience_header_still_resolves(monkeypatch, tmp_path):
    """The regression that mattered: Fabric guarantees `Location`, not
    `x-ms-operation-id`. A resolver that indexes the header raises KeyError on a
    response the service was entitled to send."""
    common = load_common(monkeypatch, tmp_path)
    no_sleeping(monkeypatch, common)
    session = LROSession(LOCATION_ONLY)
    monkeypatch.setattr(common, "S", session)

    got = common.post_and_wait("https://f/v1/workspaces/ws-1/items", {"displayName": "x"})

    assert got == {"id": "created-1"}
    assert session.polls > 1, "returned before the operation reached a terminal state"


def test_create_item_resolves_the_same_202(monkeypatch, tmp_path):
    """create_item must not have its own opinion about this. It carried a copy
    that lacked the fallback, and a second implementation of one protocol is the
    defect: matching them by hand only resets the clock on the next divergence."""
    common = load_common(monkeypatch, tmp_path)
    no_sleeping(monkeypatch, common)
    monkeypatch.setattr(common, "S", LROSession(LOCATION_ONLY))

    assert common.create_item("gold", "Warehouse", {"x.txt": "hi"}) == "created-1"


def test_retry_after_is_honoured(monkeypatch, tmp_path):
    """Polling faster than the service asked earns a 429. The flat one-second
    sleep the old copy used ignores the header entirely."""
    common = load_common(monkeypatch, tmp_path)
    slept = no_sleeping(monkeypatch, common)
    monkeypatch.setattr(common, "S", LROSession(LOCATION_ONLY, pending=2, retry_after="7"))

    common.post_and_wait("https://f/v1/workspaces/ws-1/items", {})

    assert slept and all(s == 7 for s in slept), slept


def test_retry_after_is_capped(monkeypatch, tmp_path):
    """A service that asks for an hour should not hang the example for one."""
    common = load_common(monkeypatch, tmp_path)
    slept = no_sleeping(monkeypatch, common)
    monkeypatch.setattr(common, "S", LROSession(LOCATION_ONLY, pending=2, retry_after="3600"))

    common.post_and_wait("https://f/v1/workspaces/ws-1/items", {})

    assert slept and all(s == 20 for s in slept), slept


def test_operation_id_header_is_preferred_when_present(monkeypatch, tmp_path):
    """The fallback is a fallback. When Fabric does send the id, use it rather
    than parsing a URL."""
    common = load_common(monkeypatch, tmp_path)
    no_sleeping(monkeypatch, common)
    asked = []

    class Recording(LROSession):
        def get(self, url, **kw):
            asked.append(url)
            return super().get(url, **kw)

    monkeypatch.setattr(common, "S", Recording(BOTH))
    common.post_and_wait("https://f/v1/workspaces/ws-1/items", {})

    assert all("/operations/op-42" in u for u in asked), asked


def test_a_failed_operation_is_not_reported_as_success(monkeypatch, tmp_path):
    """A terminal Failed must surface. The whole class of bug this file guards
    is a wait that returns something plausible when the service said no."""
    common = load_common(monkeypatch, tmp_path)
    no_sleeping(monkeypatch, common)

    class Failing(LROSession):
        def get(self, url, **_):
            if url.endswith("/result"):
                return FakeResponse({"id": "should-not-be-read"})
            return FakeResponse({"status": "Failed"})

    monkeypatch.setattr(common, "S", Failing(LOCATION_ONLY))
    with pytest.raises(AssertionError, match="Failed"):
        common.post_and_wait("https://f/v1/workspaces/ws-1/items", {})


def test_no_example_carries_its_own_operation_loop():
    """The structural half of "one resolver".

    Delegation fixed `create_item`, and there were FOUR more copies of the same
    loop in the per-example `semantic_model.py` scripts, which are what people
    actually open. A behavioural test cannot see them: each copy is correct in
    its own way, publishes fine against the emulator, and simply polls too fast.
    So this asserts the shape instead. `common.py` is the one file allowed to
    resolve an operation; anywhere else, it is a copy that will drift.
    """
    offenders = []
    for path in (REPO / "examples").rglob("*.py"):
        if path.name == "common.py":
            continue
        text = path.read_text()
        if '"x-ms-operation-id"' in text and "/operations/" in text:
            offenders.append(str(path.relative_to(REPO)))
    assert not offenders, (
        "these resolve an LRO themselves instead of calling common.post_and_wait "
        f"or common.create_item: {offenders}")


def test_synchronous_create_is_unchanged(monkeypatch, tmp_path):
    """The 201 path. `create_item` always sends a definition so the emulator never
    answers it this way, but Fabric documents 201 for a create and a definitionless
    caller would take it, so it must survive a change aimed at the other branch."""
    common = load_common(monkeypatch, tmp_path)

    class Sync:
        def post(self, url, **_):
            return FakeResponse({"id": "sync-1"}, status_code=201)

    monkeypatch.setattr(common, "S", Sync())
    assert common.create_item("bronze", "Lakehouse", {"x.txt": "hi"}) == "sync-1"
