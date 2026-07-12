"""notebookutils.lakehouse — lakehouse CRUD via the Fabric control plane.

Thin wrapper over /v1/workspaces/{id}/lakehouses using a fabric-audience token
minted for the notebook identity. Workspace defaults to the runtime context.
"""
from ._config import config
from ._http import request
from . import credentials


def _ws(workspaceId):
    ws = workspaceId or config().workspace_id
    if not ws:
        raise RuntimeError("no workspace: pass workspaceId or set NOTEBOOKUTILS_WORKSPACE_ID")
    return ws


def _base(workspaceId):
    return f"{config().fabric_url}/v1/workspaces/{_ws(workspaceId)}/lakehouses"


def create(name, description=None, workspaceId=None):
    body = {"displayName": name}
    if description:
        body["description"] = description
    return request("POST", _base(workspaceId), token=credentials.getToken("fabric"), body=body)


def get(lakehouseId, workspaceId=None):
    return request("GET", f"{_base(workspaceId)}/{lakehouseId}", token=credentials.getToken("fabric"))


def list(workspaceId=None):
    resp = request("GET", _base(workspaceId), token=credentials.getToken("fabric"))
    return resp.get("value", resp)
