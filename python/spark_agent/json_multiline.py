"""`read.json(multiLine=True)` for engines whose JSON reader is NDJSON-only.

Sail parses one JSON object per line. A file that is a single JSON array —
the shape Spark JVM's `multiLine=True` reads — fails with a parser error on
a bare `[`. The Livy agent and the sail-delta matrix probe wrap
`DataFrameReader.json` for that named option only, parse the file on the
driver, and hand the rows back through `createDataFrame`.

The honest caveat, same class as Change Data Feed: the notebook API is
preserved, the plan is a LocalRelation, and a large file lands on the
driver. The distributed workaround (`from_json` + `explode` over a text
read) stays the right advice for production-shaped data.

Scope is deliberately narrow. Plain `json()`, `multiLine=False`, a list of
paths, and a caller-supplied schema all go to the engine untouched.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path


class JsonMultilineError(RuntimeError):
    """The wrap recognised the option but could not honour it faithfully."""


def multiline_truthy(value) -> bool:
    """Spark option values arrive as strings. `true`/`1`/`yes` enable."""
    if value is None:
        return False
    return str(value).strip().lower() in ("true", "1", "yes")


def _option(options, *names):
    lower = {str(k).lower(): v for k, v in (options or {}).items()}
    for name in names:
        if name.lower() in lower:
            return lower[name.lower()]
    return None


def should_intercept(path, multiLine=None, options=None, schema=None) -> bool:
    """True only for a string path whose multiLine flag is on, with no schema.

    A schema is the engine's business: we materialise dicts and do not apply
    Spark DDL. A list of paths is left alone rather than guessed at.
    """
    if schema is not None or not isinstance(path, str):
        return False
    flag = multiLine
    if flag is None:
        flag = _option(options, "multiLine")
    return multiline_truthy(flag)


def _is_spark_side_file(name: str) -> bool:
    return name.startswith(("_", ".")) or name.endswith(".crc")


def _list_local(path: str) -> list[str]:
    p = Path(path)
    if p.is_file():
        return [str(p)]
    if p.is_dir():
        return sorted(
            str(child)
            for child in p.iterdir()
            if child.is_file() and not _is_spark_side_file(child.name)
        )
    return []


def _list_abfss(path: str) -> list[str]:
    from notebookutils import fs

    try:
        entries = fs.ls(path)
    except Exception:
        # A file, not a directory — Spark still accepts a concrete JSON path.
        return [path]
    return [
        e.path
        for e in entries
        if getattr(e, "isFile", True) and not _is_spark_side_file(e.name)
    ]


def _read_text(path: str) -> str:
    """The WHOLE file, which is `read` and not `head`.

    This used to call `fs.head(path)`, and it worked only because the shim's
    `head` diverged from Fabric's: real `head` returns the first `max_bytes`
    (100 KB by default) and is a PREVIEW, while this needs every byte to parse
    JSON. Once `head` was corrected to the documented default, a JSON file over
    100 KB would have been truncated mid-document and failed to parse — or
    worse, parsed as a shorter valid document. Found while citing the surface
    (docs/56 Phase 0).
    """
    if path.startswith(("abfss://", "abfs://")):
        from notebookutils import fs

        return fs.read(path).decode("utf-8")
    return Path(path).read_text(encoding="utf-8")


def _records_from_text(text: str, source: str) -> list[dict]:
    text = text.strip()
    if not text:
        return []
    try:
        data = json.loads(text)
    except json.JSONDecodeError as exc:
        raise JsonMultilineError(
            f"{source}: not valid JSON ({exc.msg})"
        ) from exc
    if isinstance(data, dict):
        return [data]
    if isinstance(data, list):
        if all(isinstance(row, dict) for row in data):
            return data
        raise JsonMultilineError(
            f"{source}: JSON array must hold objects, not {type(data[0]).__name__ if data else 'empty'}"
        )
    raise JsonMultilineError(
        f"{source}: expected a JSON object or array of objects, got {type(data).__name__}"
    )


def records_from_path(path: str) -> list[dict]:
    """Rows a Spark `multiLine=True` read of `path` would produce.

    `path` may be a file, a Spark-writer directory (part files + `_SUCCESS`),
    or an `abfss://` URI listed through `notebookutils.fs`.
    """
    files = _list_abfss(path) if path.startswith(("abfss://", "abfs://")) else _list_local(path)
    rows: list[dict] = []
    for f in files:
        rows.extend(_records_from_text(_read_text(f), f))
    if not rows:
        raise JsonMultilineError(
            f"{path}: no JSON records — enable multiLine on a file that is a "
            "JSON object or array of objects"
        )
    return rows


def read_json_multiline(spark, path: str):
    """Parse `path` on the driver and return a materialised DataFrame."""
    print(
        "[json_multiline] read.json(multiLine=True) via driver parse "
        "(materialised LocalRelation, not a lazy scan)",
        file=sys.stderr,
        flush=True,
    )
    return spark.createDataFrame(records_from_path(path))


def patch_json_reader(reader_cls, spark):
    """Wrap `reader_cls.json` so the named option never reaches Sail."""
    if getattr(reader_cls, "_emu_json_multiline_patched", False):
        return
    orig = reader_cls.json

    def json_reader(self, path=None, *args, **kwargs):
        schema = args[0] if args else kwargs.get("schema")
        opts = dict(getattr(self, "_options", None) or {})
        if should_intercept(
            path,
            multiLine=kwargs.get("multiLine"),
            options=opts,
            schema=schema,
        ) and isinstance(path, str):
            return read_json_multiline(spark, path)
        return orig(self, path, *args, **kwargs)

    reader_cls.json = json_reader
    reader_cls._emu_json_multiline_patched = True


def install(spark) -> bool:
    """Wrap Connect `spark.read.json` for `multiLine=True`. No-op otherwise.

    Gated the same way as the CDF wrap: only a Connect DataFrameReader is
    patched, so a classic JVM session keeps Spark's native reader.
    """
    try:
        from pyspark.sql.connect.readwriter import DataFrameReader
    except ImportError:
        return False
    reader = getattr(spark, "read", None)
    if reader is None or not isinstance(reader, DataFrameReader):
        return False
    patch_json_reader(type(reader), spark)
    return True
