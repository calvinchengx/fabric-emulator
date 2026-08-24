"""Notebook resources — the `builtin/` folder that travels with a notebook.

Fabric gives every notebook a system-defined resource folder. Files put there
are addressed with the relative path `builtin/…`, and `notebookutils.nbResPath`
composes the absolute one.

THE ROOT NOTEBOOK'S FOLDER, NOT THE CURRENT ONE — and that is the whole subtlety.
Microsoft's own guidance is that `builtin/` "will always point to the root
notebook's built-in folder", and that a referenced notebook should use
`nbResPath` so it "points to the same folder as the interactive run". So in a
reference run the resources a child sees are the PARENT's. Resolving to the
child's own would give a notebook different files depending on how it was
started, which is the kind of divergence nobody debugs quickly.

WHERE THE FILES COME FROM HERE. A notebook item's definition carries parts, and
resources are parts whose path begins with `builtin/`. They are materialised to
a local directory on first use, so ordinary Python file I/O reaches them —
which is exactly how the docs describe using them ("as if you're working with
your local file system").
"""
import base64
import os
import tempfile

from . import credentials, runtime
from ._config import config
from ._http import request

PREFIX = "builtin/"

# session key -> materialised directory. Cached because `nbResPath` is read
# inside loops in real notebooks, and re-fetching the definition per read would
# turn a path lookup into an HTTP call.
_MATERIALISED = {}


def _root_notebook_id():
    """The notebook whose resources `builtin/` means.

    `rootNotebookId` when this is a reference run, else the current notebook.
    Both come from runtime.context, which contract 1 proves answers with the
    RUNNING notebook's identity rather than an environment fallback.
    """
    ctx = runtime.context
    return (ctx.get("rootNotebookId")
            or ctx.get("currentNotebookId")
            or "")


def _materialise(notebook_id):
    cfg = config()
    ws = ctx_workspace()
    token = credentials.getToken("fabric")
    got = request("POST",
                  f"{cfg.fabric_url}/v1/workspaces/{ws}/notebooks/{notebook_id}/getDefinition",
                  token=token)
    parts = ((got.get("definition") or {}).get("parts")
             if isinstance(got, dict) else None) or []
    root = os.path.join(tempfile.gettempdir(), "nb_resource", notebook_id, "builtin")
    os.makedirs(root, exist_ok=True)
    for part in parts:
        path = part.get("path") or ""
        if not path.startswith(PREFIX):
            continue
        relative = path[len(PREFIX):]
        target = os.path.join(root, relative)
        os.makedirs(os.path.dirname(target) or root, exist_ok=True)
        with open(target, "wb") as fh:
            fh.write(base64.b64decode(part.get("payload") or ""))
    return root


def ctx_workspace():
    ctx = runtime.context
    return (ctx.get("rootWorkspaceId")
            or ctx.get("currentWorkspaceId")
            or config().workspace_id
            or "")


def nb_res_path():
    """The absolute path of the root notebook's `builtin/` folder."""
    notebook_id = _root_notebook_id()
    if not notebook_id:
        raise RuntimeError(
            "notebookutils.nbResPath needs a notebook identity and this session "
            "has none — it is only meaningful inside a notebook run")
    if notebook_id not in _MATERIALISED:
        _MATERIALISED[notebook_id] = _materialise(notebook_id)
    return _MATERIALISED[notebook_id]


def forget():
    """Drop the materialised cache. For tests, and for a session that rebinds."""
    _MATERIALISED.clear()
