r"""The ratchet, held to the standard it holds the tree to.

check_route_coverage fails when the baseline and the tree disagree. Both of its
halves can fail silently and the result reads as a healthy tree:

  * REGISTRATION PARSING. If the regex stops matching, `registered()` returns
    an empty set, every route is "covered" vacuously, and the gate passes
    forever while measuring nothing.
  * TEMPLATE MATCHING. If a recorded path never matches its template, nothing
    is credited, the uncovered set becomes everything, and the baseline diff is
    so large nobody reads it -- which is the same outcome as no gate at all.

So the tests below assert each half FINDS things, and assert the gate fires in
BOTH directions: a new unexercised route, and an exercised route still listed
as not yet. The second is the one people will be tempted to soften, because it
fails on an improvement; it is also the one that keeps the file honest.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_route_coverage as c  # noqa: E402

GO_SOURCE = '''
package api

func (a *API) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/widgets", a.listWidgets)
	mux.HandleFunc("POST /v1/widgets", a.createWidget)
	mux.HandleFunc("GET /v1/widgets/{wid}", a.getWidget)
	mux.HandleFunc("GET /v1.0/myorg/gadgets", a.listGadgets)
	// Out of scope: the recorder does not write these down, so requiring
	// evidence for them would demand what cannot be produced.
	mux.HandleFunc("GET /_emulator/portal/widgets", a.portalWidgets)
	mux.HandleFunc("GET /onelake/ws/item", a.onelake)
}
'''

# A test file must not contribute routes: its registrations are fixtures, not
# surfaces the emulator serves.
GO_TEST_SOURCE = '''
package api

func TestSomething(t *testing.T) {
	mux.HandleFunc("GET /v1/only-in-a-test", nil)
}
'''


@pytest.fixture
def tree(tmp_path, monkeypatch):
    src = tmp_path / "api"
    src.mkdir()
    (src / "routes.go").write_text(GO_SOURCE)
    (src / "routes_test.go").write_text(GO_TEST_SOURCE)
    monkeypatch.setattr(c, "SOURCES", (src,))
    monkeypatch.setattr(c, "BASELINE", tmp_path / "route-coverage.json")
    return tmp_path


def recording(tmp_path, entries, name="rec.jsonl"):
    path = tmp_path / name
    path.write_text("\n".join(json.dumps(e) for e in entries) + "\n")
    return path


def hit(method, path):
    return {"method": method, "path": path, "status": 200, "body": {}}


# --- registration parsing -------------------------------------------------------

def test_registered_finds_the_in_scope_routes(tree):
    assert c.registered() == {
        "GET /v1/widgets", "POST /v1/widgets", "GET /v1/widgets/{wid}",
        "GET /v1.0/myorg/gadgets",
    }


def test_out_of_scope_prefixes_are_not_demanded(tree):
    """The recorder skips them, so requiring evidence would be impossible."""
    assert not any("_emulator" in r or "onelake" in r for r in c.registered())


def test_a_test_file_does_not_contribute_routes(tree):
    assert "GET /v1/only-in-a-test" not in c.registered()


def test_the_real_tree_registers_a_plausible_number():
    """The parser against the tree it guards.

    An empty set is the silent failure: every route would count as covered and
    the gate would pass forever while measuring nothing.
    """
    assert len(c.registered()) > 50


# --- template matching ----------------------------------------------------------

def test_a_recorded_path_credits_its_template(tree, tmp_path):
    rec = recording(tmp_path, [hit("GET", "/v1/widgets/abc-123")])
    assert c.exercised([rec]) == {"GET /v1/widgets/{wid}"}


def test_the_method_must_match_too(tree, tmp_path):
    rec = recording(tmp_path, [hit("POST", "/v1/widgets")])
    assert c.exercised([rec]) == {"POST /v1/widgets"}


def test_the_longer_template_wins(tree, tmp_path):
    """`/v1/widgets/{wid}` and `/v1/widgets` can both be plausible reads of a
    path; crediting the shorter would mark the wrong route proved."""
    rec = recording(tmp_path, [hit("GET", "/v1/widgets/abc")])
    assert c.exercised([rec]) == {"GET /v1/widgets/{wid}"}


def test_several_recordings_are_unioned(tree, tmp_path):
    a = recording(tmp_path, [hit("GET", "/v1/widgets")], "a.jsonl")
    b = recording(tmp_path, [hit("POST", "/v1/widgets")], "b.jsonl")
    assert c.exercised([a, b]) == {"GET /v1/widgets", "POST /v1/widgets"}


def test_a_truncated_line_does_not_stop_the_scan(tree, tmp_path):
    path = tmp_path / "rec.jsonl"
    path.write_text(json.dumps(hit("GET", "/v1/widgets")) + "\n{\"method\":\"GE")
    assert c.exercised([path]) == {"GET /v1/widgets"}


# --- the gate, both directions --------------------------------------------------

def baseline_of(tree, uncovered):
    (tree / "route-coverage.json").write_text(json.dumps({
        "exercised": 0, "registered": 4, "notYetExercised": sorted(uncovered)}))


def run(tmp_path, recordings, argv_extra=()):
    monkey = pytest.MonkeyPatch()
    monkey.setattr(sys, "argv",
                   ["check_route_coverage.py", *[str(r) for r in recordings], *argv_extra])
    try:
        return c.main()
    finally:
        monkey.undo()


def test_a_matching_baseline_passes(tree, tmp_path, capsys):
    rec = recording(tmp_path, [hit("GET", "/v1/widgets")])
    baseline_of(tree, {"POST /v1/widgets", "GET /v1/widgets/{wid}", "GET /v1.0/myorg/gadgets"})
    assert run(tmp_path, [rec], ("--strict",)) == 0
    assert "not yet exercised, as recorded" in capsys.readouterr().out


def test_a_new_unexercised_route_fails(tree, tmp_path, capsys):
    """An endpoint must arrive with its evidence, not acquire it later."""
    rec = recording(tmp_path, [hit("GET", "/v1/widgets")])
    baseline_of(tree, {"POST /v1/widgets", "GET /v1/widgets/{wid}"})  # gadgets missing
    assert run(tmp_path, [rec], ("--strict",)) == 1
    out = capsys.readouterr().out
    assert "GET /v1.0/myorg/gadgets" in out
    assert "no conformance traffic" in out


def test_a_route_that_stopped_being_exercised_fails(tree, tmp_path, capsys):
    """Coverage regressing quietly is the same defect a stale witness is."""
    rec = recording(tmp_path, [])
    baseline_of(tree, {"POST /v1/widgets", "GET /v1/widgets/{wid}", "GET /v1.0/myorg/gadgets"})
    assert run(tmp_path, [rec], ("--strict",)) == 1
    assert "GET /v1/widgets" in capsys.readouterr().out


def test_an_improvement_that_does_not_shrink_the_baseline_fails(tree, tmp_path, capsys):
    """The direction people will want to soften, because it fails on progress.

    It is also the one that keeps the file honest: the baseline records what is
    NOT proved, so a proved route left in it is a lie somebody will later read
    as a gap.
    """
    rec = recording(tmp_path, [hit("GET", "/v1/widgets"), hit("POST", "/v1/widgets")])
    baseline_of(tree, {"POST /v1/widgets", "GET /v1/widgets/{wid}", "GET /v1.0/myorg/gadgets"})
    assert run(tmp_path, [rec], ("--strict",)) == 1
    out = capsys.readouterr().out
    assert "now exercised but still" in out
    assert "POST /v1/widgets" in out


def test_without_strict_it_reports_but_exits_zero(tree, tmp_path):
    rec = recording(tmp_path, [hit("GET", "/v1/widgets")])
    baseline_of(tree, set())
    assert run(tmp_path, [rec]) == 0


def test_update_writes_a_baseline_that_then_passes(tree, tmp_path, capsys):
    rec = recording(tmp_path, [hit("GET", "/v1/widgets")])
    assert run(tmp_path, [rec], ("--update",)) == 0
    written = json.loads((tree / "route-coverage.json").read_text())
    assert written["exercised"] == 1 and written["registered"] == 4
    assert "GET /v1/widgets" not in written["notYetExercised"]
    capsys.readouterr()
    assert run(tmp_path, [rec], ("--strict",)) == 0


def test_a_missing_recording_fails_and_says_how_to_produce_one(tree, tmp_path, capsys):
    assert run(tmp_path, [tmp_path / "gone.jsonl"], ("--strict",)) == 1
    assert "FABRIC_RECORD_RESPONSES" in capsys.readouterr().err


def test_a_missing_baseline_fails_rather_than_inventing_one(tree, tmp_path, capsys):
    rec = recording(tmp_path, [hit("GET", "/v1/widgets")])
    assert run(tmp_path, [rec], ("--strict",)) == 1
    assert "no baseline" in capsys.readouterr().err


def test_the_real_baseline_matches_the_real_tree():
    """The committed baseline against the committed routes.

    Not a fixture: if these two drift, every other test here still passes and
    the gate is the only thing that would have said so.
    """
    assert c.BASELINE.is_file(), f"{c.BASELINE} is missing"
    recorded = set(json.loads(c.BASELINE.read_text())["notYetExercised"])
    assert recorded <= c.registered(), (
        "the baseline lists routes the emulator no longer registers: "
        f"{sorted(recorded - c.registered())}")


# --- assembled registrations ----------------------------------------------------
#
# The parser used to require a closing quote right after the path, so a route
# built from a variable was invisible: not counted, and therefore never
# reportable as uncovered. Thirty-four routes sat outside the denominator,
# including every dataset `refreshes` and `datasources` route -- served,
# exercised, and uncounted. These pin each form that was missed.

ASSEMBLED_SOURCE = '''
package api

func (a *API) registerAssembled(mux *http.ServeMux) {
	base := "/v1/workspaces/{wid}/items/{iid}/shortcuts"
	mux.HandleFunc("GET "+base, a.listShortcuts)
	mux.HandleFunc("DELETE "+base+"/{path}/{name}", a.deleteShortcut)

	// A range over a slice literal whose ITEMS CONTAIN BRACES. Terminating the
	// literal at the first `}` cuts this after `{datasetId`, which is exactly
	// how the first fix for this stayed broken.
	for _, prefix := range []string{
		"/v1.0/myorg/datasets/{datasetId}",
		"/v1.0/myorg/groups/{groupId}/datasets/{datasetId}",
	} {
		mux.HandleFunc("POST "+prefix+"/refreshes", a.postRefresh)
	}

	// The METHOD can be the variable instead of the path.
	const p = "/v1/proxy/"
	for _, m := range []string{"GET", "DELETE"} {
		mux.HandleFunc(m+" "+p+"{rest...}", a.proxy)
	}
}
'''


@pytest.fixture
def assembled(tmp_path, monkeypatch):
    src = tmp_path / "api"
    src.mkdir()
    (src / "assembled.go").write_text(ASSEMBLED_SOURCE)
    monkeypatch.setattr(c, "SOURCES", (src,))
    monkeypatch.setattr(c, "UNPARSED_OK", {})
    return tmp_path


def test_a_route_built_from_a_variable_is_counted(assembled):
    found = c.registered()
    assert "GET /v1/workspaces/{wid}/items/{iid}/shortcuts" in found
    assert "DELETE /v1/workspaces/{wid}/items/{iid}/shortcuts/{path}/{name}" in found


def test_a_slice_literal_of_paths_containing_braces_expands(assembled):
    found = c.registered()
    assert "POST /v1.0/myorg/datasets/{datasetId}/refreshes" in found
    assert "POST /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/refreshes" in found


def test_the_method_may_be_the_variable(assembled):
    found = c.registered()
    assert "GET /v1/proxy/{rest...}" in found
    assert "DELETE /v1/proxy/{rest...}" in found
    assert "POST /v1/proxy/{rest...}" not in found


def test_every_registration_in_that_file_resolved(assembled):
    """No surprises: the fixture is fully parsed, so the allowlist stays empty."""
    assert c.unparsed_registrations() == []


UNPARSEABLE_SOURCE = '''
package api

func (a *API) registerOdd(mux *http.ServeMux) {
	mux.HandleFunc(buildRoute("GET", "/v1/surprise"), a.surprise)
}
'''


def test_an_unresolvable_registration_is_reported_not_dropped(tmp_path, monkeypatch):
    """The whole lesson: going unparsed must be LOUD.

    A form the parser cannot resolve is not a route that quietly leaves the
    denominator -- it stops the gate until somebody either teaches the parser
    or records why it is not a route template.
    """
    src = tmp_path / "api"
    src.mkdir()
    (src / "odd.go").write_text(UNPARSEABLE_SOURCE)
    monkeypatch.setattr(c, "SOURCES", (src,))
    monkeypatch.setattr(c, "UNPARSED_OK", {})
    surprises = c.unparsed_registrations()
    assert len(surprises) == 1
    assert surprises[0][0].endswith("odd.go")


def test_an_allowlisted_registration_is_not_a_surprise(tmp_path, monkeypatch):
    src = tmp_path / "api"
    src.mkdir()
    (src / "odd.go").write_text(UNPARSEABLE_SOURCE)
    monkeypatch.setattr(c, "SOURCES", (src,))
    monkeypatch.setattr(c, "UNPARSED_OK", {})
    allow = {rel: count for rel, count, _ in c.unparsed_registrations()}
    monkeypatch.setattr(c, "UNPARSED_OK", allow)
    assert c.unparsed_registrations() == []


def test_the_real_tree_has_no_unexplained_registration():
    """Every HandleFunc in the tree is either parsed or named in UNPARSED_OK."""
    assert c.unparsed_registrations() == []
