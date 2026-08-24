"""notebookutils.lakehouse — lakehouse CRUD via the Fabric control plane.

Thin wrapper over /v1/workspaces/{id}/lakehouses using a fabric-audience token
minted for the notebook identity. Workspace defaults to the runtime context.

ADDRESSED BY NAME, not by id, and that is the documented contract rather than a
convenience. Fabric's page says `get(name)` takes "name of the lakehouse to
retrieve" and defaults to the current one when omitted; this shim used to take
`lakehouseId`, which is both the wrong parameter name and the wrong lookup. An
id is still accepted — `notebook.get` documents "name or ID" for the same shape
and callers pass both — but a name is what resolves first.
"""
import os
import re

from . import credentials
from ._config import config
from ._http import request

# Captured before `list` below shadows the builtin at module scope.
_list = list

_ITEM_ID = re.compile(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
                      r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}")


def _ws(workspaceId):  # noqa: N803 - documented spelling
    ws = workspaceId or config().workspace_id
    if not ws:
        raise RuntimeError("no workspace: pass workspaceId or set NOTEBOOKUTILS_WORKSPACE_ID")
    return ws


def _base(workspaceId):  # noqa: N803 - documented spelling
    # Resolve the workspace FIRST: "no workspace" is the actionable error, and
    # evaluating the base URL ahead of it lets a config problem mask it.
    ws = _ws(workspaceId)
    return f"{config().fabric_url}/v1/workspaces/{ws}/lakehouses"


def _token():
    return credentials.getToken("fabric")


def _resolve_id(name, workspaceId):  # noqa: N803 - documented spelling
    """The item id for `name`, which may already be an id.

    An empty name means the session's default lakehouse — the same "defaults to
    the current lakehouse" the page describes, read from the runtime context
    rather than guessed.
    """
    if not name:
        current = config().lakehouse_id or os.environ.get("NOTEBOOKUTILS_LAKEHOUSE_ID")
        if not current:
            raise RuntimeError(
                "no lakehouse name given and no default lakehouse is attached to "
                "this session — pass a name, or attach a default lakehouse")
        return current
    if _ITEM_ID.fullmatch(name):
        # AN ID IS ALREADY THE ANSWER. Fabric documents "name or ID" for this
        # shape, and listing the workspace to rediscover an id the caller
        # already holds costs a round-trip and fails outright when the caller
        # can read the item but not enumerate its neighbours.
        return name
    for item in list(workspaceId=workspaceId):
        if item.get("displayName") == name or item.get("id") == name:
            return item["id"]
    raise KeyError(f"no lakehouse named {name!r} in this workspace")


def create(name, description="", definition=None, workspaceId=""):  # noqa: N803
    """Create a lakehouse. `definition` carries {"enableSchemas": True}."""
    body = {"displayName": name}
    if description:
        body["description"] = description
    if definition:
        # The documented shape for schema support. Passed through rather than
        # interpreted: what the control plane does with it is its business.
        body["creationPayload"] = definition
    return request("POST", _base(workspaceId), token=_token(), body=body)


def get(name="", workspaceId=""):  # noqa: N803 - documented spelling
    return request("GET", f"{_base(workspaceId)}/{_resolve_id(name, workspaceId)}",
                   token=_token())


def getWithProperties(name, workspaceId=""):  # noqa: N802,N803 - documented spelling
    """`get` plus the extended properties block.

    The same call on this surface: the emulator's item response already carries
    `properties` (the SQL endpoint connection string among them). Kept as a
    separate member because a framework introspects for it, and because Fabric
    documents the two as different reads.
    """
    return get(name=name, workspaceId=workspaceId)


def update(name, newName, description="", workspaceId=""):  # noqa: N803
    """Rename a lakehouse, and optionally re-describe it."""
    body = {"displayName": newName}
    if description:
        body["description"] = description
    return request("PATCH", f"{_base(workspaceId)}/{_resolve_id(name, workspaceId)}",
                   token=_token(), body=body)


def delete(name, workspaceId=""):  # noqa: N803 - documented spelling
    request("DELETE", f"{_base(workspaceId)}/{_resolve_id(name, workspaceId)}",
            token=_token())
    return True


def list(workspaceId="", maxResults=1000):  # noqa: A001,N803 - documented spelling
    resp = request("GET", _base(workspaceId), token=_token())
    items = resp.get("value", resp) if isinstance(resp, dict) else resp
    return _list(items)[:maxResults]


class Table:
    """One entry of `listTables()`."""

    def __init__(self, name, location, type_="Managed", format_="delta"):
        self.name = name
        self.location = location
        self.type = type_
        self.format = format_

    def __repr__(self):
        return f"Table({self.name!r} {self.format} at {self.location!r})"


def listTables(lakehouse="", workspaceId="", maxResults=1000):  # noqa: N802,N803
    """Tables in a lakehouse, read from OneLake rather than a metastore.

    Nothing in this stack holds a metastore — the lakehouse's `Tables/` folder
    IS the table list, which is the same thing `livy_catalog` enumerates when a
    session binds. Read through the DFS surface so the answer comes from where
    the data is.
    """
    from . import fs

    item = _resolve_id(lakehouse, workspaceId)
    ws = _ws(workspaceId)
    root = f"abfss://{ws}@{config().onelake_host}/{item}/Tables"
    out = []
    for entry in fs.ls(root):
        if entry.isDir:
            out.append(Table(entry.name, entry.path))
    return out[:maxResults]


def loadTable(loadOption, table, lakehouse="", workspaceId=""):  # noqa: N802,N803
    """REFUSED, by name, because faking it would be worse than not having it.

    Fabric's load-table starts a server-side ingestion job: it reads the file
    named by `loadOption["relativePath"]`, infers or applies a schema, and
    writes a Delta table — with format options (header, delimiter), a load mode,
    and recursion over a folder. None of that exists in this emulator, and the
    plausible shortcut (read the CSV here, write a Delta table from the client)
    would be a DIFFERENT operation wearing the same name: no job, no server-side
    schema inference, and a silent success for options it never applied.

    A notebook can do the real thing today in one line —
    `spark.read.format("csv").option("header", True).load(path)` then
    `.write.format("delta").saveAsTable(table)` — which is the engine doing the
    work Fabric's job would do, visibly.

    The member EXISTS so a framework introspecting the surface does not decline
    to run (contract 2), and refuses when called so nothing reads a fabricated
    success as a load (docs/56, and the parity ledger's "honest 501" rule).
    """
    raise NotImplementedError(
        "notebookutils.lakehouse.loadTable is not implemented: Fabric runs a "
        "server-side ingestion job (schema inference, format options, load "
        "mode) that this emulator has no equivalent for, and a client-side "
        "read-then-write would silently ignore the options you passed. Use "
        "spark.read.format(...).load(path).write.format('delta')"
        ".saveAsTable(table) — see docs/56."
    )
