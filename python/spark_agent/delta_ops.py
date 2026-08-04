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

Credentials come from `storage.py`, resolved per statement — delta-rs reaches
OneLake on its own account because a Connect client cannot read back the bearer
the engine holds. See that module for why, and `docs/20-lakesail-engine.md`.
"""
import re
import sys

# Locations of tables the emulator itself registered, name -> Delta URI.
#
# A statement naming a registered table has to be resolved to a physical path
# before delta-rs can act on it. That used to be asked of the engine, with
# `DESCRIBE DETAIL <name>`, which Sail does not implement at all — DETAIL is not
# in its DESCRIBE grammar (see the ᶠ row in docs/engine-matrix.md). So every
# OPTIMIZE against a NAMED table degraded to skipped on Sail, silently losing
# compaction on every dbt-fabricspark model build, while the path-addressed form
# worked.
#
# Asking the engine was the weaker design regardless of Sail's grammar: the
# emulator WROTE these locations when it registered the tables, so querying them
# back is asking to be told something we already recorded. register_tables in
# the agent now calls remember() as it goes.
_LOCATIONS = {}


def remember(name, location, schema=None):
    """Record where a table the emulator registered actually lives.

    Both the bare and schema-qualified spellings are stored, because a client
    may address either — the agent registers into a schema AND mirrors into
    `default` so unqualified names resolve the way they do in a Fabric notebook.
    """
    if not name or not location:
        return
    _LOCATIONS[name.lower()] = location
    if schema:
        _LOCATIONS[f"{schema}.{name}".lower()] = location


# Locations of SCHEMAS the emulator registered (schema-enabled lakehouse
# folders reflected at session bind), lowercase name -> Tables/<schema> URI.
# CTAS interception needs them: a `CREATE TABLE schema.t AS SELECT` must land
# under the schema's real location, and only the emulator knows it.
_SCHEMA_LOCATIONS = {}


def remember_schema(name, location):
    """Record where a schema the emulator registered actually lives."""
    if name and location:
        _SCHEMA_LOCATIONS[name.lower()] = location.rstrip("/")


def known_schema_location(name):
    """The recorded location for schema `name`, or None."""
    if not name:
        return None
    return _SCHEMA_LOCATIONS.get(name.replace("`", "").strip().lower())


def forget_all():
    """Drop every recorded location. For tests that need a clean registry."""
    _LOCATIONS.clear()
    _SCHEMA_LOCATIONS.clear()


def known_location(name):
    """The recorded location for `name`, or None. Backticks and case ignored."""
    if not name:
        return None
    key = name.replace("`", "").strip().lower()
    if key in _LOCATIONS:
        return _LOCATIONS[key]
    # `catalog.schema.table` addressed against a two-part registration, or the
    # reverse — match on the last component rather than failing over spelling.
    tail = key.rsplit(".", 1)[-1]
    return _LOCATIONS.get(tail)

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

# `CREATE [OR REPLACE] TABLE schema.tbl [USING delta] AS <query>`.
#
# Sail executes CTAS happily but places the table in its OWN warehouse,
# ignoring the schema's registered LOCATION — so a notebook building gold
# tables goes green while the lakehouse stays empty, the false-green write
# the emulator exists to prevent. Only statements whose SCHEMA the emulator
# itself registered (with a location) are intercepted; everything else passes
# to the engine untouched.
_CTAS = re.compile(
    r"^\s*CREATE\s+(?P<replace>OR\s+REPLACE\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"
    r"(?P<target>[\w.`]+)\s+(?:USING\s+delta\s+)?AS\s+(?P<query>\(?\s*SELECT\b.+)$",
    re.IGNORECASE | re.DOTALL)

# The bounded MERGE shape an upsert notebook writes (Rosetta's silver layer is
# the motivating case):
#
#   MERGE INTO <target> [AS] t USING <source-name> [AS] s ON <cond>
#   [WHEN MATCHED [AND <cond>] THEN UPDATE SET <assignments>]
#   [WHEN NOT MATCHED THEN INSERT (<cols>) VALUES (<vals>)]
#
# Sail parses MERGE but its plan resolver fails on any Delta TARGET holding a
# date or timestamp column ("attribute #N is missing from the schema"),
# reproduced minimally and unchanged in pysail 0.7.0 — which makes every
# audit-columned table unmergeable, i.e. practically all of them. The source
# must be a bare view/table NAME: a subquery source does not match and falls
# through to the engine, honest failure included.
_MERGE = re.compile(
    r"^\s*MERGE\s+INTO\s+(?P<target>delta\.`[^`]+`|[\w.`]+)(?:\s+AS)?\s+(?P<talias>\w+)\s+"
    r"USING\s+(?P<source>[\w.]+)(?:\s+AS)?\s+(?P<salias>\w+)\s+"
    r"ON\s+(?P<on>.+?)\s+"
    r"(?:WHEN\s+MATCHED(?:\s+AND\s+(?P<mcond>.+?))?\s+THEN\s+UPDATE\s+SET\s+(?P<sets>.+?)\s*)?"
    r"(?:WHEN\s+NOT\s+MATCHED\s+THEN\s+INSERT\s*\((?P<icols>[^)]+)\)\s*VALUES\s*\((?P<ivals>[^)]+)\)\s*)?"
    r";?\s*$",
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
    m = _MERGE.match(text)
    if m:
        params = m.groupdict()
        # A MERGE with neither branch parsed is one this grammar did not really
        # understand (DELETE clauses, subquery source, ...): fall through.
        if params.get("sets") or params.get("icols"):
            return "merge", params
    m = _CTAS.match(text)
    if m:
        params = m.groupdict()
        target = params["target"].replace("`", "")
        # Only a two-part name whose schema the emulator registered is ours;
        # anything else is the engine's own business (its warehouse IS the
        # right place for a schema nobody gave a location).
        if "." in target and known_schema_location(target.split(".", 1)[0]):
            return "ctas", params
    return None


def execute_ctas(spark, original_sql, params, storage_options=None):
    """Run a CTAS whose schema has a registered location, landing it there.

    The SELECT runs on the ENGINE (arbitrary SQL is its job); only the write
    is redirected: DataFrame.save to the schema-location path, then a catalog
    registration so later statements resolve the name. Without this, Sail
    places the table in its own warehouse and the lakehouse silently stays
    empty.
    """
    target = params["target"].replace("`", "")
    schema, tbl = target.split(".", 1)
    loc = f"{known_schema_location(schema)}/{tbl}"
    replace = bool(params.get("replace"))

    df = original_sql(params["query"].strip().rstrip(";"))
    df.write.format("delta").mode("overwrite" if replace else "errorifexists").save(loc)
    if replace:
        try:
            original_sql(f"DROP TABLE IF EXISTS `{schema}`.`{tbl}`")
        except Exception:  # noqa: BLE001 - a stale entry must not block the re-register
            pass
    original_sql(f"CREATE TABLE IF NOT EXISTS `{schema}`.`{tbl}` USING delta LOCATION '{loc}'")
    remember(tbl, loc, schema)
    return f"CREATE TABLE: {schema}.{tbl} at its schema location (delta write + register)"


def _dequote(expr: str) -> str:
    """Backtick identifiers -> double-quoted, for delta-rs (DataFusion) SQL.

    Also load-bearing for CASE: DataFusion lowercases bare identifiers, so a
    column like `Strm` must arrive quoted or it will not resolve.
    """
    return re.sub(r"`([^`]+)`", r'"\1"', expr).strip()


def _split_top(text: str, sep: str = ","):
    """Split on `sep` outside quotes/parens — assignment lists may hold both."""
    parts, depth, buf, quote = [], 0, [], None
    for ch in text:
        if quote:
            buf.append(ch)
            if ch == quote:
                quote = None
            continue
        if ch in ("'", '"', "`"):
            quote = ch
            buf.append(ch)
        elif ch == "(":
            depth += 1
            buf.append(ch)
        elif ch == ")":
            depth -= 1
            buf.append(ch)
        elif ch == sep and depth == 0:
            parts.append("".join(buf))
            buf = []
        else:
            buf.append(ch)
    if buf:
        parts.append("".join(buf))
    return [p.strip() for p in parts if p.strip()]


def _plain_column(ref: str, alias: str) -> str:
    """`t.\\`col\\``/`t.col`/`\\`col\\`` -> the bare column name."""
    ref = ref.strip().strip("`\"")
    prefix = alias + "."
    if ref.lower().startswith(prefix.lower()):
        ref = ref[len(prefix):]
    return ref.strip().strip("`\"")


def execute_merge(spark, params, resolve, storage_options=None):
    """Run a bounded MERGE through delta-rs and return a result line.

    The engine cannot: Sail fails plan resolution for any Delta merge target
    holding a date/timestamp column. delta-rs performs the same upsert against
    the same table, so the observable outcome is real; the executor differs,
    which is this module's standing, documented divergence.

    The source is materialised from the session (`spark.table` -> Arrow):
    delta-rs cannot see Spark temp views, and Arrow preserves the date and
    timestamp types whose presence is the whole reason we are here.
    """
    from deltalake import DeltaTable

    talias, salias = params["talias"], params["salias"]
    uri = _table_uri(params["target"].replace("`", ""), resolve)
    source = spark.table(params["source"]).toArrow()

    merger = DeltaTable(uri, storage_options=_resolve_options(storage_options)).merge(
        source=source,
        predicate=_dequote(params["on"]),
        source_alias=salias,
        target_alias=talias,
    )
    if params.get("sets"):
        updates = {}
        for assign in _split_top(params["sets"]):
            lhs, _, rhs = assign.partition("=")
            if not rhs:
                raise DeltaOpError(f"MERGE: cannot parse assignment {assign!r}")
            updates[_plain_column(lhs, talias)] = _dequote(rhs)
        merger = merger.when_matched_update(
            updates=updates,
            predicate=_dequote(params["mcond"]) if params.get("mcond") else None,
        )
    if params.get("icols"):
        cols = [_plain_column(c, talias) for c in _split_top(params["icols"])]
        vals = [_dequote(v) for v in _split_top(params["ivals"])]
        if len(cols) != len(vals):
            raise DeltaOpError(
                f"MERGE: INSERT names {len(cols)} column(s) but {len(vals)} value(s)")
        merger = merger.when_not_matched_insert(updates=dict(zip(cols, vals)))

    metrics = merger.execute()
    upd = metrics.get("num_target_rows_updated", "?")
    ins = metrics.get("num_target_rows_inserted", "?")
    return f"MERGE: updated {upd} row(s), inserted {ins} (delta-rs)"


def _resolve_options(storage_options):
    """`storage_options` may be a dict or a zero-arg callable returning one.

    The callable form is what the agent uses: credentials are read at statement
    time, so a refreshed bearer is picked up without restarting the session.
    """
    if callable(storage_options):
        return storage_options() or {}
    return storage_options or {}


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
    table = DeltaTable(uri, storage_options=_resolve_options(storage_options))

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
    import pyarrow as pa
    from deltalake import DeltaTable

    table = DeltaTable(uri, storage_options=_resolve_options(storage_options))
    reader = table.load_cdf(starting_version=starting_version,
                            ending_version=ending_version)
    # deltalake 1.x returns an arro3 table, not a pyarrow one — it has no
    # .to_pandas(). Both sides speak the Arrow PyCapsule interface, so pa.table
    # converts without a copy through Python.
    frame = pa.table(reader.read_all()).to_pandas()
    if frame.empty:
        raise DeltaOpError(
            "change feed is empty — enable it on the table first "
            "(delta.enableChangeDataFeed) and write at least one version")
    return spark.createDataFrame(frame)


def install(spark, storage_options=None):
    """Wrap `spark.sql` so OPTIMIZE/VACUUM route here, and expose the CDF helper.

    Shared by the Livy agent and the engine-matrix probes so the matrix measures
    the *same* code path users get, rather than a re-implementation that could
    drift from it.

    `storage_options` defaults to `storage.options` — the callable, not its
    result — so every statement resolves credentials afresh from the agent's
    environment and a refreshed bearer needs no restart. Pass a dict to pin
    them, or `{}` to force the unauthenticated path (local tables). Where
    `storage` cannot be imported the default is `{}`, which keeps local-path
    tables working in a runtime that has no OneLake configured at all.

    Returns the original `sql` callable, which the caller needs both to restore
    it and to resolve catalog names.
    """
    if storage_options is None:
        try:
            import storage
            storage_options = storage.options
        except ImportError:  # pragma: no cover - runtime without the agent module
            storage_options = {}
    original_sql = spark.sql

    def resolve(name):
        """Find where a named table lives: what we recorded, else ask the engine.

        The recorded answer is preferred because the emulator wrote it. The
        engine fallback stays for tables this process did not register — a user
        CREATE TABLE in a notebook cell, or the engine-matrix probes, which
        register their own tables and never call remember().

        The fallback is LOUD on purpose. Everything that went wrong around this
        code went wrong quietly: a DESCRIBE returning zero rows with no error, a
        TCP fallback that reached a different server, a DSN that parsed into a
        wrong-but-valid config. A cache miss degrading silently to a route Sail
        does not implement would be the same mistake again, so it announces
        itself before it tries.
        """
        loc = known_location(name)
        if loc:
            return loc
        print(f"[delta_ops] no recorded location for {name!r}; falling back to "
              f"DESCRIBE DETAIL, which Sail does not implement — if this fails, "
              f"the table was not registered through the emulator",
              file=sys.stderr, flush=True)
        rows = original_sql(f"DESCRIBE DETAIL {name}").collect()
        if not rows:
            # Would have been an IndexError naming this file rather than the
            # cause. See the ᵉ row in docs/engine-matrix.md: an engine can answer
            # a DESCRIBE with the right schema and no rows, and raise nothing.
            raise DeltaOpError(
                f"cannot resolve {name!r} to a table location: the engine "
                f"answered DESCRIBE DETAIL with no rows. Address the table by "
                f"path, or register it through the emulator.")
        return rows[0]["location"]

    def sql(query, *args, **kwargs):
        matched = match(query) if isinstance(query, str) else None
        if matched is None:
            return original_sql(query, *args, **kwargs)
        kind, params = matched
        if kind == "merge":
            message = execute_merge(spark, params, resolve, storage_options)
        elif kind == "ctas":
            message = execute_ctas(spark, original_sql, params, storage_options)
        else:
            message = execute(kind, params, resolve, storage_options)
        # A DataFrame, so callers can .show()/.collect() as after a native
        # OPTIMIZE rather than getting None back.
        return spark.createDataFrame([(message,)], ["result"])

    def change_feed(uri, **kw):
        kw.setdefault("storage_options", storage_options)
        return read_change_feed(spark, uri, **kw)

    spark.sql = sql
    spark.delta_change_feed = change_feed
    return original_sql
