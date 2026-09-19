r"""The ledger, held to the standard it holds the tree to.

check_surface_ledger classifies every documented operation as served, refused
or silent. Both halves can fail silently and read as a healthy tree:

  * If `documented()` returns nothing, every count is zero, the baseline
    matches trivially and the gate passes forever while measuring nothing.
  * If `_shape` stopped folding parameter names, NOTHING would ever match --
    the spec says {workspaceId} and this emulator says {wid} -- so `served`
    would collapse to zero and the silent set would swallow the whole API.

So the tests assert each half FINDS things, and assert the gate fires in both
directions: a regression out of served, and an improvement that left the
baseline stale.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_surface_ledger as L  # noqa: E402

# --- shape folding --------------------------------------------------------------

def test_parameter_names_do_not_matter():
    """The spec's spelling and the emulator's are the same route."""
    assert L._shape("/v1/workspaces/{workspaceId}/items") == \
        L._shape("/v1/workspaces/{wid}/items")


def test_a_wildcard_folds_like_any_other_parameter():
    assert L._shape("/v1/a/{path...}") == L._shape("/v1/a/{p}")


def test_a_trailing_slash_is_not_a_different_route():
    assert L._shape("/v1/a/") == L._shape("/v1/a")


def test_different_routes_stay_different():
    assert L._shape("/v1/a/{x}") != L._shape("/v1/a/{x}/b")


# --- the real specs -------------------------------------------------------------

def test_the_specs_document_a_plausible_number_of_operations():
    """An empty index is the silent failure: everything would be 'silent'."""
    ops, _specs = L.documented()
    assert len(ops) > 500


def test_the_tree_serves_a_plausible_number():
    """And an empty registration set would put the whole API in 'silent'."""
    shapes = L.served_shapes()
    assert sum(len(v) for v in shapes.values()) > 50


def test_every_documented_operation_is_in_exactly_one_state():
    states = L.classify([])
    assert set(states.values()) <= {"served", "refused", "silent"}
    ops, _ = L.documented()
    assert len(states) == len(ops)


def test_with_no_recordings_nothing_is_refused():
    """A refusal is EVIDENCE, not a declaration: no recording, no credit."""
    states = L.classify([])
    assert "refused" not in set(states.values())


# --- the gate -------------------------------------------------------------------

@pytest.fixture
def baseline(tmp_path, monkeypatch):
    monkeypatch.setattr(L, "BASELINE", tmp_path / "surface-ledger.json")
    return L.BASELINE


def write(baseline, served=(), refused=()):
    baseline.write_text(json.dumps({
        "documented": 0, "served": len(served), "refused": len(refused),
        "silent": 0,
        "servedOperations": sorted(served),
        "refusedOperations": sorted(refused),
    }), encoding="utf-8")


def test_a_baseline_written_from_the_tree_round_trips(baseline):
    """--update then --strict must agree, or the gate fails on its own output."""
    states = L.classify([])
    L.write_baseline(states)
    base = json.loads(baseline.read_text())
    assert base["servedOperations"] == sorted(k for k, v in states.items() if v == "served")
    assert base["served"] + base["refused"] + base["silent"] == base["documented"]
    assert L.main_for_test(strict=True) == 0


def test_the_gate_fires_when_a_served_operation_disappears(baseline, monkeypatch):
    monkeypatch.setattr(L, "classify", lambda _r: {})
    write(baseline, served=["GET /v1/gone"])
    assert L.main_for_test(strict=True) == 1


def test_the_gate_fires_on_an_improvement_that_left_the_baseline_stale(baseline, monkeypatch):
    """The one people soften, because it fails on something getting better."""
    monkeypatch.setattr(L, "classify", lambda _r: {"GET /v1/new": "served"})
    write(baseline, served=[])
    assert L.main_for_test(strict=True) == 1


def test_the_gate_fires_when_a_refusal_stops_being_measured(baseline, monkeypatch):
    monkeypatch.setattr(L, "classify", lambda _r: {"GET /v1/x": "silent"})
    write(baseline, refused=["GET /v1/x"])
    assert L.main_for_test(strict=True) == 1


def test_the_gate_passes_when_the_ledger_agrees(baseline, monkeypatch):
    monkeypatch.setattr(L, "classify",
                        lambda _r: {"GET /v1/a": "served", "GET /v1/b": "refused",
                                    "GET /v1/c": "silent"})
    write(baseline, served=["GET /v1/a"], refused=["GET /v1/b"])
    assert L.main_for_test(strict=True) == 0


def test_a_missing_baseline_is_refused_under_strict(baseline, monkeypatch):
    monkeypatch.setattr(L, "classify", lambda _r: {})
    assert L.main_for_test(strict=True) == 1
