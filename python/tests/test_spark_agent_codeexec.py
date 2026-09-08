"""`run_code` executes one statement block and shapes the Livy REPL result.

Every Python statement a notebook sends reaches the engine through this
function, and until it was split out of agent.py it had NO test: importing
`agent` runs `_install_custom_wheels()` and needs pyspark, so nothing could
import it in-process. It now sits beside run_magic and task_scope, which this
package already keeps importable-without-an-engine for exactly that reason.

The assertions are on the RETURNED DOCUMENT, because that document is the wire
contract a Livy client reads: `status`, `ename`, `evalue`, `traceback`, and
`data["text/plain"]`. A test that only checked "it did not raise" would pass
while the client saw the wrong shape.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import codeexec  # noqa: E402


def test_a_trailing_expression_comes_back_as_its_repr():
    """Livy semantics: the last expression is the cell's value."""
    got = codeexec.run_code("1 + 1", {})
    assert got["status"] == "ok"
    assert got["data"]["text/plain"] == "2"


def test_statements_without_a_trailing_expression_return_no_value():
    g = {}
    got = codeexec.run_code("x = 41\nx += 1", g)
    assert got["status"] == "ok"
    # The block ran in the caller's namespace...
    assert g["x"] == 42
    # ...and produced no value, which is not the same as producing "None".
    assert got["data"]["text/plain"] == ""


def test_stdout_is_captured_and_precedes_the_value():
    got = codeexec.run_code("print('hello')\n7", {})
    assert got["data"]["text/plain"] == "hello\n7"


def test_a_none_valued_last_expression_contributes_nothing():
    """`None` is the value of a bare call, and repr(None) would be noise."""
    got = codeexec.run_code("print('x')\nNone", {})
    assert got["data"]["text/plain"] == "x\n"


def test_a_syntax_error_is_reported_rather_than_raised():
    got = codeexec.run_code("def (", {})
    assert got["status"] == "error"
    assert got["ename"] == "SyntaxError"
    assert got["evalue"] == "invalid syntax"
    assert got["traceback"], "a syntax error must carry a traceback"


def test_a_runtime_error_is_reported_with_its_traceback():
    got = codeexec.run_code("raise ValueError('boom')", {})
    assert got["status"] == "error"
    assert got["ename"] == "Error"
    assert "boom" in got["evalue"]
    assert any("ValueError" in line for line in got["traceback"])


def test_a_notebook_exit_stashes_its_value_in_THIS_namespace():
    """The exit value must land in the caller's globals, not a shared module.

    Each session's prelude re-patches the one shared notebookutils, so the
    raising class belongs to whichever session ran its prelude last. Matching by
    type NAME rather than identity is what keeps a SUCCESS exit from being
    recorded against another session -- observed both ways, including a real
    failure inheriting another run's exit value, which reads as a false green.
    """
    g = {}
    src = (
        "class _NotebookExit(Exception):\n"
        "    pass\n"
        "raise _NotebookExit('done')\n"
    )
    got = codeexec.run_code(src, g)
    assert got["status"] == "error"
    assert g["__nb_exit__"] == "done"


def test_an_exit_class_defined_in_another_module_still_matches_by_name():
    """Identity would fail here; the name is what the contract rests on."""
    ns = {}
    exec("class _NotebookExit(Exception):\n    pass\n", ns)
    g = {"Foreign": ns["_NotebookExit"]}
    got = codeexec.run_code("raise Foreign('bye')", g)
    assert got["status"] == "error"
    assert g["__nb_exit__"] == "bye"


def test_an_unrelated_exception_does_not_stash_an_exit_value():
    """The dual of the test above: a real failure must not look like an exit."""
    g = {}
    codeexec.run_code("raise RuntimeError('nope')", g)
    assert "__nb_exit__" not in g


def test_run_magic_is_expanded_before_python_sees_it():
    """`%run` is a line magic and a syntax error to Python, so it has to become
    a call before ast.parse runs -- otherwise every cell using one would come
    back as SyntaxError."""
    import run_magic

    g = {run_magic.HELPER: lambda *a, **k: "ran"}
    got = codeexec.run_code("%run other", g)
    assert got["status"] == "ok", got
