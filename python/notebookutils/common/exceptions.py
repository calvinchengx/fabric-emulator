"""Exceptions real Fabric raises, importable from the path Fabric puts them on.

Microsoft's documented way to read a partly-failed DAG is:

    from notebookutils.common.exceptions import RunMultipleFailedException
    try:
        results = notebookutils.notebook.runMultiple(DAG)
    except RunMultipleFailedException as ex:
        results = ex.result

so the module path, the class name and the `.result` attribute are all part of
the contract, not implementation detail.
"""


class RunMultipleFailedException(Exception):
    """Raised when any activity in a `runMultiple` DAG did not complete.

    `result` carries the SAME dict a fully successful run returns, so a caller
    can inspect what did work. Losing it and re-raising bare would make a
    partial failure indistinguishable from a total one.
    """

    def __init__(self, message, result=None):
        super().__init__(message)
        self.result = result if result is not None else {}
