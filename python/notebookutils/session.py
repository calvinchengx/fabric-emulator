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

from ._http import request


def _agent_url():
    """Where the statement agent listens, if this is running inside one."""
    return (os.environ.get("SPARK_AGENT_URL")
            or os.environ.get("NOTEBOOKUTILS_AGENT_URL")
            or "").rstrip("/")


def _session_id():
    """This caller's Livy session, as the runtime exports it."""
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
