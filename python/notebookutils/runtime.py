"""notebookutils.runtime — the session's context.

Mirrors the real `notebookutils.runtime.context` dict: the workspace and
default lakehouse the notebook is attached to.

A kernel without an agent still reads the emulator-injected environment
(see _config). Inside the Spark agent, each statement binds the running
notebook's identity so two sessions in one process cannot see each other's
workspace (docs/38 §1). Import-time freeze was process-global.
"""
import sys as _sys
from collections.abc import Mapping
from contextvars import ContextVar

from . import _help
from ._config import config

_bound: ContextVar[dict | None] = ContextVar("notebookutils_runtime_context", default=None)


class _Context(dict):
    """dict access (context["currentWorkspaceId"]) and attribute access, to
    match how notebooks read runtime.context in the wild."""
    def __getattr__(self, k):
        try:
            return self[k]
        except KeyError as e:
            raise AttributeError(k) from e


def _from_env():
    c = config()
    return _Context({
        "currentWorkspaceId": c.workspace_id,
        "defaultLakehouseId": c.lakehouse_id,
        "isForPipeline": False,
    })


class _LiveContext(Mapping):
    """Fabric's runtime.context, reading the running statement when one is bound.

    Attribute access matches the wild (`context.currentWorkspaceId`) and must
    raise AttributeError — not KeyError — so hasattr / getattr(..., default)
    work the way frameworks probe optional fields.
    """

    def _inner(self):
        bound = _bound.get()
        return bound if bound is not None else _from_env()

    def __getitem__(self, k):
        return self._inner()[k]

    def __iter__(self):
        return iter(self._inner())

    def __len__(self):
        return len(self._inner())

    def __getattr__(self, k):
        try:
            return self._inner()[k]
        except KeyError as e:
            raise AttributeError(k) from e


context = _LiveContext()


def bind(overrides):
    """Pin this statement to a notebook's identity. Returns a token for unbind.

    Empty values fall through to the environment, so a Livy session that has
    not mounted a lakehouse still sees NOTEBOOKUTILS_* if the operator set them.
    """
    inner = _from_env()
    if overrides:
        for key, value in overrides.items():
            if value is None or value == "":
                continue
            inner[key] = value
    return _bound.set(inner)


def unbind(token):
    _bound.reset(token)


def getCurrentWorkspaceId():  # noqa: N802 - Microsoft's spelling
    """The running notebook's workspace id.

    The same value as `context["currentWorkspaceId"]`, through the same bound
    context — NOT a second read of the environment, which is what would make
    the two answers able to disagree. Contract 1 exists because a runtime that
    answers from an environment fallback can hide two broken control-plane
    links; a second accessor with its own fallback would reopen that.
    """
    return context.get("currentWorkspaceId", "")


def help(method_name=None):  # noqa: A001 - Fabric's own spelling, on every module
    """List this module's methods, or document one of them.

    Fabric's `fs` page opens by documenting `notebookutils.fs.help()` as the
    discovery mechanism, and the stubs carry it on every module. Shadows the
    builtin inside this module only, exactly as Microsoft's package does.
    """
    _help.emit(_sys.modules[__name__], method_name)
