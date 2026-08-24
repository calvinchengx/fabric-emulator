"""A functional `notebookutils` for the Microsoft Fabric emulator.

Microsoft ships `notebookutils` as an import-only stub outside the Fabric
runtime; this package makes the same surface *work* against the emulator
family so notebook code — `notebookutils.fs`, `.credentials`, `.lakehouse`,
`.runtime`, `.env`, `.notebook` — runs unchanged against a local Fabric.

The wiring (endpoints, notebook identity, default lakehouse) comes from the
environment the emulator injects; see notebookutils._config.

    import notebookutils
    notebookutils.fs.put("abfss://ws@onelake.dfs.fabric.microsoft.com/lake.Lakehouse/Files/x.txt", "hi")
    tok = notebookutils.credentials.getToken("storage")
    pw  = notebookutils.credentials.getSecret("myvault", "db-password")
    p   = notebookutils.variableLibrary.get("$(/**/envLib/bronzePath)")
"""
# `mssparkutils` is the older name for the same surface; alias it so notebooks
# written against either name resolve here (attribute *and* `import mssparkutils`).
import sys as _sys

from . import (  # noqa: F401
    credentials,
    env,
    fs,
    lakehouse,
    nbresources,
    notebook,
    runtime,
    session,
    udf,
    variableLibrary,
)


def __getattr__(name):
    """`nbResPath` is a VALUE on Fabric, not a call: notebooks write
    `notebookutils.nbResPath` and get a path. Computing it at import would mean
    an HTTP round-trip every time this package is imported — including in
    processes with no notebook identity at all — so it is resolved on first
    read instead. PEP 562 module __getattr__ is what makes that possible
    without turning the documented attribute into a function."""
    if name == "nbResPath":
        return nbresources.nb_res_path()
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


mssparkutils = _sys.modules[__name__]
_sys.modules.setdefault("mssparkutils", _sys.modules[__name__])

__all__ = ["credentials", "env", "fs", "lakehouse", "mssparkutils", "nbResPath",
           "nbresources", "notebook", "runtime", "session", "udf", "variableLibrary"]
