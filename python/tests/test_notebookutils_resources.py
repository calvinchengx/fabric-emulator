"""Notebook resources: the `builtin/` folder that travels with a notebook.

THE ROOT NOTEBOOK'S FOLDER, NOT THE CURRENT ONE, is the subtlety worth testing.
Microsoft's guidance is that `builtin/` "will always point to the root
notebook's built-in folder", and that a referenced notebook should use
`nbResPath` so it "points to the same folder as the interactive run".

Resolving to the child's own folder would give a notebook different files
depending on how it was started — interactively vs. as a reference run — which
is the kind of divergence nobody debugs quickly.
"""
import base64
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import notebookutils  # noqa: E402
from notebookutils import nbresources  # noqa: E402

ROOT_NB = "aaaaaaaa-1111-2222-3333-444444444444"
CHILD_NB = "bbbbbbbb-1111-2222-3333-444444444444"
WS = "cccccccc-1111-2222-3333-444444444444"


def _part(path, text):
    return {"path": path, "payload": base64.b64encode(text.encode()).decode()}


@pytest.fixture
def stub(monkeypatch, tmp_path):
    """A definition response, and a temp home for what gets materialised."""
    calls = []

    def request(method, url, token=None, body=None, headers=None, raw=False):
        calls.append(url)
        return {"definition": {"parts": [
            _part("notebook-content.py", "print('hi')"),
            _part("builtin/data.csv", "id,name\n1,a\n"),
            _part("builtin/nested/deep.txt", "deep"),
            _part(".platform", "{}"),
        ]}}

    monkeypatch.setattr(nbresources, "request", request)
    monkeypatch.setattr(nbresources.credentials, "getToken", lambda a: "tok")
    monkeypatch.setattr(nbresources, "config",
                        lambda: type("C", (), {"fabric_url": "https://f",
                                               "workspace_id": WS})())
    monkeypatch.setattr(nbresources.tempfile, "gettempdir", lambda: str(tmp_path))
    nbresources.forget()
    yield calls
    nbresources.forget()


def _context(monkeypatch, **kw):
    monkeypatch.setattr(nbresources.runtime, "context", kw)


def test_resources_are_materialised_for_ordinary_file_io(stub, monkeypatch):
    """The docs describe using these "as if you're working with your local file
    system", so the proof is a plain open()."""
    _context(monkeypatch, currentNotebookId=ROOT_NB, currentWorkspaceId=WS)
    path = notebookutils.nbResPath
    with open(path + "/data.csv") as fh:
        assert fh.read() == "id,name\n1,a\n"


def test_nested_resources_keep_their_shape(stub, monkeypatch):
    _context(monkeypatch, currentNotebookId=ROOT_NB, currentWorkspaceId=WS)
    with open(notebookutils.nbResPath + "/nested/deep.txt") as fh:
        assert fh.read() == "deep"


def test_only_builtin_parts_are_materialised(stub, monkeypatch):
    """`notebook-content.py` and `.platform` are the notebook itself, not its
    resources. Writing them into builtin/ would put the notebook's own source
    where its data belongs."""
    _context(monkeypatch, currentNotebookId=ROOT_NB, currentWorkspaceId=WS)
    root = pathlib.Path(notebookutils.nbResPath)
    names = sorted(p.name for p in root.rglob("*") if p.is_file())
    assert names == ["data.csv", "deep.txt"]


def test_a_reference_run_reads_the_ROOT_notebooks_resources(stub, monkeypatch):
    """THE SUBTLETY. In a reference run `builtin/` is the parent's folder — a
    child resolving to its own would see different files depending on how it
    was started."""
    _context(monkeypatch, currentNotebookId=CHILD_NB, rootNotebookId=ROOT_NB,
             currentWorkspaceId=WS, rootWorkspaceId=WS)
    path = notebookutils.nbResPath
    assert ROOT_NB in path and CHILD_NB not in path
    assert any(f"/notebooks/{ROOT_NB}/getDefinition" in u for u in stub), stub


def test_an_interactive_run_uses_the_current_notebook(stub, monkeypatch):
    _context(monkeypatch, currentNotebookId=CHILD_NB, currentWorkspaceId=WS)
    assert CHILD_NB in notebookutils.nbResPath


def test_the_definition_is_fetched_once_and_cached(stub, monkeypatch):
    """`nbResPath` is read inside loops in real notebooks; re-fetching per read
    would turn a path lookup into an HTTP call."""
    _context(monkeypatch, currentNotebookId=ROOT_NB, currentWorkspaceId=WS)
    seen = {notebookutils.nbResPath for _ in range(3)}
    assert len(seen) == 1, "the same path every time"
    assert len(stub) == 1, stub


def test_no_notebook_identity_says_so_rather_than_returning_a_path(stub, monkeypatch):
    """A path that pointed nowhere would be worse: the caller would write files
    into it and never find them again."""
    _context(monkeypatch, currentWorkspaceId=WS)
    with pytest.raises(RuntimeError, match="only meaningful inside a notebook run"):
        _ = notebookutils.nbResPath


def test_an_unknown_attribute_still_raises_AttributeError():
    """The module __getattr__ must not swallow real typos."""
    with pytest.raises(AttributeError, match="no attribute 'nbResPathh'"):
        _ = notebookutils.nbResPathh
