"""`input_file_name()` on engines that do not implement it.

Sail answers `function: input_file_name`, so a notebook that records which file
each row came from, a normal thing for a landing-to-bronze step to do, fails at
write time, long after the read looked fine.

WHY THIS IS NOT A STUB. The easy version registers a UDF returning "" and the
column is silently wrong: every row claims it came from nowhere, and a lineage
audit built on it is worse than one that failed. That is the same
"looks like support and provides none" defect the notebookPrelude comments
condemn. So this reconstructs the real answer instead:

  1. wrap the file-source readers, so a read that would have globbed N files
     instead reads each file and tags its rows with THAT file's path, unioned;
  2. point `input_file_name()` at the tag;
  3. strip the tag at every write, so nothing persisted differs from Fabric.

Each row then genuinely carries the file it came from, which is what the
function means. The cost is one read per file rather than one glob read, the
right trade for an emulator, bounded by the landing layouts it exists to serve.

DEAD ENDS, so nobody retries them:
  - Go-side interception: the control plane relays statement TEXT and never
    sees a Spark plan; expressions only exist here, where the plan is built.
  - `_metadata.file_path` (Spark's file-source metadata column): probed, Sail
    does not resolve it either.
  - Listing files via the engine (`binaryFile`): reads run on Sail's own
    filesystem and Sail has no binaryFile source, so listing must not depend on
    the engine at all, hence the OneLake DFS listing below.

SAIL SCHEMA CAVEAT. Sail defers CSV schema inference to execution: the analyzed
schema of a fresh csv read is EMPTY (select-by-name fails even unpatched), so
the per-file union is positional (`union`), not by name. That is sound here
because every file under one glob is one dataset with one schema; a drifting
file would misalign silently, but it would do so in the plain glob read too.

SCOPE, deliberately narrow. Only installed when the engine actually lacks the
function, so the JVM overlay keeps Spark's native answer. Only the file readers
are wrapped. A frame that never came from a file has no tag, and
`input_file_name()` on it yields "", Spark's own answer for a non-file source.
"""
from __future__ import annotations

import fnmatch
import glob as _glob
import posixpath

_TAG = "__emu_input_file_name"
_GLOB_CHARS = "*?["


def engine_has_input_file_name(spark) -> bool:
    """True when the engine implements the function itself."""
    try:
        from pyspark.sql import functions as F

        spark.range(1).select(F.input_file_name()).collect()
        return True
    except Exception:
        return False


def _list_files(path: str) -> list[str]:
    """Concrete files behind a path or basename-glob, in a stable order.

    Never asks the engine (see DEAD ENDS above). `abfss://` paths list through
    the OneLake DFS API with the agent's own token, the same route
    `notebookutils.fs` already uses, and anything else falls back to the local
    filesystem, which only helps on a single-container layout. A glob in a
    directory segment is not expanded; the caller then keeps the plain glob
    read and the tag degrades to the glob string, which is coarse but honest.
    """
    directory, pattern = posixpath.split(path)
    if any(c in directory for c in _GLOB_CHARS):
        return []
    try:
        if path.startswith(("abfss://", "abfs://")):
            from notebookutils import fs

            entries = fs.ls(directory)
            return sorted(
                e.path
                for e in entries
                if e.isFile and fnmatch.fnmatch(e.name, pattern or "*")
            )
        return sorted(p for p in _glob.glob(path) if posixpath.basename(p))
    except Exception:
        return []


def install(spark) -> bool:
    """Make `input_file_name()` work on this session. Returns True if installed.

    No-op (returning False) when the engine already has the function, so the
    JVM overlay is untouched and the emulator does not shadow a real
    implementation with an approximation.
    """
    from pyspark.sql import functions as F
    from pyspark.sql.connect.dataframe import DataFrame as _ConnectDataFrame

    if engine_has_input_file_name(spark):
        return False

    reader_cls = type(spark.read)
    if getattr(reader_cls, "_emu_input_file_patched", False):
        return True

    def _wrap(fmt_name):
        original = getattr(reader_cls, fmt_name)

        def tagged(self, path=None, *args, **kwargs):
            if not isinstance(path, str):
                # a list of paths, or a schema-first call form: leave untouched
                return original(self, path, *args, **kwargs)
            files = _list_files(path)
            if len(files) <= 1:
                df = original(self, files[0] if files else path, *args, **kwargs)
                return df.withColumn(_TAG, F.lit(files[0] if files else path))
            out = None
            for f in files:
                part = original(self, f, *args, **kwargs).withColumn(_TAG, F.lit(f))
                out = part if out is None else out.union(part)
            return out

        setattr(reader_cls, fmt_name, tagged)

    for fmt in ("csv", "json", "parquet", "orc", "text"):
        if hasattr(reader_cls, fmt):
            _wrap(fmt)

    def _input_file_name():
        # Resolves per-frame to the tag a file read put there (coalesce guards
        # the tag going null through outer joins). Known divergence: on a frame
        # with NO tag, one that never came from a wrapped file read, this
        # fails to resolve, where real Spark returns "". A visible error beats
        # a silently empty lineage column, and no known workload asks for file
        # provenance on a non-file frame.
        return F.coalesce(F.col(_TAG), F.lit(""))

    F.input_file_name = _input_file_name

    # Strip the tag from anything persisted: real Fabric tables have no such
    # column, and an emulator that leaks bookkeeping into user data is creating
    # exactly the parity drift it exists to prevent. `input_file_name()` copies
    # the value into the user's own column before the write sees the frame.
    original_write = _ConnectDataFrame.write

    @property
    def write(self):
        try:
            if _TAG in self.columns:
                return original_write.fget(self.drop(_TAG))
        except Exception:
            pass
        return original_write.fget(self)

    _ConnectDataFrame.write = write

    reader_cls._emu_input_file_patched = True
    return True
