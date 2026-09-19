r"""The undocumented-route gate, held to the standard it holds the tree to.

check_undocumented_routes answers the one question its three siblings cannot:
which routes does this emulator register that no published spec documents. It
has three halves, and EVERY ONE OF THEM FAILS SILENTLY AS A HEALTHY TREE:

  * If `documented_shapes()` returns nothing, every registered route is
    undocumented -- 613 of them -- the baseline diff is unreadable, and
    whoever regenerates it pins the whole surface as a finding.
  * If `cov.registered()` returns nothing, there is nothing to classify, the
    baseline matches trivially, and the gate passes forever measuring nothing.
    THIS IS THE DANGEROUS ONE: it reads exactly like a clean tree.
  * If the alias rule gets LOOSER, an emulator-native route is credited
    against a documented operation it has nothing to do with. Measured before
    this landed: a rule that swapped any segment for `items` credited
    `POST /v1/workspaces/{wid}/lineage` against the documented
    `POST /v1/workspaces/{workspaceId}/items`.

So the tests below assert each half FINDS things, pin the false-alias case as
a test rather than a comment, and fire the ratchet in both directions.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_route_coverage as cov  # noqa: E402
import check_surface_ledger as led  # noqa: E402
import check_undocumented_routes as u  # noqa: E402

# A miniature swagger, in the two-tree shape this repository vendors: enough
# documented operations that a fixture route can be credited, and deliberately
# NOT documenting the emulator-native ones.
MINI_SWAGGER = {
    "basePath": "/v1",
    "paths": {
        "/workspaces/{workspaceId}/items": {
            "get": {"responses": {"200": {}}},
            "post": {"responses": {"201": {}}},
        },
        "/workspaces/{workspaceId}/items/{itemId}": {
            "get": {"responses": {"200": {}}},
        },
        # A query VARIANT, which the loader splits off -- the same route.
        "/workspaces/{workspaceId}/widgets?preview=true": {
            "get": {"responses": {"200": {}}},
        },
    },
}

# Two typed collections and one generic route, registered the way the tree
# does it: a literal, and a family assembled from a map literal's keys.
GO_SOURCE = '''
package api

var typedCollections = map[string]string{
	"notebooks":  "Notebook",
	"warehouses": "Warehouse",
}

func (a *API) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{wid}/items", a.listItems)
	mux.HandleFunc("GET /v1/workspaces/{wid}/widgets", a.listWidgets)
	// Emulator-native: no documented operation, no typed-collection spelling.
	mux.HandleFunc("POST /v1/workspaces/{wid}/lineage", a.reportLineage)
	mux.HandleFunc("GET /v1/workspaces/{wid}/items/{iid}/_emulator/access", a.access)
	for collection, itemType := range typedCollections {
		mux.HandleFunc("GET /v1/workspaces/{wid}/"+collection+"/{iid}", a.typedGet(itemType))
	}
}
'''


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """A whole miniature repository: one spec tree, one Go source, one baseline."""
    spec = tmp_path / "specs" / "platform"
    spec.mkdir(parents=True)
    (spec / "swagger.json").write_text(json.dumps(MINI_SWAGGER))

    src = tmp_path / "api"
    src.mkdir()
    (src / "routes.go").write_text(GO_SOURCE)

    monkeypatch.setattr(u, "SPEC_ROOTS", (tmp_path / "specs",))
    monkeypatch.setattr(u, "BASELINE", tmp_path / "undocumented-routes.json")
    monkeypatch.setattr(u, "NOTES", {})
    monkeypatch.setattr(cov, "SOURCES", (src,))
    monkeypatch.setattr(cov, "ROOT", tmp_path)
    monkeypatch.setattr(cov, "UNPARSED_OK", {})
    monkeypatch.setattr(cov, "PARAMETERISED", {"api/routes.go:collection": "test family"})
    # The fixture declares ONE native pattern, so the suppression and the
    # stale-declaration tests are about the mechanism rather than about which
    # of the real tree's surfaces happens to be declared today.
    native(monkeypatch, {r"/_emulator/access": "the emulator's own lever"})
    return tmp_path


def native(monkeypatch, table):
    monkeypatch.setattr(u, "NATIVE", table)
    monkeypatch.setattr(
        u, "_NATIVE",
        {__import__("re").compile(p): r for p, r in table.items()})


# --- the halves find things -----------------------------------------------------

def test_the_real_specs_document_a_plausible_number():
    """An empty index would make the entire tree read as undocumented."""
    shapes = u.documented_shapes()
    assert sum(len(v) for v in shapes.values()) > 500


def test_the_real_tree_registers_a_plausible_number():
    """An empty registration set is the silent pass: nothing to classify."""
    assert len(cov.registered()) > 50


def test_the_alias_names_come_from_the_source():
    names = u.alias_collections()
    assert "notebooks" in names
    # collectionSpellings()'s initial-capital variant, which the emulator
    # really registers because ASP.NET matches segments case-insensitively.
    assert "Notebooks" in names
    # And the capitalised-in-the-reference one is unchanged by that rule.
    assert "GraphQLApis" in names


def test_the_shape_rule_agrees_with_the_surface_ledger():
    """Two halves of one comparison: spec-to-tree there, tree-to-spec here.

    A shape rule that drifted between them would let a route be `served` in
    the ledger and `undocumented` here -- not a finding, an inconsistency
    wearing one.
    """
    for template in ("/v1/workspaces/{workspaceId}/items/{itemId}",
                     "/v1/workspaces/{wid}/items/{iid}",
                     "/v1/a/{path...}", "/v1/a/"):
        assert u._shape(template) == led._shape(template)


# --- classification -------------------------------------------------------------

def test_a_documented_route_is_not_reported(tree):
    assert u.classify()["GET /v1/workspaces/{wid}/items"] == "documented"


def test_a_query_variant_still_counts_as_documented(tree):
    """The spec keys `?preview=true` as its own path; it is one route."""
    assert u.classify()["GET /v1/workspaces/{wid}/widgets"] == "documented"


def test_a_typed_collection_spelling_is_credited_as_an_alias(tree):
    """`/notebooks/{iid}` IS `/items/{itemId}` with the type forced."""
    states = u.classify()
    assert states["GET /v1/workspaces/{wid}/notebooks/{iid}"] == "alias"
    assert states["GET /v1/workspaces/{wid}/warehouses/{iid}"] == "alias"


def test_the_alias_rule_refuses_a_segment_that_is_not_a_collection(tree):
    """THE MEASURED FALSE POSITIVE, pinned as a test rather than a comment.

    A rule that swapped ANY segment for `items` credits
    `POST /v1/workspaces/{wid}/lineage` against the documented
    `POST /v1/workspaces/{workspaceId}/items`, because `lineage` is a segment
    and the substitution lands. Two emulator-native routes would then have
    read as documented Fabric operations -- exactly the claim this gate exists
    to refuse.
    """
    names = u.alias_collections()
    shapes = u.documented_shapes()
    assert u._alias_of("POST", "/v1/workspaces/{wid}/lineage", names, shapes) is None
    # …while the real collection in the same position still resolves, so the
    # assertion above is about the RULE and not about a broken substitution.
    assert u._alias_of("GET", "/v1/workspaces/{wid}/notebooks/{iid}", names, shapes)
    assert u.classify()["POST /v1/workspaces/{wid}/lineage"] == "undocumented"


def test_a_declared_native_pattern_suppresses(tree):
    assert u.classify()["GET /v1/workspaces/{wid}/items/{iid}/_emulator/access"] == "native"


def test_every_route_lands_in_exactly_one_state(tree):
    states = u.classify()
    assert set(states.values()) <= {"documented", "alias", "native", "undocumented"}
    assert len(states) == len(cov.registered())


# --- stale declarations ---------------------------------------------------------

def test_a_stale_native_declaration_is_reported(tree, monkeypatch, capsys):
    """A declaration that outlives its route is a claim about a gone tree."""
    native(monkeypatch, {r"/_emulator/access": "real",
                         r"/v1/retired/surface": "nothing registers this"})
    u.write_baseline(u.classify())
    capsys.readouterr()
    assert u.main_for_test(strict=True) == 1
    assert "/v1/retired/surface" in capsys.readouterr().out


def test_the_real_tree_has_no_stale_native_declaration():
    assert u.stale_native(cov.registered()) == []


# --- the ratchet, both directions -----------------------------------------------

def write(baseline, routes):
    baseline.write_text(json.dumps({
        "registered": 0, "documented": 0, "alias": 0, "native": 0,
        "undocumented": len(routes), "notes": {},
        "undocumentedRoutes": sorted(routes),
    }), encoding="utf-8")


def test_a_matching_baseline_passes(tree, capsys):
    write(u.BASELINE, {"POST /v1/workspaces/{wid}/lineage"})
    assert u.main_for_test(strict=True) == 0
    assert "undocumented route(s), as recorded" in capsys.readouterr().out


def test_a_new_undocumented_route_fails(tree, capsys):
    """A surface arriving with no spec behind it and no decision recorded."""
    write(u.BASELINE, set())
    assert u.main_for_test(strict=True) == 1
    out = capsys.readouterr().out
    assert "POST /v1/workspaces/{wid}/lineage" in out
    assert "not in the baseline" in out


def test_a_route_that_left_the_set_fails(tree, capsys):
    """The direction people soften, because it fails on an improvement.

    The baseline records what this emulator answers WITHOUT a spec behind it.
    Leaving a route in it after it stopped being that leaves a lie in the file
    that somebody will later read as a real divergence.
    """
    write(u.BASELINE, {"POST /v1/workspaces/{wid}/lineage", "GET /v1/gone"})
    assert u.main_for_test(strict=True) == 1
    out = capsys.readouterr().out
    assert "GET /v1/gone" in out
    assert "still calls this undocumented" in out


def test_without_strict_it_reports_but_exits_zero(tree):
    write(u.BASELINE, set())
    assert u.main_for_test(strict=False) == 0


def test_update_round_trips_to_a_clean_strict(tree, capsys):
    assert u.main_for_test(update=True) == 0
    written = json.loads(u.BASELINE.read_text())
    assert written["undocumentedRoutes"] == ["POST /v1/workspaces/{wid}/lineage"]
    assert (written["documented"] + written["alias"] + written["native"]
            + written["undocumented"]) == written["registered"]
    capsys.readouterr()
    assert u.main_for_test(strict=True) == 0


def test_a_missing_baseline_fails_rather_than_inventing_one(tree, capsys):
    assert u.main_for_test(strict=True) == 1
    assert "no baseline" in capsys.readouterr().err


def test_an_unparseable_registration_stops_the_gate(tmp_path, monkeypatch, capsys):
    """Going blind must be LOUD here, and it is a stronger reason than next door.

    The coverage ratchet's denominator shrinking makes it DEMAND LESS. This
    one's shrinking makes it REPORT LESS -- a registration form the parser
    stops understanding is an invented surface silently leaving the set.
    """
    src = tmp_path / "api"
    src.mkdir()
    (src / "odd.go").write_text(
        'package api\n\nfunc r(mux *http.ServeMux) {\n'
        '\tmux.HandleFunc(buildRoute("GET", "/v1/surprise"), a.surprise)\n}\n')
    monkeypatch.setattr(cov, "SOURCES", (src,))
    monkeypatch.setattr(cov, "ROOT", tmp_path)
    monkeypatch.setattr(cov, "UNPARSED_OK", {})
    monkeypatch.setattr(cov, "PARAMETERISED", {})
    monkeypatch.setattr(u, "BASELINE", tmp_path / "undocumented-routes.json")
    assert u.main_for_test(strict=True) == 1
    assert "could not be parsed" in capsys.readouterr().err


# --- the committed baseline against the committed tree --------------------------

def test_the_real_baseline_matches_the_real_tree():
    """Not a fixture: if these drift, every test above still passes."""
    assert u.BASELINE.is_file(), f"{u.BASELINE} is missing"
    base = json.loads(u.BASELINE.read_text())
    assert set(base["undocumentedRoutes"]) == {
        k for k, v in u.classify().items() if v == "undocumented"}


def test_every_route_in_the_baseline_carries_a_note():
    """A bare string in that file makes 'we decided this' and 'nobody looked'
    identical, which is the failure mode this whole directory is written
    against."""
    base = json.loads(u.BASELINE.read_text())
    assert sorted(base["notes"]) == sorted(base["undocumentedRoutes"])
    assert all(len(note) > 60 for note in base["notes"].values())
