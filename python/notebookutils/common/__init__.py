"""`notebookutils.common` — the namespace real Fabric puts shared types in.

Exists so `from notebookutils.common.exceptions import RunMultipleFailedException`
resolves here exactly as it does on Fabric. Code that catches that exception is
following Microsoft's own documented pattern for reading partial DAG results,
and an import error is the one outcome that makes it unrunnable locally.
"""
