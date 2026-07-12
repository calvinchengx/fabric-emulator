"""notebookutils.runtime — the session's context.

Mirrors the real `notebookutils.runtime.context` dict: the workspace and
default lakehouse the notebook is attached to. Sourced from the environment
the emulator injects (see _config).
"""
from ._config import config


class _Context(dict):
    """dict access (context["currentWorkspaceId"]) and attribute access, to
    match how notebooks read runtime.context in the wild."""
    def __getattr__(self, k):
        try:
            return self[k]
        except KeyError as e:
            raise AttributeError(k) from e


context = _Context({
    "currentWorkspaceId": config().workspace_id,
    "defaultLakehouseId": config().lakehouse_id,
    "isForPipeline": False,
})
