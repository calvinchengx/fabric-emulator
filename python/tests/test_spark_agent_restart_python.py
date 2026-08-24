"""`restartPython()` restarts the INTERPRETER and keeps the Spark context.

That distinction is the entire method. A `pip install` in one cell is not
importable in the next until the interpreter restarts; tearing down the engine
as well would cost every cached DataFrame and temp view for no reason at all.

WHY THIS IS TESTED HERE AS WELL AS IN AN E2E. The notebookutils e2e is a single
script running INSIDE the namespace this clears — calling it mid-script is
self-destructive, so that suite cannot prove it, and this file was the only
evidence for a long time. e2e/livy CAN prove it: a Livy session is a persistent
REPL, so one statement restarts the session and the NEXT one reports what
survived. These unit tests stay because they reach the branches an e2e cannot
enumerate cheaply — a runtime with no mssparkutils, a session that does not
exist — and because they run without Spark.

`agent.py` needs a live engine to import, so the function under test is loaded
in isolation with the module-level globals it reads.
"""
import sys
import types
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))


def _restart_python():
    """`agent.restart_python`, loaded without importing the whole agent.

    Extracting the source rather than importing agent.py: that module calls
    getOrCreate() at import and would need a real engine. The alternative —
    testing nothing — is how this behaviour would have shipped unverified.
    """
    source = (Path(__file__).resolve().parents[1]
              / "spark_agent" / "agent.py").read_text(encoding="utf-8")
    start = source.index("def restart_python(session):")
    end = source.index("\ndef ", start + 1)
    module = types.ModuleType("agent_restart_slice")
    module.namespaces = {}
    module._notebookutils = lambda: None
    # The notebook BUILTINS the restart puts back. Named here rather than
    # imported so the slice still needs no engine, and recognisable in an
    # assertion so a restart that dropped one would be visible.
    module.run_magic = types.SimpleNamespace(HELPER="__nbrun__")
    module.notebook_display = types.SimpleNamespace(
        display="display-fn", displayHTML="displayHTML-fn")
    module._make_run_helper = lambda g: "run-helper-for-this-namespace"
    exec(compile(source[start:end], "agent.py", "exec"), module.__dict__)  # noqa: S102
    return module


@pytest.fixture
def agent():
    return _restart_python()


class Engine:
    """Stands in for the session's Spark handle — identity is all that matters."""


def test_the_spark_context_survives_a_restart(agent):
    """THE POINT OF THE METHOD. Fabric's page is explicit: restartPython
    'restarts the Python interpreter while keeping the Spark context intact'."""
    engine = Engine()
    agent.namespaces["s1"] = {"spark": engine, "sc": "ctx", "df": "cached"}
    out = agent.restart_python("s1")
    assert out["restarted"] is True
    assert agent.namespaces["s1"]["spark"] is engine, "the engine handle must survive"
    assert agent.namespaces["s1"]["sc"] == "ctx"


def test_user_state_does_not_survive(agent):
    """What a Python restart costs is the interpreter's state. A restart that
    quietly preserved half the namespace would be harder to reason about than
    one that cleared it all."""
    agent.namespaces["s1"] = {"spark": Engine(), "df": "cached", "counter": 7}
    out = agent.restart_python("s1")
    assert "df" not in agent.namespaces["s1"]
    assert "counter" not in agent.namespaces["s1"]
    assert out["dropped"] == ["counter", "df"], out


def test_the_caller_is_told_what_went(agent):
    """Reporting what was lost is the same rule session_recovery follows: a
    silent clear is the friendlier face of a worse failure."""
    agent.namespaces["s1"] = {"spark": Engine(), "temp_view": 1}
    out = agent.restart_python("s1")
    assert out["kept"] == ["spark"]
    assert out["dropped"] == ["temp_view"]


def test_notebookutils_is_rebound_because_a_restart_is_not_a_wipe(agent):
    """A fresh namespace gets `notebookutils` as a global. Without rebinding it
    the next statement has no notebookutils at all, which is not what a restart
    means — it means a clean interpreter, not a crippled one."""
    nbu = types.SimpleNamespace(mssparkutils=types.SimpleNamespace())
    agent._notebookutils = lambda: nbu
    agent.namespaces["s1"] = {"spark": Engine(), "leftover": 1}
    agent.restart_python("s1")
    assert agent.namespaces["s1"]["notebookutils"] is nbu
    assert agent.namespaces["s1"]["mssparkutils"] is nbu.mssparkutils


def test_a_runtime_without_mssparkutils_binds_only_what_it_has(agent):
    """The legacy alias is absent from this tree today (docs/56). Binding a
    None under that name would be worse than not binding it."""
    agent._notebookutils = lambda: types.SimpleNamespace()
    agent.namespaces["s1"] = {"spark": Engine()}
    agent.restart_python("s1")
    assert "mssparkutils" not in agent.namespaces["s1"]


def test_only_this_session_is_restarted(agent):
    """The agent is shared (docs/38 §5). Restarting the interpreter for real
    would end every other live notebook — the same reason /close drops one
    namespace rather than exiting the process."""
    other = {"spark": Engine(), "their_df": "theirs"}
    agent.namespaces["s1"] = {"spark": Engine(), "mine": 1}
    agent.namespaces["s2"] = other
    agent.restart_python("s1")
    assert agent.namespaces["s2"] is other
    assert agent.namespaces["s2"]["their_df"] == "theirs"


def test_restarting_an_unknown_session_says_so_rather_than_inventing_one(agent):
    """`ns()` would CREATE a namespace on demand — and creating one here would
    build a new isolated engine session, which is precisely what this method
    must not do."""
    out = agent.restart_python("never-seen")
    assert out["restarted"] is False
    assert "never-seen" in out["reason"]
