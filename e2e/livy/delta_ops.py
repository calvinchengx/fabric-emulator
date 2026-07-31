"""Delta maintenance operations Sail does not implement, executed by delta-rs.

Sail's SQL planner has no `OPTIMIZE`/`VACUUM` (it answers `found OPTIMIZE at
0:8 expected something else`) and rejects Change Data Feed reads. Those are the
gaps `docs/engine-matrix.md` measures. They share a property the streaming gaps
do not: each is a **bounded statement that starts and finishes against a table
path**, carrying no Spark session state. That makes them interceptable —
a streaming query, which lives inside the engine, is not.

So the agent recognises exactly these statements and runs them through
**delta-rs** (`deltalake`), a real Delta Lake implementation, against the same
table the Spark session would have touched. The result is a genuinely compacted
table, genuinely expired files, or a genuine CDF read — real Parquet and a real
`_delta_log`, not a synthesized answer.

**The honest caveat, repeated in the docs:** the *emulator* performs these, not
the Spark engine. Someone watching Spark sees no job. The observable outcome on
the table is real; the executor differs. This is a deliberate, documented
divergence — the alternative is an honest failure, which helps nobody testing a
notebook that calls `OPTIMIZE`.

Scope is deliberately narrow. Anything not matched here goes to Spark
untouched: a shim that guesses would be worse than the gap.
"""
import re

# `OPTIMIZE delta.`path``, `OPTIMIZE tbl`, optionally with a WHERE predicate or
# ZORDER clause. The predicate/zorder are captured so they can be *refused*
# rather than silently ignored — quietly dropping a WHERE would compact more
# than the user asked for.
_OPTIMIZE = re.compile(
    r"^\s*OPTIMIZE\s+(?P<target>delta\.`[^`]+`|[\w.]+)\s*(?P<rest>.*)$",
    re.IGNORECASE | re.DOTALL)

# `VACUUM tbl [RETAIN n HOURS] [DRY RUN]`
_VACUUM = re.compile(
    r"^\s*VACUUM\s+(?P<target>delta\.`[^`]+`|[\w.]+)"
    r"(?:\s+RETAIN\s+(?P<hours>[\d.]+)\s+HOURS?)?"
    r"(?P<dry>\s+DRY\s+RUN)?\s*;?\s*$",
    re.IGNORECASE | re.DOTALL)


class DeltaOpError(RuntimeError):
    """Raised for a matched statement we decline to execute — never for one we
    simply do not recognise, which is passed to Spark instead."""


def _table_uri(target: str, resolve):
    """`delta.\\`uri\\`` gives the location directly; a bare name has to be
    resolved through the session catalog, which is the caller's job."""
    if target.lower().startswith("delta.`"):
        return target[len("delta.`"):-1]
    return resolve(target)


def match(sql: str):
    """Return (kind, params) for a statement delta-rs should handle, else None."""
    text = sql.strip()
    m = _VACUUM.match(text)
    if m:
        return "vacuum", m.groupdict()
    m = _OPTIMIZE.match(text)
    if m:
        return "optimize", m.groupdict()
    return None


def execute(kind, params, resolve, storage_options=None):
    """Run the operation and return a short human-readable result line."""
    from deltalake import DeltaTable

    # Validate the statement *before* touching storage, so an unsupported
    # clause reports the clause rather than a missing-table error.
    if kind == "optimize":
        rest = (params.get("rest") or "").strip().rstrip(";").strip()
        if rest:
            # ZORDER and WHERE change *what* is compacted. Executing a bare
            # compaction while the user asked for something narrower would be a
            # silent semantic change, so refuse and say why.
            raise DeltaOpError(
                f"OPTIMIZE ... {rest.split()[0].upper()} is not supported by the "
                "delta-rs path this engine uses for OPTIMIZE. Run it on the JVM "
                "overlay (docs/engine-matrix.md), which supports the full syntax.")

    uri = _table_uri(params["target"], resolve)
    table = DeltaTable(uri, storage_options=storage_options or {})

    if kind == "optimize":
        metrics = table.optimize.compact()
        added = metrics.get("numFilesAdded", metrics.get("num_files_added", "?"))
        removed = metrics.get("numFilesRemoved", metrics.get("num_files_removed", "?"))
        return f"OPTIMIZE: compacted {removed} file(s) into {added} (delta-rs)"

    # vacuum
    # delta-rs takes whole hours; Spark's RETAIN accepts fractions, so round
    # *down* — retaining less than asked would delete files the user wanted
    # kept, which is the unrecoverable direction.
    hours = int(float(params["hours"])) if params.get("hours") else 168
    dry = bool(params.get("dry"))
    files = table.vacuum(retention_hours=hours, dry_run=dry,
                         enforce_retention_duration=False)
    verb = "would delete" if dry else "deleted"
    return f"VACUUM: {verb} {len(files)} file(s), retain {hours}h (delta-rs)"


def read_change_feed(spark, uri, starting_version=0, ending_version=None,
                     storage_options=None):
    """Change Data Feed read, returned as a Spark DataFrame.

    Sail rejects `option("readChangeFeed", "true")` outright, so this is exposed
    as a helper rather than by intercepting the DataFrameReader: silently
    rewriting a user's `spark.read` chain would hide which engine answered.
    Call it explicitly and the source of the data is obvious.

    The rows come from delta-rs and are handed back through `createDataFrame`,
    so the result is a normal Spark DataFrame — but note it is *materialised*,
    not a lazily-planned scan.
    """
    from deltalake import DeltaTable

    table = DeltaTable(uri, storage_options=storage_options or {})
    reader = table.load_cdf(starting_version=starting_version,
                            ending_version=ending_version)
    frame = reader.read_all().to_pandas()
    if frame.empty:
        raise DeltaOpError(
            "change feed is empty — enable it on the table first "
            "(delta.enableChangeDataFeed) and write at least one version")
    return spark.createDataFrame(frame)
