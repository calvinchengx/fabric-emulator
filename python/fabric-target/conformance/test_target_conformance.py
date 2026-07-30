"""Dual-target conformance: the same tests run against fabric-emulator and
against real Microsoft Fabric — `FABRIC_TARGET` decides, the tests never
branch on it (except to assert the guards themselves).

    FABRIC_TARGET=emulator FABRIC_WORKSPACE=<name> pytest -m target
    FABRIC_TARGET=real     FABRIC_WORKSPACE=<name> pytest -m target

Every divergence this finds against real Fabric is parity-map material —
the toggle doubles as a fidelity oracle (docs/21, T1).

Requirements either way: the scoped workspace exists and the credential has
a role on it. Tests create and delete their own items (cleanup opts into the
destructive gate explicitly, scoped to items the suite created).
"""
import os
import uuid

import pytest

from fabric_target import FABRIC_SCOPE, STORAGE_SCOPE, TargetError, target

pytestmark = pytest.mark.target


@pytest.fixture(scope="module")
def t():
    tgt = target(fresh=True)
    if not tgt.workspace_scope:
        pytest.skip("FABRIC_WORKSPACE not set — conformance is always workspace-scoped")
    return tgt


@pytest.fixture(scope="module")
def ws(t):
    return t.workspace()


def test_credential_mints_per_scope(t):
    fab = t.credential.get_token(FABRIC_SCOPE)
    sto = t.credential.get_token(STORAGE_SCOPE)
    assert fab.token and sto.token and fab.token != sto.token
    assert fab.expires_on > 0


def test_workspace_resolves_by_name(t, ws):
    assert ws.id and ws.display_name == t.workspace_scope


def test_workspace_listing_contains_scope(t, ws):
    r = t.session().get("/workspaces")
    assert r.status_code == 200
    assert any(w["id"] == ws.id for w in r.json()["value"])


def test_item_lifecycle_with_lro(t, ws):
    name = f"conformance-{uuid.uuid4().hex[:8]}"
    s = t.session()
    r = s.post(f"/workspaces/{ws.id}/items",
               json={"displayName": name, "type": "Notebook"})
    assert r.status_code in (201, 202), (r.status_code, r.text[:200])
    final = t.poll_lro(r)
    assert final.status_code in (200, 201), final.status_code

    items = s.get(f"/workspaces/{ws.id}/items").json()["value"]
    mine = [i for i in items if i["displayName"] == name]
    assert len(mine) == 1, f"created item not listed: {name}"

    # Cleanup — explicitly opt into the destructive gate for the item WE made.
    os.environ["FABRIC_TARGET_ALLOW_DESTRUCTIVE"] = "1"
    try:
        r = s.delete(f"/workspaces/{ws.id}/items/{mine[0]['id']}")
        assert r.status_code in (200, 202, 204), r.status_code
        t.poll_lro(r)
    finally:
        os.environ.pop("FABRIC_TARGET_ALLOW_DESTRUCTIVE", None)


def test_throttling_contract_shape(t, ws):
    # Not a load test: just prove the session survives a burst without a
    # raw 429 escaping (real Fabric throttles; the emulator can rehearse it).
    s = t.session()
    for _ in range(5):
        assert s.get(f"/workspaces/{ws.id}").status_code == 200


def test_emulator_only_guard_matches_target(t):
    if t.is_emulator:
        t.emulator_only("clock control")  # allowed here
    else:
        with pytest.raises(TargetError, match="emulator-only"):
            t.emulator_only("clock control")


def test_destructive_gate_matches_target(t, ws):
    if t.is_real:
        with pytest.raises(TargetError, match="ALLOW_DESTRUCTIVE"):
            t.session().delete(f"/workspaces/{ws.id}/items/nonexistent")
    else:
        # Locally ungated: the call goes through (and 404s harmlessly).
        assert t.session().delete(
            f"/workspaces/{ws.id}/items/nonexistent").status_code in (404, 400)
