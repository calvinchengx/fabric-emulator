"""notebookutils.session — the lifecycle of the notebook session itself.

Two methods, and they mean different things to the process they run in:
`stop()` ends the session and releases its Spark resources; `restartPython()`
restarts the interpreter while LEAVING the Spark context alone.

WHY THIS SHIM CANNOT JUST CALL sys.exit(). The emulator runs one long-lived
agent behind many session namespaces (docs/38 §5). Ending "the session" here
means ending THIS caller's namespace, not the process — a `sys.exit()` would
take every other live notebook down with it, which is the shared-agent leak
contract 5 exists to catch, in its most destructive form.

So both methods ask the AGENT to do it, over the same loopback the rest of the
shim uses, and the agent decides what "this session" means. Where no agent is
reachable — a plain `python` importing the shim — they say so rather than
pretending to have stopped something.
"""
import os
from contextvars import ContextVar

from ._http import request

# BOUND PER STATEMENT, NOT READ FROM THE ENVIRONMENT. The agent is one process
# behind many sessions, so `FABRIC_SESSION_ID` in os.environ would be a single
# value shared by every concurrent notebook — one session would stop another's,
# which is the shared-agent leak contract 5 exists to catch, in its most
# destructive form. `runtime.context` already solved this with a ContextVar the
# agent binds around each statement; this is the same mechanism for the same
# reason, kept separate so the documented context dict gains no invented keys.
#
# Until this existed both methods raised "running outside a notebook session"
# INSIDE a notebook session — nothing set those variables anywhere in the tree
# — so the two members were unreachable in the one place they are meant to be
# used. Unit tests passed throughout, because they set the environment.
_bound: ContextVar[tuple | None] = ContextVar("notebookutils_session", default=None)


def bind(agent_url, session_id):
    """Pin this statement to its agent and session. Returns a token for unbind."""
    return _bound.set((agent_url or "", session_id or ""))


def unbind(token):
    _bound.reset(token)


def _agent_url():
    """Where the statement agent listens, if this is running inside one."""
    bound = _bound.get()
    if bound and bound[0]:
        return bound[0].rstrip("/")
    return (os.environ.get("SPARK_AGENT_URL")
            or os.environ.get("NOTEBOOKUTILS_AGENT_URL")
            or "").rstrip("/")


def _session_id():
    """This caller's Livy session, as the runtime exports it."""
    bound = _bound.get()
    if bound and bound[1]:
        return bound[1]
    return (os.environ.get("FABRIC_SESSION_ID")
            or os.environ.get("LIVY_SESSION_ID")
            or "")


def _ask_agent(path, payload):
    base = _agent_url()
    if not base:
        raise RuntimeError(
            "notebookutils.session needs the statement agent: this is running "
            "outside a notebook session, so there is no session to act on. Set "
            "SPARK_AGENT_URL if you are driving the agent directly.")
    return request("POST", f"{base}{path}", body=payload)


def stop(detach=True):
    """Stop this interactive session and release its Spark resources.

    `detach` is documented on the PySpark/Scala/R surface and defaults to True:
    in a high-concurrency session it detaches this notebook rather than
    stopping the shared session out from under its siblings. Passed through to
    the agent, which is the only component that knows whether the session it
    holds is shared.

    Nothing after this runs, on Fabric or here — the page says so, and it is
    the reason the method returns nothing.
    """
    _ask_agent("/close", {"session": _session_id(), "detach": bool(detach)})


def restartPython():  # noqa: N802 - documented spelling
    """Restart the Python interpreter, keeping the Spark context.

    The distinction is the whole point: a `pip install` in one cell is not
    importable in the next until the interpreter restarts, and restarting the
    SPARK context as well would cost every cached DataFrame and temp view for
    no reason. The agent rebuilds this session's namespace and leaves its
    engine handle alone.
    """
    _ask_agent("/restart-python", {"session": _session_id()})
