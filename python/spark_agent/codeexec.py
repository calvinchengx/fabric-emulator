"""Execute one Python statement block and shape the REPL result.

SPLIT OUT OF agent.py TO BREAK THE ONE IMPORT CYCLE IN THIS REPOSITORY.
`agent` imported `usercontext` at module scope while `usercontext._dispatch`
imported `agent` back, inside the function, for this one call. A deferred
import hides a cycle at runtime; it does not remove it, and the direction that
mattered was never agent -> usercontext but this single edge.

The second reason is testability, and it is the larger one. Importing `agent`
runs `_install_custom_wheels()` and needs pyspark, so NO test imports it -- and
`run_code`, the function every Python statement in a notebook goes through, had
no test at all. Here it sits beside `run_magic`, `task_scope` and `sqlrun`,
which this package already keeps importable-without-an-engine for exactly this
reason, and which are unit-tested.

IMPORTING run_magic HERE IS SAFE despite agent.py deferring it until after
`_install_custom_wheels()`. That deferral groups the modules that may want a
custom wheel; `run_magic` and `task_scope` import only the standard library
(json, re / os, sys, types, contextlib, contextvars), so nothing they need can
arrive in a wheel and neither can be affected by the ordering.
"""

import ast
import io
import traceback

import run_magic
import task_scope


def run_code(code, g):
    """Exec the block; if its last statement is an expression, eval that and
    return its repr as the REPL result (Livy semantics). Capture stdout too."""
    out = io.StringIO()
    # `%run` is a LINE magic inside an ordinary Python cell, so the cell parser
    # correctly leaves it alone — but it is a syntax error to Python, and has
    # to become a call before ast.parse sees it. See run_magic.py.
    code = run_magic.expand(code)
    try:
        tree = ast.parse(code, mode="exec")
    except SyntaxError:
        return {"status": "error", "ename": "SyntaxError",
                "evalue": "invalid syntax", "traceback": traceback.format_exc().splitlines()}
    last_expr = None
    if tree.body and isinstance(tree.body[-1], ast.Expr):
        last_expr = ast.Expression(tree.body.pop().value)
    try:
        # NOT redirect_stdout. That assigns `sys.stdout`, one attribute on one
        # module per interpreter, and this server runs statements concurrently:
        # measured (#346), one task's response carried another's output and two
        # came back empty. `task_scope.capturing` binds the buffer in a
        # ContextVar instead, so each statement resolves its own and neither
        # restores over the other.
        with task_scope.capturing(out):
            if tree.body:
                exec(compile(tree, "<statement>", "exec"), g)
            result = eval(compile(last_expr, "<statement>", "eval"), g) if last_expr is not None else None
        text = out.getvalue()
        if result is not None:
            text += repr(result)
        return {"status": "ok", "execution_count": 0, "data": {"text/plain": text}}
    except Exception as exc:
        # A graceful notebook exit surfaces here as an exception named
        # _NotebookExit (raised by the driver prelude's patched
        # notebookutils.notebook.exit). Stash its value in THIS session's
        # globals, the prelude cannot: each session's prelude re-patches the
        # one shared notebookutils module, so under concurrent notebook runs
        # the raising function belongs to whichever session ran its prelude
        # last, and its `global __nb_exit__` writes into that session's
        # namespace, not the caller's. Observed both ways: SUCCESS exits
        # recorded Failed, and the dual, a real failure inheriting another
        # run's exit value, would read as a false green. Matching by type
        # NAME, not identity, for the same reason: every session defines its
        # own _NotebookExit class.
        if type(exc).__name__ == "_NotebookExit":
            g["__nb_exit__"] = str(exc)
        tb = traceback.format_exc().splitlines()
        return {"status": "error", "ename": "Error", "evalue": tb[-1] if tb else "error", "traceback": tb}
