"""A functional `notebookutils` for the Microsoft Fabric emulator.

Microsoft ships `notebookutils` as an import-only stub outside the Fabric
runtime; this package makes the same surface *work* against the emulator
family so notebook code — `notebookutils.fs`, `.credentials`, `.lakehouse`,
`.runtime`, `.notebook` — runs unchanged against a local Fabric.

The wiring (endpoints, notebook identity, default lakehouse) comes from the
environment the emulator injects; see notebookutils._config.

    import notebookutils
    notebookutils.fs.put("abfss://ws@onelake.dfs.fabric.microsoft.com/lake.Lakehouse/Files/x.txt", "hi")
    tok = notebookutils.credentials.getToken("storage")
    pw  = notebookutils.credentials.getSecret("myvault", "db-password")
"""
from . import fs, credentials, lakehouse, runtime, notebook

# `mssparkutils` is the older name for the same surface; alias it so notebooks
# written against either name resolve here (attribute *and* `import mssparkutils`).
import sys as _sys

mssparkutils = _sys.modules[__name__]
_sys.modules.setdefault("mssparkutils", _sys.modules[__name__])

__all__ = ["fs", "credentials", "lakehouse", "runtime", "notebook", "mssparkutils"]
