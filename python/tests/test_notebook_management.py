"""The notebook management surface — create/get/update/delete/list/definition.

WHY THIS FILE EXISTS. Framework-conformance contract 2 (docs/38 §2) found seven
DOCUMENTED `notebookutils.notebook` methods absent from the shim. A framework
introspects a signature and declines to run when one is missing, without ever
calling it, so the absence alone stopped CI/CD-shaped code from starting.

WHAT IS UNDER TEST is the shim's own logic: the URL each method builds, the
body it sends, the 200-vs-202 outcome it must resolve, and the errors it
refuses to guess past. Whether the emulator answers is the e2e's job.
"""
import base64
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from notebookutils import credentials, notebook  # noqa: E402

WS = "11111111-1111-1111-1111-111111111111"
NB = "33333333-3333-3333-3333-333333333333"
IPYNB = '{"cells":[],"metadata":{},"nbformat":4,"nbformat_minor":5}'


class Recorder:
    def __init__(self):
        self.calls = []
        self.answers = []

    def push(self, value):
        self.answers.append(value)
        return self

    def __call__(self, method, url, *, token=None, body=None, headers=None, raw=False):
        self.calls.append({"method": method, "url": url, "body": body, "raw": raw})
        if self.answers:
            answer = self.answers.pop(0)
            if isinstance(answer, Exception):
                raise answer
            return answer
        return {}

    def urls(self):
        return [c["url"] for c in self.calls]

    def bodies(self):
        return [c["body"] for c in self.calls if c["body"] is not None]


@pytest.fixture
def http(monkeypatch):
    rec = Recorder()
    monkeypatch.setattr(notebook, "request", rec, raising=False)
    monkeypatch.setattr(credentials, "getToken", lambda audience: f"tok-{audience}")
    cfg = type("C", (), {"fabric_url": "https://localhost:9443",
                         "workspace_id": WS, "is_real": False})()
    monkeypatch.setattr(notebook, "config", lambda: cfg, raising=False)
    return rec


def _listing(name="nb"):
    return {"value": [{"id": NB, "displayName": name, "description": "d",
                       "type": "Notebook", "workspaceId": WS}]}


# --- create -------------------------------------------------------------------

def test_create_sends_only_the_ipynb_part(http):
    """The executable `.py` is derived SERVER-SIDE, where the parser lives.

    A client-side conversion would be a second definition of one thing — the
    defect this repository keeps finding — so the shim must not send one.
    """
    http.push((201, {}, json.dumps({"id": NB, "displayName": "nb"})))
    art = notebook.create("nb", "desc", IPYNB)
    assert art.id == NB
    body = http.bodies()[0]
    paths = [p["path"] for p in body["definition"]["parts"]]
    assert paths == ["notebook-content.ipynb"]
    assert base64.b64decode(body["definition"]["parts"][0]["payload"]).decode() == IPYNB
    assert body["displayName"] == "nb" and body["description"] == "desc"


def test_create_accepts_a_dict_as_content():
    """Documented: "String, bytes, or dict"."""
    parts = notebook._notebook_parts({"cells": []})
    assert json.loads(base64.b64decode(parts[0]["payload"])) == {"cells": []}


def test_create_refuses_empty_content():
    """Documented as "Cannot be empty". Refused by name here rather than sent,
    so the error says `content` instead of arriving as a 400 about parts."""
    for empty in (None, "", b""):
        with pytest.raises(notebook.NotebookError, match="content cannot be empty"):
            notebook._notebook_parts(empty)


def test_create_refuses_json_that_is_not_a_notebook():
    """`{}` is valid JSON and carries no notebook. Sent, it would create an item
    that runs zero cells and reports success — empty in the way that matters."""
    for not_a_notebook in ({"metadata": {}}, '{"nbformat": 4}'):
        with pytest.raises(notebook.NotebookError, match="not a notebook"):
            notebook._notebook_parts(not_a_notebook)


def test_create_passes_non_json_content_through():
    """The `.py` form is what getDefinition falls back to; refusing it would
    break the round trip updateDefinition uses for a lakehouse-only change."""
    parts = notebook._notebook_parts("# Fabric notebook source\n")
    assert base64.b64decode(parts[0]["payload"]).decode() == "# Fabric notebook source\n"


def test_create_follows_the_202_operation(http):
    """A real tenant answers 202. A client that reads that body gets `null`."""
    http.push((202, {"Location": "https://localhost:9443/v1/operations/op1"}, ""))
    http.push({"status": "Succeeded"})
    http.push({"id": NB, "displayName": "nb"})
    art = notebook.create("nb", content=IPYNB)
    assert art.id == NB
    assert http.urls()[-1].endswith("/v1/operations/op1/result")


def test_create_surfaces_a_failed_operation(http):
    http.push((202, {"x-ms-operation-id": "op2"}, ""))
    http.push({"status": "Failed", "error": {"message": "nope"}})
    with pytest.raises(notebook.NotebookError, match="operation failed"):
        notebook.create("nb", content=IPYNB)


def test_a_202_with_no_operation_is_refused(http):
    """Following nothing would hang or report an empty create."""
    http.push((202, {}, ""))
    with pytest.raises(notebook.NotebookError, match="no operation to follow"):
        notebook.create("nb", content=IPYNB)


def test_create_with_a_default_lakehouse_adds_the_binding(http):
    http.push((201, {}, json.dumps({"id": NB})))
    notebook.create("nb", content=IPYNB, defaultLakehouse="lake")
    paths = [p["path"] for p in http.bodies()[0]["definition"]["parts"]]
    assert paths == ["notebook-content.ipynb", "notebook-content-metadata.json"]


# --- get / list ---------------------------------------------------------------

def test_get_returns_an_artifact_with_attributes(http):
    """Microsoft's examples read `notebook.displayName`; a dict would make every
    one of those lines an AttributeError in code that works on Fabric."""
    http.push(_listing())
    http.push({"id": NB, "displayName": "nb", "description": "d"})
    art = notebook.get("nb")
    assert (art.id, art.displayName, art.description) == (NB, "nb", "d")
    assert "Artifact(" in repr(art)


def test_get_accepts_an_id_as_well_as_a_name(http):
    """Fabric's management APIs take "name or ID" everywhere, so a caller
    holding an id from create() must not have to look the name up."""
    http.push({"value": []})       # not a display name
    http.push({"id": NB})          # ...but it is an id
    http.push({"id": NB, "displayName": "nb"})
    art = notebook.get(NB)
    assert art.id == NB


def test_get_reports_a_notebook_that_is_neither_a_name_nor_an_id(http):
    http.push({"value": []})
    http.push(RuntimeError("404"))
    with pytest.raises(notebook.NotebookError, match="not found in workspace"):
        notebook.get("ghost")


def test_list_returns_artifacts_and_honours_maxResults(http):
    http.push({"value": [{"id": "a"}, {"id": "b"}, {"id": "c"}]})
    got = notebook.list(maxResults=2)
    assert [a.id for a in got] == ["a", "b"]
    assert http.urls()[0].endswith("/items?type=Notebook")


# --- getDefinition ------------------------------------------------------------

def test_getDefinition_returns_the_ipynb_string(http):
    http.push(_listing())
    http.push((200, {}, json.dumps({"definition": {"parts": [
        {"path": "notebook-content.ipynb",
         "payload": base64.b64encode(IPYNB.encode()).decode()}]}})))
    assert notebook.getDefinition("nb") == IPYNB


def test_getDefinition_falls_back_to_the_py_part(http):
    """An item created before the ipynb part existed still has content."""
    src = "# Fabric notebook source\n"
    http.push(_listing())
    http.push((200, {}, json.dumps({"definition": {"parts": [
        {"path": "notebook-content.py",
         "payload": base64.b64encode(src.encode()).decode()}]}})))
    assert notebook.getDefinition("nb") == src


def test_getDefinition_refuses_a_format_fabric_does_not_document(http):
    """Answering ipynb for a format nobody documents would be a silent lie."""
    with pytest.raises(notebook.NotebookError, match="unsupported format"):
        notebook.getDefinition("nb", format="html")


def test_getDefinition_reports_a_definition_with_no_content_part(http):
    http.push(_listing())
    http.push((200, {}, json.dumps({"definition": {"parts": []}})))
    with pytest.raises(notebook.NotebookError, match="no notebook-content part"):
        notebook.getDefinition("nb")


# --- update / updateDefinition / delete ---------------------------------------

def test_update_patches_the_item(http):
    http.push(_listing())
    http.push({"id": NB, "displayName": "renamed"})
    art = notebook.update("nb", "renamed", description="new")
    assert art.displayName == "renamed"
    call = http.calls[-1]
    assert call["method"] == "PATCH"
    assert call["body"] == {"displayName": "renamed", "description": "new"}


def test_updateDefinition_replaces_content_and_returns_true(http):
    http.push(_listing())
    http.push((200, {}, ""))
    assert notebook.updateDefinition("nb", content=IPYNB) is True
    body = http.bodies()[-1]
    assert [p["path"] for p in body["definition"]["parts"]] == ["notebook-content.ipynb"]


def test_updateDefinition_needs_something_to_change(http):
    with pytest.raises(notebook.NotebookError, match="needs content"):
        notebook.updateDefinition("nb")


def test_updateDefinition_reads_current_content_for_a_lakehouse_only_change(http):
    """The API REPLACES the definition rather than patching it, so a
    lakehouse-only change that sent no content would erase the notebook."""
    http.push(_listing())                      # resolve for updateDefinition
    http.push(_listing())                      # resolve inside getDefinition
    http.push((200, {}, json.dumps({"definition": {"parts": [
        {"path": "notebook-content.ipynb",
         "payload": base64.b64encode(IPYNB.encode()).decode()}]}})))
    http.push((200, {}, ""))
    assert notebook.updateDefinition("nb", defaultLakehouse="lake") is True
    parts = http.bodies()[-1]["definition"]["parts"]
    assert [p["path"] for p in parts] == [
        "notebook-content.ipynb", "notebook-content-metadata.json"]
    assert base64.b64decode(parts[0]["payload"]).decode() == IPYNB


def test_updateDefinition_records_the_environment_parameters(http):
    """Fabric documents them for the Spark runtime. Accepting and ignoring is
    allowed by §2; recording them costs nothing and keeps the round trip honest."""
    http.push(_listing())
    http.push((200, {}, ""))
    notebook.updateDefinition("nb", content=IPYNB, environmentId="env-1")
    parts = http.bodies()[-1]["definition"]["parts"]
    env_part = next(p for p in parts if p["path"] == "notebook-environment.json")
    assert json.loads(base64.b64decode(env_part["payload"]))["environmentId"] == "env-1"


def test_updateDefinition_does_not_poll_for_a_result_document(http):
    """Real Fabric's updateDefinition LRO has no result; polling /result for it
    would 404 on a success."""
    http.push(_listing())
    http.push((202, {"Location": "https://localhost:9443/v1/operations/op3"}, ""))
    http.push({"status": "Succeeded"})
    assert notebook.updateDefinition("nb", content=IPYNB) is True
    assert not any(u.endswith("/result") for u in http.urls())


def test_delete_removes_the_item_and_returns_true(http):
    http.push(_listing())
    assert notebook.delete("nb") is True
    assert http.calls[-1]["method"] == "DELETE"
    assert http.calls[-1]["url"].endswith(f"/items/{NB}")


# --- workspace resolution -----------------------------------------------------

def test_a_method_with_no_workspace_anywhere_says_so(monkeypatch):
    """Better than a request to `/v1/workspaces//items`, which 404s about a path."""
    cfg = type("C", (), {"fabric_url": "https://x", "workspace_id": "", "is_real": False})()
    monkeypatch.setattr(notebook, "config", lambda: cfg, raising=False)
    with pytest.raises(notebook.NotebookError, match="no workspace"):
        notebook.list()
