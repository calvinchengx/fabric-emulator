# 42 — Sail stays the default: how close to 100% that can get

**Status: bounded SQL interception is in place; the generated matrix is the
ledger.** DESCRIBE, MERGE, Change Data Feed, `read.json(multiLine=True)`, and
durable streaming sinks (delta / parquet / memory) are ✅ on the middle column
of [engine-matrix.md](engine-matrix.md).
The residue is `sc` / `_jvm` and Java/Scala UDFs.

Sail is the default engine and stays that way. The question this answers is the
one that follows from it: **how much of the JVM's capability can Sail reach, and
what is the irreducible remainder?**

The honest answer is that **100% is not reachable**, for reasons no amount of
work on this seam removes — and that the reachable part is larger than a casual
read of [engine-matrix.md](engine-matrix.md) suggests, because several ❌ rows
are qualified artefacts rather than gaps. Read the **middle column** (Sail +
delta-rs): that is what a user of this emulator actually gets.

## Read the matrix carefully before believing it

The matrix is generated and its footnotes matter.

- **`MERGE INTO`**: `e2e/sail` proves the same MERGE succeeds on Sail against
  an `az://` OneLake URL — the path the emulator actually uses. The local-path
  probe fails on **bare Sail** (ᵃ), which is a storage-URL artefact, not a
  missing grammar. The **middle column** intercepts the probe shape (subquery
  source + `INSERT *`) through delta-rs. Named-source MERGE (the medallion
  shape) was already intercepted. `WHEN MATCHED THEN DELETE` still falls through.
- **Change Data Feed** (ᵇ): the **middle column** intercepts the notebook API
  (write enable + read), announces it, and materialises the feed. Bare Sail
  still cannot enable the feature; `e2e/sail` (no intercept) still shows the
  unwrapped read as inert. `.explain()` is a LocalRelation.
- **`OPTIMIZE` / `VACUUM` are closed** on the middle column by the delta-rs
  interception. They stay ❌ on bare Sail, which is correct.
- **`CREATE TABLE` with no `USING`** is ❌ Sail / ✅ emulator / ❌ JVM (ᵍ): the
  emulator honours an explicit LOCATION and writes Delta. That is the Fabric
  default, and the one row where this stack is more faithful than the JVM
  overlay.
- **`DESCRIBE TABLE` / `DESCRIBE DETAIL`** are Sail gaps that the interception
  answers from the Delta log, for tables whose LOCATION this process has
  recorded (`registerLakehouseTables`, or `CREATE TABLE … USING delta
  LOCATION` passing through `spark.sql`). A DESCRIBE of a name nobody recorded
  is still the engine's business.
- **`read.json(multiLine=True)`** (ʰ): the **middle column** wraps the named
  option, parses the file on the driver, and materialises. Bare Sail stays
  NDJSON-only.
- **Durable streaming sinks** (ⁱ): the **middle column** wraps
  `writeStream.format("delta"|"parquet"|"memory").start()`, pulls one
  micro-batch via `limit(n).collect()` (Sail returns real `rate` rows to the
  Connect client), and batch-writes. Announced; no checkpoint. Bare Sail
  still cannot land those sinks. Native kafka and `foreachBatch` on an engine
  stream fall through. The Fabric Eventstream notebook API is a named wrap
  (`eventstream_kafka.py`: emulator consume, Kafka schema, local
  `foreachBatch`) — never `rate`, and not a matrix cell.

## The taxonomy

Every remaining gap falls into one of four buckets, and only one is about the
engine's SQL grammar.

### 1. Spark Connect forbids it — unreachable for ANY Connect client

`sc` / RDD and `spark._jvm` answer
`[JVM_ATTRIBUTE_NOT_SUPPORTED] … is not supported in Spark Connect`. This is the
**protocol**, not Sail: Apache Spark's own Connect server refuses identically.
The JVM column is green only because that leg runs a **classic** session.

No engine change reaches these. A classic-session mode would, and that is what
the JVM overlay is. Java/Scala UDFs and `spark.jars` sit next to this: they
need Spark's own JVM classes, which is the overlay, not a delta-rs statement.

### 2. Lives inside the engine — not interceptable as a long-lived query

Streaming *execution* still lives inside Sail: `foreachBatch` is not a
callback into this process (measured: `start()` fails with
`missing argument: Python UDF output type`), and a continuous query with a
checkpoint is engine work.

What *is* interceptable is the **notebook sink API** for one bounded pull.
A streaming `rate` plan executed as a query returns rows to the Connect
client (`limit(n).collect()`, measured 2026-08-14). §3d wraps `start()` for
delta / parquet / memory on that fact. Console, native kafka, and
`foreachBatch` on an engine stream stay on the engine. Eventstream's
notebook API is a separate named wrap (`eventstream_kafka.py`).

### 3. Interceptable — bounded, path-scoped, session-free SQL

`OPTIMIZE`, `VACUUM`, named-source and subquery `MERGE`, LOCATION-bearing CTAS,
`DESCRIBE DETAIL`, and `DESCRIBE TABLE` (when the name is locatable) route this
way.

| Statement | Bare Sail | Emulator (middle column) |
|---|---|---|
| `OPTIMIZE` / `VACUUM` | ❌ not in the grammar | ✅ delta-rs |
| named-source / subquery `MERGE`, `INSERT *` | ❌ local-path planner | ✅ delta-rs |
| `CREATE TABLE` … `LOCATION` (no `USING`) | ❌ Hive / empty log | ✅ Delta at that path |
| `DESCRIBE DETAIL` | ❌ not in the grammar | ✅ from the Delta log, if located |
| `DESCRIBE TABLE` | ❌ right schema, **zero rows**, no error | ✅ real columns, if located |

### 3b. CDF: the notebook API, taken

The seam originally wrapped **`spark.sql` only**. The CDF probe — and real
Fabric notebook code — never touches SQL. That is now intercepted on the
DataFrame writer and reader, announced on stderr, gated by `install()` (Sail /
Connect only).

**Bare Sail** still fails the write loudly and leaves the read inert
(`e2e/sail` does not install this module). **The emulator** answers both
halves from delta-rs. The result is materialised via `createDataFrame`:
`.explain()` is a LocalRelation, no predicate pushdown. That consequence is
accepted and documented, not hidden.

```python
spark.sql("SELECT 1 AS id").write.format("delta") \
     .option("delta.enableChangeDataFeed", "true").mode("overwrite").save(p)
spark.read.format("delta").option("readChangeFeed", "true").load(p)
```

The recorded objection was to **silence**, not to interception. Announcing on
stderr, gating on `install()` / Connect, and documenting the LocalRelation is
the middle path. The helper `spark.delta_change_feed` remains for callers who
want to name the source.

### 3c. JSON `multiLine`: the notebook API, taken

Sail's JSON reader is NDJSON-only. `json(..., multiLine=True)` over a JSON
array is wrapped on the Connect reader, announced, and materialised via
`createDataFrame`. Plain `json()`, `multiLine=False`, a list of paths, and a
caller-supplied schema fall through. The fixture the probe writes is a Spark
text-writer directory; the wrap reads the part file and skips `_SUCCESS`.

`.explain()` is a LocalRelation. A large file lands on the driver. The
distributed workaround (`from_json` + `explode`) stays the right advice for
production-shaped data.

### 3d. Durable streaming sinks: one micro-batch from Sail

`foreachBatch` is not the seam (bucket 2). `limit(n).collect()` on a
streaming `rate` plan is: Sail returns real rows to the Connect client.
`stream_sinks.py` wraps `DataStreamWriter.start()` for `delta`, `parquet`,
and `memory` (a path, or a `queryName`), pulls `PULL_LIMIT` rows, strips
flow-event columns, and batch-writes (or `createOrReplaceTempView`).
Announced on stderr. The query object is a stand-in: `isActive` until
`stop()`, no checkpoint, one micro-batch even if the caller omitted
`trigger(once=True)`.

`console`, a kafka *sink*, Eventstream `eventstream.*` options on this
module, `foreachBatch` / `foreach`, and `outputMode("complete"|"update")`
fall through **this** module. Inventing `rate` rows, or mapping Kafka onto
`rate`, stays forbidden. OSS `format("kafka")` + bootstrap/subscribe and
the Fabric Eventstream notebook API live in `eventstream_kafka.py`
(driver or emulator consume, Kafka schema, LocalRelation on Sail).

Witnesses: `py:test_probe_shaped_delta_and_parquet_both_land_rate_rows`,
`py:test_patch_intercepts_delta_and_parquet_start_and_skips_the_engine`,
`ci:engine-matrix` after a `sail-delta` regen. `e2e/sail` does not install
this module; its "streaming execution unavailable" assertion stays the
bare-engine fact.

### 4. Neither engine — a Fabric property, not a Spark one

`CREATE TABLE` with no `USING` is ❌ on **both** bare Sail and the JVM (ᵍ): OSS
Spark defaults to Hive, Fabric defaults to Delta. The interception honours an
explicit LOCATION and writes Delta — more faithful to Fabric than the JVM
overlay. It does not change Sail's default for a CREATE with no LOCATION.

## So: what is the ceiling

| Bucket | Reachable with Sail default? |
|---|---|
| Bounded SQL (bucket 3) | **Yes**, for statements the matcher locates |
| Fabric-vs-OSS semantics (bucket 4) | **Yes**, for LOCATION-bearing CREATE |
| Streaming sinks (bucket 2 / §3d) | **Yes**, one announced micro-batch; not a checkpointed query |
| `sc` / `_jvm` / Java/Scala UDFs (bucket 1) | **Never** on Spark Connect |
| CDF notebook API (§3b) | **Yes**, materialised LocalRelation, announced |
| JSON `multiLine` (§3c) | **Yes**, materialised LocalRelation, announced |
| OSS `format("kafka")` + bootstrap/subscribe | **Yes**, driver consume → Kafka-schema LocalRelation on Sail |

That is the answer to "100% if possible": no. The JVM overlay stays as the
documented route for the irreducible set. `make up-jvm` swaps the engine; the
cost is 943 MB → 2.1 GB, seconds → minutes, 125 → 78,040 log lines, which is
why it is not the default and should not become one.

## The rule this work must not break

From `delta_ops.py`, and it governs every future addition here:

> Scope is deliberately narrow. Anything not matched here goes to Spark
> untouched: a shim that guesses would be worse than the gap.

Every interception is also a **documented divergence**: the emulator performs
these, not the Spark engine, so someone watching Spark sees no job. The
observable outcome on the table is real; the executor differs. That trade is
worth making for a bounded statement and is not worth making for anything that
requires guessing.

Recording `CREATE TABLE … USING delta LOCATION '…'` is not a guess: the
statement named the path. Inventing a LOCATION for a name nobody registered
would be.

## Closing more middle-column cells

The generated matrix is [engine-matrix.md](engine-matrix.md). "Closed by the
emulator" means the **middle column** goes ✅ after a `sail-delta` regen — not
a hand-edit, not a helper the notebook does not call.

Today that column is **23 / 25**. Two stay red.

| Probe | Closable on this seam? | Why |
|---|---|---|
| `MERGE INTO` registered (local path) | **Done** | Subquery source + `INSERT *` intercept |
| `MERGE INTO delta.\`path\`` | **Done** | Same change; path URI is taken from the statement |
| Change Data Feed | **Done** | Writer + reader wrapped, announced, materialised |
| `read.json(multiLine=True)` | **Done** | Named option wrapped, announced, materialised |
| Streaming sinks (memory / parquet / delta) | **Done** | One `limit(n).collect()` + batch write; announced; no checkpoint |
| `sc` / `_jvm` | **No** | Spark Connect protocol. The Livy RDD facade is a measured subset, and the matrix probe does not install it — painting that cell green would measure the facade, not Spark |
| Java/Scala UDFs / `spark.jars` | **No** | Need Spark's JVM classloader. Overlay only |

Ceiling on this table: **23 / 25**. The last two (`sc` + `_jvm`) are why the JVM overlay exists for the Connect protocol gap. Continuous checkpointed streaming, native kafka, and `foreachBatch` on an engine stream are still engine or overlay work. The Fabric Eventstream notebook API is `eventstream_kafka.py`, not a matrix cell.

The rule does not change: a shim that guesses is worse than the gap. Each step
below is a shape the statement (or the DataFrame options) named, executed the
way CTAS already does — engine for the query, delta-rs for the table.

### Step 1 — MERGE: the probe shape (two cells) — done

Witnesses: `py:test_probe_shaped_merge_upserts_a_real_delta_log` (a real
`_delta_log` on disk), `ci:engine-matrix` (the two generated rows), `ci:sail`
(the same SQL against `az://` on bare Sail — a different fact: the engine
can MERGE; the shim is what closes the local-path cells).

`execute_merge` materialises a source to Arrow and upserts through delta-rs.
Named table/view via `spark.table`; subquery via `spark.sql` (SELECT on the
engine, same split as CTAS). `INSERT *` is `when_not_matched_insert_all`.

Still out of scope, still fall through: `WHEN MATCHED THEN DELETE`, extra
clauses the regex does not parse. Refuse those; do not silently drop them.

### Step 2 — CDF notebook API — done

Witnesses: `py:test_probe_shaped_cdf_write_and_read_carry_change_type` (real
log: overwrite+append+read with `_change_type`), `ci:engine-matrix` (the
generated row), `ci:livy-native` (named helper against OneLake).

`e2e/sail` does **not** install this module; its inert-read assertion stays
as the bare-engine fact. The notebook path is the Livy agent.

Writer: `delta.enableChangeDataFeed` on a Delta save, and later appends to a
table that already has the feature. Reader: `readChangeFeed`. Both announce.
`partitionBy` is not intercepted. Spark Connect rejects `uint64`; delta-rs
types `_commit_version` that way, so the feed is cast to signed integers
before `createDataFrame`
(`test_read_change_feed_casts_uint64_so_spark_connect_can_ingest_it`).

### Step 3 — JSON `multiLine` notebook API — done

Witnesses: `py:test_probe_shaped_spark_text_dir_yields_two_objects` (the
engine-matrix fixture: Spark text-writer directory, `_SUCCESS` skipped),
`py:test_patch_intercepts_keyword_multiline_and_skips_the_engine` (the wrap),
`ci:engine-matrix` (the generated row).

`e2e/sail` does **not** install this module; bare Sail stays NDJSON-only.
Plain `json()`, `multiLine=False`, a list of paths, and a schema fall through.
`partitionBy` is not a reader option here.

### Step 4 — durable streaming sinks — done

Witnesses: `py:test_probe_shaped_delta_and_parquet_both_land_rate_rows`,
`py:test_patch_intercepts_delta_and_parquet_start_and_skips_the_engine`
(console / kafka / foreachBatch on an engine stream fall through),
`ci:engine-matrix` after a `sail-delta` regen.

### Do not do

- **Map Eventstream `format("kafka")` onto `rate`, or invent `rate` rows in
  delta-rs.** The collect wrap pulls Sail's own source; guessing a source
  paints the cell green for the wrong schema.
- **Treat `foreachBatch` as a Livy callback** for arbitrary streams. It
  pickles to the server and Sail rejects `start()`. Eventstream's wrap runs
  the callback locally for that named source only.
- **Install the RDD facade into the sail-delta probe** to turn `sc` / `_jvm`
  green. The middle column is "delta-rs interception", not "full Livy agent".
  The facade is documented in [20-lakesail-engine.md](20-lakesail-engine.md)
  and [50-rdd-usage-capture.md](50-rdd-usage-capture.md); it is not Spark.
- **Java/Scala UDFs / `spark.jars`.** Overlay.
- **Hand-editing [engine-matrix.md](engine-matrix.md).**
- **Growing the matcher to guess** a MERGE it did not parse (`DELETE` dropped
  on the floor, subquery half-consumed, INSERT * on a source it did not load).

### Process, every step

1. Red test for the new grammar or wrap (unit), then the existing engine-matrix
   probe as the acceptance test.
2. Implement only the named shape.
3. `python3 e2e/engine-matrix/run.py --engine sail-delta` — CI `--check` will
   demand the committed markdown match.
4. One capability per PR. Two MERGE rows are one capability.

## Next

1. **`sc` / `_jvm`, Java/Scala UDFs, checkpointed streaming** —
   not this seam. The ceiling on the generated table is 23/25.
