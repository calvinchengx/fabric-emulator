"""notebookutils.variableLibrary, driven with the HTTP layer stubbed.

The notebook half of Fabric's environment abstraction. Unlike the pipeline
half (docs/48), Microsoft publishes this API, so these tests follow the
reference rather than a capture — including the two rules easiest to soften by
accident: the `/**/` prefix is REQUIRED, and names are CASE-SENSITIVE even
though the pipeline surface treats the library name as case-insensitive.

The definition payloads below are the shapes a real tenant returns
(`{name, note, type, value}`, a value set overriding a SUBSET, and an active
set name that has no file).
"""
import base64
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from notebookutils import variableLibrary as vl  # noqa: E402

WS = "11111111-1111-1111-1111-111111111111"
LIB = "33333333-3333-3333-3333-333333333333"


def b64(obj):
    return base64.b64encode(json.dumps(obj).encode()).decode()


VARIABLES = {
    "variables": [
        {"name": "bronzePath", "note": "env-invariant relative path", "type": "String",
         "value": "Files/bronze"},
        {"name": "batchSize", "note": "", "type": "Integer", "value": 100},
        {"name": "debugEnabled", "note": "", "type": "Boolean", "value": False},
    ]
}
QAT = {
    "name": "qat",
    "variableOverrides": [{"name": "bronzePath", "value": "Files/bronze-qat"}],
}
PARTS = [
    {"path": "variables.json", "payload": b64(VARIABLES), "payloadType": "InlineBase64"},
    {"path": "settings.json", "payload": b64({"valueSetsOrder": ["qat"]}),
     "payloadType": "InlineBase64"},
    {"path": "valueSets/qat.json", "payload": b64(QAT), "payloadType": "InlineBase64"},
    {"path": ".platform", "payload": b64({"metadata": {"type": "VariableLibrary"}}),
     "payloadType": "InlineBase64"},
]


class Http:
    """Replays the three calls the module makes: list, getDefinition, get item."""

    def __init__(self, *, active="Default value set", lro=False, libraries=("envLib",)):
        self.active = active
        self.lro = lro
        self.libraries = libraries
        self.calls = []
        self.polls = 0

    def __call__(self, method, url, *, token=None, body=None, headers=None, raw=False):
        self.calls.append((method, url))
        if url.endswith("/items?type=VariableLibrary"):
            return {"value": [{"id": LIB, "displayName": n, "type": "VariableLibrary"}
                              for n in self.libraries]}
        if url.endswith("/getDefinition"):
            if self.lro:
                return (202, {"x-ms-operation-id": "op-1",
                              "Location": "https://localhost:9443/v1/operations/op-1"}, b"")
            payload = json.dumps({"definition": {"parts": PARTS}}).encode()
            return (200, {}, payload) if raw else {"definition": {"parts": PARTS}}
        if url.endswith("/operations/op-1"):
            self.polls += 1
            return {"status": "Succeeded" if self.polls > 1 else "Running"}
        if url.endswith("/operations/op-1/result"):
            return {"definition": {"parts": PARTS}}
        if f"/items/{LIB}" in url:
            return {"id": LIB, "displayName": "envLib",
                    "properties": {"activeValueSetName": self.active}}
        return {}


@pytest.fixture
def http(monkeypatch):
    def install(**kw):
        rec = Http(**kw)
        monkeypatch.setattr(vl, "request", rec)
        monkeypatch.setattr(vl.credentials, "getToken", lambda audience: f"tok-{audience}")
        cfg = type("C", (), {"fabric_url": "https://localhost:9443", "workspace_id": WS})()
        monkeypatch.setattr(vl, "config", lambda: cfg)
        return rec
    return install


def test_get_library_resolves_defaults(http):
    http()
    lib = vl.getLibrary("envLib")
    # The three documented access forms.
    assert lib.bronzePath == "Files/bronze"
    assert lib.getVariable("batchSize") == 100
    assert lib["debugEnabled"] is False
    # Typed by the definition, not stringified.
    assert isinstance(lib.batchSize, int)
    assert isinstance(lib.debugEnabled, bool)


def test_active_value_set_overrides_a_subset(http):
    http(active="qat")
    lib = vl.getLibrary("envLib")
    assert lib.bronzePath == "Files/bronze-qat"
    # Not mentioned by the qat set, so the defaults survive. This is the whole
    # reason a value set is not a second copy of the variable list.
    assert lib.batchSize == 100
    assert lib.debugEnabled is False


def test_out_of_the_box_active_set_has_no_file(http):
    """Fabric's default active set is named "Default value set" and has NO file
    under valueSets/. Treating an unmatched active set as an error would fail
    every library in its default configuration."""
    http(active="Default value set")
    assert vl.getLibrary("envLib").bronzePath == "Files/bronze"


def test_get_by_reference(http):
    http()
    assert vl.get("$(/**/envLib/bronzePath)") == "Files/bronze"
    assert vl.get("  $(/**/envLib/batchSize)  ") == 100


@pytest.mark.parametrize("ref", [
    "$(envLib/bronzePath)",        # missing the required /**/ prefix
    "$(/**/envLib)",               # no variable
    "$(/**/envLib/bronzePath/x)",  # too many segments
    "$(/**//bronzePath)",          # empty library
    "envLib/bronzePath",           # not a reference at all
    "",
    None,
])
def test_malformed_references_are_refused(http, ref):
    """The reference documents `$(/**/lib/var)` and calls the prefix required.
    Accepting a shorter form would let a notebook work here and fail in Fabric."""
    http()
    with pytest.raises(vl.VariableLibraryError):
        vl.get(ref)


def test_names_are_case_sensitive(http):
    """Documented: "Variable and library names are case-sensitive." Note this
    DIFFERS from the pipeline surface, where Fabric documents the library name
    as not case sensitive — so the asymmetry is real and is preserved."""
    http()
    with pytest.raises(vl.VariableLibraryError) as e:
        vl.getLibrary("ENVLIB")
    assert "case-sensitive" in str(e.value)

    lib = vl.getLibrary("envLib")
    with pytest.raises(vl.VariableLibraryError):
        lib.getVariable("BRONZEPATH")


def test_missing_library_names_what_is_available(http):
    http(libraries=("other",))
    with pytest.raises(vl.VariableLibraryError) as e:
        vl.getLibrary("envLib")
    assert "other" in str(e.value)


def test_getdefinition_202_is_followed(http):
    """getDefinition documents 200 AND 202, and a real tenant answers 202. A
    client that reads the 202 body gets `null` and reports an EMPTY library
    rather than an error — so the operation is followed to its result."""
    rec = http(lro=True)
    lib = vl.getLibrary("envLib")
    assert lib.bronzePath == "Files/bronze"
    assert rec.polls >= 2, "the operation should have been polled until Succeeded"
    assert any(u.endswith("/operations/op-1/result") for _, u in rec.calls)


def test_library_helpers(http):
    http()
    lib = vl.getLibrary("envLib")
    assert "bronzePath" in lib
    assert sorted(lib) == ["batchSize", "bronzePath", "debugEnabled"]
    assert lib.asDict()["batchSize"] == 100
    assert "envLib" in repr(lib)
    # A private attribute must not be routed through variable lookup — it has
    # to raise AttributeError, not VariableLibraryError, or copy/pickle and
    # every hasattr() probe would misbehave.
    with pytest.raises(AttributeError):
        _ = lib._nope


def test_no_workspace_is_a_clear_error(monkeypatch):
    monkeypatch.setattr(vl, "config", lambda: type("C", (), {
        "fabric_url": "https://localhost:9443", "workspace_id": ""})())
    with pytest.raises(vl.VariableLibraryError) as e:
        vl.getLibrary("envLib")
    assert "workspace" in str(e.value)


class BadHttp(Http):
    """Http with the operation path broken in a specific way."""

    def __init__(self, mode):
        super().__init__(lro=True)
        self.mode = mode

    def __call__(self, method, url, *, token=None, body=None, headers=None, raw=False):
        if url.endswith("/getDefinition") and self.mode == "no-operation":
            return (202, {}, b"")
        if url.endswith("/operations/op-1") and self.mode == "failed":
            return {"status": "Failed", "error": {"errorCode": "OperationFailed"}}
        if url.endswith("/items?type=VariableLibrary") and self.mode == "no-variables":
            return {"value": [{"id": LIB, "displayName": "envLib"}]}
        if url.endswith("/getDefinition") and self.mode == "no-variables":
            return (200, {}, json.dumps({"definition": {"parts": [
                {"path": "settings.json", "payload": b64({"valueSetsOrder": []})},
                {"path": "junk.json", "payload": "!!!not-base64!!!"},
            ]}}).encode())
        return super().__call__(method, url, token=token, body=body, headers=headers, raw=raw)


@pytest.mark.parametrize("mode,fragment", [
    ("no-operation", "no operation"),
    ("failed", "failed"),
    ("no-variables", "variables.json"),
])
def test_definition_failure_paths_are_named(monkeypatch, mode, fragment):
    """Each failure says which one it was. A library that silently resolves to
    nothing is the outcome this whole surface exists to prevent."""
    rec = BadHttp(mode)
    monkeypatch.setattr(vl, "request", rec)
    monkeypatch.setattr(vl.credentials, "getToken", lambda audience: "tok")
    cfg = type("C", (), {"fabric_url": "https://localhost:9443", "workspace_id": WS})()
    monkeypatch.setattr(vl, "config", lambda: cfg)
    with pytest.raises(vl.VariableLibraryError) as e:
        vl.getLibrary("envLib")
    assert fragment in str(e.value)


def test_operation_that_never_completes_times_out(monkeypatch):
    calls = {"n": 0}

    def never(method, url, *, token=None, body=None, headers=None, raw=False):
        if url.endswith("/items?type=VariableLibrary"):
            return {"value": [{"id": LIB, "displayName": "envLib"}]}
        if url.endswith("/getDefinition"):
            return (202, {"Location": "https://localhost:9443/v1/operations/op-1"}, b"")
        calls["n"] += 1
        return {"status": "Running"}

    monkeypatch.setattr(vl, "request", never)
    monkeypatch.setattr(vl.credentials, "getToken", lambda audience: "tok")
    monkeypatch.setattr(vl, "config", lambda: type("C", (), {
        "fabric_url": "https://localhost:9443", "workspace_id": WS})())
    # Collapse the deadline so the test does not wait two minutes. The clock
    # lives in `_lro` now: the 200-or-202 loop was written here, in
    # `notebook`, and was about to be written a third time in `lakehouse`, so
    # it moved to one place. This module still OWNS the error type and the
    # token — those differ per module; the protocol does not.
    monkeypatch.setattr(vl._lro.time, "monotonic", lambda: calls["n"] * 1000.0)
    monkeypatch.setattr(vl._lro.time, "sleep", lambda s: None)
    with pytest.raises(vl.VariableLibraryError) as e:
        vl.getLibrary("envLib")
    assert "did not complete" in str(e.value)


# --- the user-data-function shape --------------------------------------------
#
# UDF is the ONLY remaining variable-library consumer with a published API, so
# unlike the pipeline capture these assertions follow Microsoft's own sample:
# `varLib.getVariables()` then `variables.get("ENV")` / `variables["ENV"]`.


def test_udf_variables_client_reads_both_documented_ways(http):
    http()
    client = vl.FabricVariablesClient("envLib")
    variables = client.getVariables()
    assert variables["bronzePath"] == "Files/bronze"
    assert variables.get("batchSize") == 100
    assert "envLib" in repr(client)


def test_udf_get_returns_default_rather_than_raising(http):
    """The documented sample calls `variables.get("ENV")` and BRANCHES on the
    value, so an unknown name must come back None rather than raise. Bracket
    access still raises, as a mapping should."""
    http()
    variables = vl.FabricVariablesClient("envLib").getVariables()
    assert variables.get("nope") is None
    assert variables.get("nope", "fallback") == "fallback"
    with pytest.raises(vl.VariableLibraryError):
        _ = variables["nope"]


def test_udf_variables_resolve_under_the_active_value_set(http):
    """The point of the whole surface: the same function body yields a
    different value per environment, with no code change."""
    http(active="qat")
    variables = vl.FabricVariablesClient("envLib").getVariables()
    assert variables["bronzePath"] == "Files/bronze-qat"
    assert variables["batchSize"] == 100  # not overridden by qat


def test_udf_variables_mapping_helpers(http):
    http()
    variables = vl.FabricVariablesClient("envLib").getVariables()
    assert "bronzePath" in variables
    assert sorted(variables) == ["batchSize", "bronzePath", "debugEnabled"]
    assert variables.asDict()["batchSize"] == 100
    assert "bronzePath" in repr(variables)


def test_udf_client_does_not_shadow_microsofts_package():
    """`fabric-user-data-functions` is a real installable package, so providing
    a module named `fabric.functions` would override something a user may have
    installed. notebookutils is different — Microsoft ships that as an
    import-only stub outside the Fabric runtime."""
    import importlib.util
    assert importlib.util.find_spec("notebookutils.variableLibrary") is not None
    # Nothing in this repo provides `fabric.functions`.
    import pathlib
    root = pathlib.Path(vl.__file__).resolve().parents[1]
    assert not (root / "fabric" / "functions.py").exists()
    assert not (root / "fabric" / "functions").exists()
