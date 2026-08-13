"""notebookutils.env — the identity probes a Fabric framework runs first.

`mssparkutils.env.getWorkspaceId()` is the first link of the context fallback
chain (docs/38 §1). It must answer the *running* notebook, not a process
environment set out of band.
"""
from . import runtime


def getWorkspaceId():
    return runtime.context["currentWorkspaceId"]


def getLakehouseId():
    return runtime.context["defaultLakehouseId"]
