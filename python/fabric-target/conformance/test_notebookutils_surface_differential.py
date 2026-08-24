"""Phase 5 (docs/56): the notebookutils SURFACE, against either target.

Everything in Axes A-C is conformance to a PUBLISHED CONTRACT. That is the
strongest evidence available in this repo and it is still not Fabric: the two
diverge exactly where the documentation is wrong or silent, which is precisely
where an emulator built from the same page will be wrong in the same direction.
Only a differential against a real tenant can tell those apart.

    FABRIC_TARGET=emulator FABRIC_WORKSPACE=<name> pytest -m target
    FABRIC_TARGET=real     FABRIC_WORKSPACE=<name> pytest -m target

The tests never branch on the target. What they assert is the REST surface the
shim is built on — the item collections and definition round-trips that
`notebookutils.lakehouse`, `.notebook` and `.udf` call underneath — because
that is the half a differential can reach. The shim's own signatures are
already graded by contract 2 on every conformance run; what nothing has ever
checked is whether the ENDPOINTS those signatures call exist on real Fabric
with the shapes this emulator serves.

WHAT A FAILURE HERE MEANS. Not "the test is wrong". A divergence is parity-map
material either way: the emulator is wrong, or the documentation this was built
from is, and both are worth knowing. That is the point of running the same
assertions against both.
"""
import os
import sys
import uuid

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from fabric_target import target  # noqa: E402

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


def _resolve(t, ws, name):
    """The item's id, by display NAME.

    NOT from the create response. For a 202 create, `poll_lro` returns the
    OPERATION, whose `id` is the operation's — reading it as the item's is a
    404 waiting to happen, and was: this test failed exactly that way on its
    first run. Names are the cross-target contract anyway; GUIDs are not.
    """
    items = t.session().get(f"/workspaces/{ws.id}/items").json()["value"]
    mine = [i for i in items if i["displayName"] == name]
    assert len(mine) == 1, f"created item not listed: {name}"
    return mine[0]["id"]


def _cleanup(t, ws, item_id):
    os.environ["FABRIC_TARGET_ALLOW_DESTRUCTIVE"] = "1"
    try:
        r = t.session().delete(f"/workspaces/{ws.id}/items/{item_id}")
        if r.status_code in (200, 202, 204):
            t.poll_lro(r)
    finally:
        os.environ.pop("FABRIC_TARGET_ALLOW_DESTRUCTIVE", None)


# --- the typed collections the shim addresses --------------------------------


@pytest.mark.parametrize("collection", ["lakehouses", "notebooks"])
def test_the_typed_collection_the_shim_calls_exists(t, ws, collection):
    """`notebookutils.lakehouse.list()` GETs `/workspaces/{id}/lakehouses` and
    `notebook.list()` GETs `/notebooks`. If a real tenant answers 404 on either,
    the shim is calling a URL that does not exist there."""
    r = t.session().get(f"/workspaces/{ws.id}/{collection}")
    assert r.status_code == 200, (collection, r.status_code, r.text[:200])
    assert "value" in r.json(), r.json()


def test_user_data_functions_is_addressable_as_a_typed_collection(t, ws):
    """Added to the emulator for `notebookutils.udf.getFunctions`, with the
    segment taken from Microsoft's own URLs. THIS is the assertion that decides
    whether that reading was right — a real tenant either serves it or does
    not.

    A 403 is not a failure of the URL: the credential may lack the role. Only a
    404 says the collection is not there.
    """
    r = t.session().get(f"/workspaces/{ws.id}/userDataFunctions")
    assert r.status_code != 404, (
        "userDataFunctions is not a collection on this target — the segment in "
        "internal/api/definitions.go was read from the REST reference and would "
        "be wrong")
    assert r.status_code in (200, 401, 403), (r.status_code, r.text[:200])


# --- the definition round-trip `nbResPath` and `%run` are built on -----------


def test_a_notebook_definition_round_trips_with_its_parts(t, ws):
    """`%run` and `nbResPath` both READ a notebook's definition parts —
    `%run` for `notebook-content.py`, `nbResPath` for everything under
    `builtin/`. If a real tenant does not return the parts a create supplied,
    both features are built on sand.
    """
    import base64

    name = f"nbu-surface-{uuid.uuid4().hex[:8]}"
    s = t.session()
    source = "# Fabric notebook source\n\n# CELL ********************\nx = 1\n"
    payload = base64.b64encode(source.encode()).decode()
    r = s.post(f"/workspaces/{ws.id}/notebooks", json={
        "displayName": name,
        "definition": {"parts": [{
            "path": "notebook-content.py",
            "payload": payload, "payloadType": "InlineBase64"}]}})
    assert r.status_code in (201, 202), (r.status_code, r.text[:300])
    t.poll_lro(r)
    item_id = _resolve(t, ws, name)

    try:
        got = s.post(f"/workspaces/{ws.id}/notebooks/{item_id}/getDefinition")
        assert got.status_code in (200, 202), (got.status_code, got.text[:200])
        body = t.poll_lro(got).json()
        parts = (body.get("definition") or {}).get("parts") or []
        paths = [p.get("path") for p in parts]
        assert "notebook-content.py" in paths, paths
        # The PAYLOAD must survive, not just the path — `%run` executes what
        # comes back, so a definition that round-trips an empty part would run
        # nothing and report success.
        content = next(p for p in parts if p["path"] == "notebook-content.py")
        assert base64.b64decode(content["payload"]).decode().strip(), \
            "the definition round-tripped an empty part"
    finally:
        _cleanup(t, ws, item_id)


def test_getDefinition_is_posted_not_got(t, ws):
    """The shim POSTs it, which is what the REST reference documents. A target
    that answered GET instead would make every definition read in this repo
    wrong in the same way."""
    name = f"nbu-verb-{uuid.uuid4().hex[:8]}"
    s = t.session()
    r = s.post(f"/workspaces/{ws.id}/items",
               json={"displayName": name, "type": "Notebook"})
    assert r.status_code in (201, 202), r.status_code
    t.poll_lro(r)
    item_id = _resolve(t, ws, name)
    try:
        got = s.get(f"/workspaces/{ws.id}/items/{item_id}/getDefinition")
        assert got.status_code in (404, 405), (
            "getDefinition answered a GET; the shim POSTs it, per the REST "
            f"reference (got {got.status_code})")
    finally:
        _cleanup(t, ws, item_id)
