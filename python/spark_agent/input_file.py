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
are wrapped. A frame that never came from a file has no tag; asking
`input_file_name()` of it raises `InputFileNameError` naming the shim
(docs/37 §3b). Spark itself returns "" here — we will not, because an empty
lineage column is silently wrong.

SQL-string usage (`spark.sql("SELECT input_file_name() …")`) never touches
the patched `F.input_file_name`. The agent rewrites that call onto the tag
column when the FROM/JOIN relation is a view registered from a tagged frame,
and leaves every other mention (strings, comments) alone (docs/37 §3a).
"""
from __future__ import annotations

import fnmatch
import glob as _glob
import posixpath
import re

_TAG = "__emu_input_file_name"
_GLOB_CHARS = "*?["
_TAGGED_VIEWS: dict[str, list[str]] = {}
_FROM_JOIN = re.compile(r"\b(?:from|join)\s+(`?[\w.]+`?)", re.IGNORECASE)
_STAR = re.compile(r"(?<![\w.])\*(?!\s*\()")
_SHADOW_PREFIX = "__emu_ifn_"


class InputFileNameError(Exception):
    """Provenance was asked of a frame or relation that never came from a file."""


def forget_tagged_views():
    """Drop every remembered view. Tests that share a process need a clean map."""
    _TAGGED_VIEWS.clear()


def remember_tagged_view(name: str, columns: list[str]):
    """Record that `name` was created from a file-tagged frame."""
    if name:
        _TAGGED_VIEWS[name] = list(columns)


def shadow_view_name(name: str) -> str:
    """The view that still carries the tag, used only when SQL asks for it."""
    return f"{_SHADOW_PREFIX}{name}"


def _not_a_file_message() -> str:
    return (
        "input_file_name() was requested on a frame that never came from a "
        "file. The emulator's shim reconstructs provenance by tagging file "
        "reads; a range, table, or untagged view has no path to give. Do not "
        "stub this to an empty string — that is silently wrong lineage "
        "(docs/37 §3b)."
    )


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


def _code_chunks(sql: str):
    """Yield (start, end, text) of SQL that is not a string or a comment."""
    i = 0
    n = len(sql)
    code_start = 0
    while i < n:
        two = sql[i:i + 2]
        if two == "--":
            if i > code_start:
                yield code_start, i, sql[code_start:i]
            nl = sql.find("\n", i)
            i = n if nl < 0 else nl + 1
            code_start = i
            continue
        if two == "/*":
            if i > code_start:
                yield code_start, i, sql[code_start:i]
            end = sql.find("*/", i + 2)
            i = n if end < 0 else end + 2
            code_start = i
            continue
        if sql[i] in ("'", '"'):
            quote = sql[i]
            if i > code_start:
                yield code_start, i, sql[code_start:i]
            i += 1
            while i < n:
                if sql[i] == quote:
                    if i + 1 < n and sql[i + 1] == quote:
                        i += 2
                        continue
                    i += 1
                    break
                i += 1
            code_start = i
            continue
        i += 1
    if code_start < n:
        yield code_start, n, sql[code_start:]


def _function_calls(sql: str, name: str) -> list[tuple[int, int]]:
    """Absolute [start, end) spans of `name()` in code, not in strings/comments."""
    spans = []
    needle = name.lower()
    for start, _end, chunk in _code_chunks(sql):
        lower = chunk.lower()
        i = 0
        while True:
            j = lower.find(needle, i)
            if j < 0:
                break
            if j > 0 and (chunk[j - 1].isalnum() or chunk[j - 1] == "_"):
                i = j + 1
                continue
            after = chunk[j + len(name):].lstrip()
            if after.startswith("("):
                close = chunk.find(")", j)
                spans.append((start + j, start + (close + 1 if close >= 0 else j + len(name))))
            i = j + 1
    return spans


def sql_uses_input_file_name(sql: str) -> bool:
    return bool(_function_calls(sql, "input_file_name"))


def _relation_names(sql: str) -> list[str]:
    names = []
    for _start, _end, chunk in _code_chunks(sql):
        for match in _FROM_JOIN.finditer(chunk):
            names.append(match.group(1).replace("`", "").rsplit(".", 1)[-1])
    return names


def _rewrite_relations(sql: str, tagged: list[str]) -> str:
    tagged_l = {t.lower() for t in tagged}
    out = []
    last = 0
    for start, end, chunk in _code_chunks(sql):
        out.append(sql[last:start])
        rewritten = chunk
        for match in reversed(list(_FROM_JOIN.finditer(chunk))):
            raw = match.group(1)
            name = raw.replace("`", "").rsplit(".", 1)[-1]
            if name.lower() not in tagged_l:
                continue
            shadow = shadow_view_name(name)
            rewritten = rewritten[:match.start(1)] + shadow + rewritten[match.end(1):]
        out.append(rewritten)
        last = end
    out.append(sql[last:])
    return "".join(out)


def _expand_stars(sql: str, columns: list[str]) -> str:
    if not columns:
        return sql
    replacement = ", ".join(columns)
    out = []
    last = 0
    for start, end, chunk in _code_chunks(sql):
        out.append(sql[last:start])
        out.append(_STAR.sub(replacement, chunk))
        last = end
    out.append(sql[last:])
    return "".join(out)


def rewrite_sql_input_file_name(sql: str) -> str:
    """Rewrite `input_file_name()` onto the tag when the relation is tagged.

    A UDF cannot do this: the function takes no arguments, so it cannot see
    the row or the tag. Blind string replace would corrupt a query that merely
    mentions the name. Leave those alone; fail loud when the relation was
    never a file read.
    """
    if not isinstance(sql, str) or not sql_uses_input_file_name(sql):
        return sql
    rels = _relation_names(sql)
    tagged = []
    columns = []
    known = {k.lower(): (k, v) for k, v in _TAGGED_VIEWS.items()}
    for rel in rels:
        hit = known.get(rel.lower())
        if hit:
            tagged.append(hit[0])
            columns = hit[1]
    if not tagged:
        raise InputFileNameError(_not_a_file_message())
    out = sql
    for start, end in reversed(_function_calls(sql, "input_file_name")):
        out = out[:start] + f"`{_TAG}`" + out[end:]
    out = _rewrite_relations(out, tagged)
    return _expand_stars(out, columns)


def _install_sql(spark) -> None:
    """Wrap `spark.sql` so SQL-string usage hits the same tag the PySpark wrap does."""
    original = getattr(spark, "sql", None)
    if original is None or getattr(original, "_emu_input_file_sql", False):
        return

    def sql(query, *args, **kwargs):
        if isinstance(query, str) and sql_uses_input_file_name(query):
            query = rewrite_sql_input_file_name(query)
        return original(query, *args, **kwargs)

    sql._emu_input_file_sql = True
    spark.sql = sql


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
        _install_sql(spark)
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

    # Swapping pyspark's own `input_file_name` for the tag-resolving one is this
    # module's reason to exist; ty sees a signature that returns Unknown
    # replacing one that returns Column.
    F.input_file_name = _input_file_name  # ty: ignore[invalid-assignment]

    # --- keeping the tag invisible ------------------------------------------
    #
    # Stripping it at write covers persisted data. It does NOT cover the schema
    # a notebook can see, and that gap was a real defect: a landing-to-bronze
    # step counted `len(df.columns)` and got one more column than its own vendor
    # export had, on every file read in the shipped stack. The written table was
    # correct, which is what made it confusing to chase.
    #
    # So the tag is hidden from every surface a user can observe it through,
    # not just from writes. `df.columns`, `printSchema`, `SELECT *` and
    # `toPandas` are named in docs/37 as the leak this module must not create —
    # tagging only file reads narrows WHICH frames are affected, it does not
    # make the leak acceptable on those frames.
    #
    # One helper, applied at each surface, so adding a surface is one line and
    # missing one is visible in this list rather than hidden in a method body.
    original_columns = _ConnectDataFrame.columns

    def _raw_columns(df):
        """Columns INCLUDING the tag — the shim's own view."""
        return original_columns.fget(df)

    def _visible(df):
        """The frame as the user should see it: no bookkeeping column."""
        try:
            if _TAG in _raw_columns(df):
                return df.drop(_TAG)
        except Exception:
            pass
        return df

    # Properties first: schema-shaped surfaces.
    for _name in ("columns", "schema", "dtypes"):
        _original_prop = getattr(_ConnectDataFrame, _name)

        def _make(prop):
            @property
            def hidden(self):
                return prop.fget(_visible(self))

            return hidden

        setattr(_ConnectDataFrame, _name, _make(_original_prop))

    # Then the methods that render or materialise rows. `select`/`selectExpr`
    # are here for `*`: star expansion happens on the server against the plan
    # it is given, so the tag has to be gone before the request leaves.
    for _name in (
        "printSchema",
        "show",
        "collect",
        "take",
        "head",
        "first",
        "toPandas",
        "toLocalIterator",
    ):
        if not hasattr(_ConnectDataFrame, _name):
            continue
        _original_method = getattr(_ConnectDataFrame, _name)

        def _make_method(method):
            def hidden(self, *args, **kwargs):
                return method(_visible(self), *args, **kwargs)

            return hidden

        setattr(_ConnectDataFrame, _name, _make_method(_original_method))

    # `select` and `selectExpr` cannot hide unconditionally: this is where
    # `input_file_name()` is USED, and it resolves to the tag. Hiding it here
    # would break the one thing the module exists to provide. So the tag stays
    # visible to a select that references it, and is hidden from every other
    # select — which is what makes `SELECT *` safe without making provenance
    # impossible.
    for _name in ("select", "selectExpr"):
        if not hasattr(_ConnectDataFrame, _name):
            continue
        _original_select = getattr(_ConnectDataFrame, _name)

        def _make_select(method):
            def hidden(self, *args, **kwargs):
                wants_tag = any(_TAG in str(a) for a in args)
                if wants_tag:
                    try:
                        raw = _raw_columns(self)
                    except Exception:
                        raw = []
                    if _TAG not in raw:
                        raise InputFileNameError(_not_a_file_message())
                    return method(self, *args, **kwargs)
                return method(_visible(self), *args, **kwargs)

            return hidden

        setattr(_ConnectDataFrame, _name, _make_select(_original_select))

    # Writes keep their own strip: `_visible` would do it, but the write path
    # is a property returning a writer rather than a frame, so it stays
    # explicit rather than being folded into the loop above.
    original_write = _ConnectDataFrame.write

    @property
    def write(self):
        return original_write.fget(_visible(self))

    # Same shape as the writeStream patch in eventstream_kafka: a property
    # replacing a property, so the tag is stripped before anything persists.
    _ConnectDataFrame.write = write  # ty: ignore[invalid-assignment]

    # Views: SELECT * stays on a clean registration so the tag cannot leak
    # through SQL star expansion. A shadow view keeps the tag; spark.sql
    # rewrites input_file_name() onto that shadow (docs/37 §3a).
    for _name in (
        "createOrReplaceTempView",
        "createTempView",
        "createOrReplaceGlobalTempView",
    ):
        if not hasattr(_ConnectDataFrame, _name):
            continue
        _original_view = getattr(_ConnectDataFrame, _name)

        def _make_view(method):
            def hidden(self, name, *args, **kwargs):
                try:
                    raw = _raw_columns(self)
                except Exception:
                    raw = []
                if _TAG in raw:
                    remember_tagged_view(name, [c for c in raw if c != _TAG])
                    method(self, shadow_view_name(name), *args, **kwargs)
                return method(_visible(self), name, *args, **kwargs)

            return hidden

        setattr(_ConnectDataFrame, _name, _make_view(_original_view))

    _install_sql(spark)
    reader_cls._emu_input_file_patched = True
    return True
