r"""The conformance checker, held to the standard it holds responses to.

check_openapi_conformance reads Microsoft's swagger and decides whether the
emulator's answers agree with it. Two halves can fail silently and both would
report a clean tree:

  * MATCHING. If no recorded path matches a spec route, everything lands in
    "no documented route", which the checker deliberately does NOT count as a
    finding -- so a broken matcher reads exactly like a conformant emulator.
    Two rules make matching work, and both were wrong first: a $ref's origin
    travels across files, and the spec writes query variants as separate path
    keys.
  * VALIDATION. If deref returns {} or the array branch never recurses, every
    response validates against nothing at all.

So the tests below plant disagreements and assert they are FOUND, and assert
the two matching rules directly. The fixtures are miniature swagger documents
rather than the vendored tree, because a test that depends on 55 real specs
fails for reasons that have nothing to do with the checker.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_openapi_conformance as c  # noqa: E402

SWAGGER = {
    "swagger": "2.0",
    "basePath": "/v1",
    "paths": {
        "/widgets": {
            "get": {"responses": {"200": {"schema": {"$ref": "definitions.json#/definitions/WidgetList"}}}},
        },
        # The query-variant spelling the real specs use for admin routes.
        "/gadgets?preview=true": {
            "get": {"responses": {"200": {"schema": {"type": "object"}}}},
        },
        "/sprockets": {
            "get": {"responses": {"200": {"schema": {"type": "object"}}, "429": {}}},
        },
    },
}

DEFINITIONS = {
    "definitions": {
        "WidgetList": {
            "type": "object",
            "properties": {"value": {"type": "array", "items": {"$ref": "common/shared.json#/definitions/Widget"}}},
        },
    },
}

# In its own directory, so a ref that resolves relative to the WRONG file
# cannot accidentally find it.
SHARED = {
    "definitions": {
        "Widget": {
            "type": "object",
            "required": ["id", "displayName"],
            "properties": {
                "id": {"type": "string"},
                "displayName": {"type": "string"},
                "kind": {"type": "string", "enum": ["A", "B"]},
            },
        },
    },
}


@pytest.fixture
def specs(tmp_path):
    (tmp_path / "surface").mkdir()
    (tmp_path / "surface" / "swagger.json").write_text(json.dumps(SWAGGER))
    (tmp_path / "surface" / "definitions.json").write_text(json.dumps(DEFINITIONS))
    (tmp_path / "surface" / "common").mkdir()
    (tmp_path / "surface" / "common" / "shared.json").write_text(json.dumps(SHARED))
    return c.Specs((tmp_path,))


def check(specs, entries):
    found, matched, unmatched = c.conformance(entries, specs)
    return found, matched, unmatched


def widget(**overrides):
    body = {"id": "w1", "displayName": "Widget One", "kind": "A"}
    body.update(overrides)
    return {"method": "GET", "path": "/v1/widgets", "status": 200,
            "body": {"value": [body]}}


# --- matching ------------------------------------------------------------------

def test_a_documented_route_matches(specs):
    _, matched, unmatched = check(specs, [widget()])
    assert matched == 1 and not unmatched


def test_a_query_variant_path_key_still_matches_the_plain_route(specs):
    """The rule that was wrong first.

    The spec writes `/v1/admin/domains?preview=true`; a caller requests
    `/v1/admin/domains`. Matching the key literally reports a served,
    documented route as undocumented.
    """
    _, matched, unmatched = check(
        specs, [{"method": "GET", "path": "/v1/gadgets", "status": 200, "body": {}}])
    assert matched == 1, f"the query-variant key did not match: {unmatched}"


def test_an_undocumented_path_is_reported_but_is_not_a_finding(specs):
    """Reported so it is visible, uncounted so it cannot drown real findings."""
    found, matched, unmatched = check(
        specs, [{"method": "GET", "path": "/v1/nope", "status": 200, "body": {}}])
    assert matched == 0
    assert unmatched == {"GET /v1/nope"}
    assert found == []


def test_the_vendored_specs_load_and_are_not_empty():
    """The matcher against the tree it actually guards.

    Zero routes is the silent failure: every response would fall into "no
    documented route", which is not a finding, and the checker would pass
    forever while validating nothing.
    """
    for root in c.SPEC_ROOTS:
        assert root.is_dir(), f"{root} is missing"
    # Both trees: Fabric's alone is ~729, and Power BI's adds the /v1.0/myorg
    # surface. A regression to one tree would halve the routes and quietly
    # send every /v1.0 response to "no documented route", which is not a
    # finding and therefore not checked.
    assert len(c.Specs().routes) > 900


# --- validation ----------------------------------------------------------------

def test_a_conformant_response_has_no_findings(specs):
    found, _, _ = check(specs, [widget()])
    assert found == []


def test_a_missing_required_property_inside_an_array_is_found(specs):
    """Most of the surface is arrays, so this is the load-bearing case."""
    body = widget()
    del body["body"]["value"][0]["id"]
    found, _, _ = check(specs, [body])
    assert found == ["GET /v1/widgets.value[0]: MISSING required property 'id'"]


def test_a_wrong_primitive_type_is_found(specs):
    found, _, _ = check(specs, [widget(displayName=1234)])
    assert found == ["GET /v1/widgets.value[0].displayName: type is int, spec says string"]


def test_a_value_outside_the_spec_enum_is_found(specs):
    found, _, _ = check(specs, [widget(kind="Z")])
    assert len(found) == 1 and "not in the spec's enum" in found[0]


def test_a_ref_across_two_files_resolves(specs):
    """WidgetList lives in definitions.json and its items in common/shared.json.

    If the origin did not travel with the ref, the second hop would resolve
    against the swagger's directory, find nothing, and validate the entry
    against {} -- which finds no defects and reports a clean tree.
    """
    body = widget()
    del body["body"]["value"][0]["displayName"]
    found, _, _ = check(specs, [body])
    assert found, "a cross-file ref resolved to nothing, so nothing was checked"


def test_an_undocumented_status_is_found(specs):
    found, _, _ = check(
        specs, [{"method": "GET", "path": "/v1/sprockets", "status": 418, "body": {}}])
    assert len(found) == 1 and "answered 418" in found[0]


def test_a_documented_status_with_no_schema_is_not_a_finding(specs):
    found, _, _ = check(
        specs, [{"method": "GET", "path": "/v1/sprockets", "status": 429, "body": {}}])
    assert found == []


def test_a_bool_is_not_reported_as_a_bad_integer(specs):
    """Python's bool is an int; reporting it would be the checker's own type
    system leaking into its findings."""
    schema = {"type": "integer"}
    out = []
    c.validate(True, schema, specs, pathlib.Path("x.json"), "where", out)
    assert out == []


# --- the recording, and the pinned set -----------------------------------------

def test_a_truncated_final_line_is_skipped_rather_than_fatal(tmp_path):
    """A suite killed mid-run leaves a partial line. Refusing to read the
    thousands of complete ones before it turns a timeout into a second,
    unrelated failure."""
    rec = tmp_path / "r.jsonl"
    rec.write_text('{"method":"GET","path":"/v1/a","status":200}\n{"method":"GE')
    assert len(c.read_recording(rec)) == 1


def test_blank_lines_are_ignored(tmp_path):
    rec = tmp_path / "r.jsonl"
    rec.write_text('\n{"method":"GET","path":"/v1/a","status":200}\n\n')
    assert len(c.read_recording(rec)) == 1


def test_every_pinned_entry_carries_a_reason():
    """An entry without a written reason is a silencer rather than a record."""
    for pin, reason in c.KNOWN.items():
        assert reason.strip(), pin


def test_is_known_matches_by_substring():
    pin = next(iter(c.KNOWN))
    assert c.is_known(f"GET /v1/x: {pin} trailing")
    assert not c.is_known("GET /v1/x: something nobody pinned")


def test_an_empty_recording_fails_rather_than_passes(tmp_path, monkeypatch, capsys):
    """A suite that recorded nothing proves nothing.

    This is the difference between a check that ran and a check that could not,
    and it is the one a green would otherwise hide.
    """
    rec = tmp_path / "empty.jsonl"
    rec.write_text("")
    monkeypatch.setattr(sys, "argv", ["check_openapi_conformance.py", str(rec)])
    assert c.main() == 1
    assert "proves nothing" in capsys.readouterr().err


def test_a_missing_recording_fails_and_says_how_to_produce_one(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(sys, "argv", ["check_openapi_conformance.py", str(tmp_path / "gone.jsonl")])
    assert c.main() == 1
    assert "FABRIC_RECORD_RESPONSES" in capsys.readouterr().err


# --- main(), the reporting surface ---------------------------------------------

def recording_of(tmp_path, entries):
    rec = tmp_path / "rec.jsonl"
    rec.write_text("\n".join(json.dumps(e) for e in entries) + "\n")
    return rec


@pytest.fixture
def tiny_specs(specs, monkeypatch):
    """Point main() at the fixture specs rather than the vendored tree."""
    monkeypatch.setattr(c, "Specs", lambda *a, **k: specs)
    return specs


def test_main_reports_a_clean_recording_and_exits_zero(tmp_path, tiny_specs, monkeypatch, capsys):
    rec = recording_of(tmp_path, [widget()])
    monkeypatch.setattr(sys, "argv", ["check_openapi_conformance.py", str(rec)])
    assert c.main() == 0
    out = capsys.readouterr().out
    assert "no conformance findings" in out
    assert "1 matched" in out or "matched" in out


def test_main_counts_distinct_findings_not_responses(tmp_path, tiny_specs, monkeypatch, capsys):
    """The reporting defect this checker produced on its own first run.

    A suite calls the same route many times, so two real defects printed 102
    lines. A reader who scrolls past 102 identical lines stops reading the
    output, which is the checker crying wolf at itself.
    """
    broken = widget(displayName=7)
    rec = recording_of(tmp_path, [broken] * 20)
    monkeypatch.setattr(sys, "argv", ["check_openapi_conformance.py", str(rec), "--strict"])
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "1 distinct finding" in out
    assert "(20 responses)" in out, out


def test_main_without_strict_reports_but_exits_zero(tmp_path, tiny_specs, monkeypatch, capsys):
    rec = recording_of(tmp_path, [widget(displayName=7)])
    monkeypatch.setattr(sys, "argv", ["check_openapi_conformance.py", str(rec)])
    assert c.main() == 0
    assert "distinct finding" in capsys.readouterr().out


def test_main_prints_undocumented_paths_without_failing(tmp_path, tiny_specs, monkeypatch, capsys):
    rec = recording_of(tmp_path, [{"method": "GET", "path": "/v1/nope", "status": 200, "body": {}}])
    monkeypatch.setattr(sys, "argv", ["check_openapi_conformance.py", str(rec), "--strict"])
    assert c.main() == 0
    out = capsys.readouterr().out
    assert "no documented route" in out and "GET /v1/nope" in out


def test_main_says_how_many_known_disagreements_were_pinned(tmp_path, tiny_specs, monkeypatch, capsys):
    """Pinned entries are counted out loud, so the list cannot grow unnoticed."""
    pin = next(iter(c.KNOWN))
    monkeypatch.setattr(c, "conformance",
                        lambda entries, specs: ([f"GET /v1/x: {pin}"], 1, set()))
    rec = recording_of(tmp_path, [widget()])
    monkeypatch.setattr(sys, "argv", ["check_openapi_conformance.py", str(rec), "--strict"])
    assert c.main() == 0
    assert "1 known disagreement" in capsys.readouterr().out


# --- pins must stay live -------------------------------------------------------

def test_a_pin_that_matches_nothing_is_reported_stale():
    """A pin goes stale exactly when somebody fixes the thing."""
    assert c.stale_pins([]) == sorted(c.KNOWN)
    live = next(iter(c.KNOWN))
    assert live not in c.stale_pins([f"GET /v1/x: {live}"])


def test_a_stale_pin_is_not_fatal(tmp_path, tiny_specs, monkeypatch, capsys):
    """Reported, never fatal, and the distinction was wrong in the first draft.

    Staleness is a claim about THIS run: a pin is silent when its route was
    not exercised, which happens whenever a suite fails and uploads no
    recording. Failing on it would turn an unrelated failure into a second,
    more confusing one.
    """
    rec = recording_of(tmp_path, [widget()])
    monkeypatch.setattr(sys, "argv", ["check_openapi_conformance.py", str(rec), "--strict"])
    assert c.main() == 0
    out = capsys.readouterr().out
    assert "did not occur in this run" in out
    assert "Not a failure" in out


def test_the_committed_pins_are_all_still_documented():
    """Every pin carries a written reason; a bare entry is a silencer."""
    for pin, reason in c.KNOWN.items():
        assert reason.strip(), pin


def test_an_auth_status_is_not_reported_as_undocumented(specs):
    """401 and 403 are the same answer on every route.

    Specs enumerate them globally rather than per operation, so flagging them
    would fire on any suite that exercises an auth failure — which suites
    should do. Measured: medallion drives executeQueries without a token, the
    emulator correctly answers 401, and that read as a disagreement.
    """
    for status in (401, 403):
        found, _, _ = check(specs, [{"method": "GET", "path": "/v1/sprockets",
                                     "status": status, "body": {}}])
        assert found == [], f"{status} should not be a finding"


def test_a_404_is_still_reported(specs):
    """The `not implemented` signal stays visible: it is how an unserved route
    announces itself, and the Power BI pins in KNOWN are exactly that."""
    found, _, _ = check(specs, [{"method": "GET", "path": "/v1/sprockets",
                                 "status": 404, "body": {}}])
    assert len(found) == 1 and "answered 404" in found[0]
