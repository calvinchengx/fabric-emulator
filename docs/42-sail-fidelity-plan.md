# 42 — Sail stays the default: how close to 100% that can get

**Status: taxonomy measured, first increment delivered.** `DESCRIBE TABLE` and
`DESCRIBE DETAIL` now answer from the Delta log
([delta_ops.py](../python/spark_agent/delta_ops.py)).

Sail is the default engine and stays that way. The question this answers is the
one that follows from it: **how much of the JVM's capability can Sail reach, and
what is the irreducible remainder?**

The honest answer is that **100% is not reachable**, for two reasons that no
amount of work removes — and that the reachable part is larger than
[engine-matrix.md](engine-matrix.md) makes it look, because several ❌ rows are
qualified artefacts rather than gaps.

## Read the matrix carefully before believing it

The matrix is generated and its footnotes matter. Three of its ❌ rows are not
what they appear:

- **`MERGE INTO` is not a Sail gap** (ᵃ). `e2e/sail` proves the same MERGE
  succeeds on Sail against an `az://` OneLake URL — the path the emulator
  actually uses. Only the *local-path* probe fails.
- **Change Data Feed fails on the WRITE, not the read** (ᵇ). Sail's writer
  cannot enable the table feature; the read side is real and verified in
  `e2e/livy` against a delta-rs-written table.
- **`OPTIMIZE` / `VACUUM` are already closed** by the delta-rs interception —
  the matrix's middle column, which is what a user actually gets.

## The taxonomy

Every remaining gap falls into one of four buckets, and only one is about the
engine's capability.

### 1. Spark Connect forbids it — unreachable for ANY client

`sc` / RDD and `spark._jvm` answer
`[JVM_ATTRIBUTE_NOT_SUPPORTED] … is not supported in Spark Connect`. This is the
**protocol**, not Sail: Apache Spark's own Connect server refuses identically.
The JVM column is green only because that leg runs a **classic** session.

No engine change reaches these. A classic-session mode would, and that is what
the JVM overlay is.

### 2. Lives inside the engine — not interceptable

Streaming sinks (delta / parquet / memory). `delta_ops.py` states the rule that
decides this, and it is the right one:

> each is a **bounded statement that starts and finishes against a table path**,
> carrying no Spark session state. That makes them interceptable — a streaming
> query, which lives inside the engine, is not.

A structured streaming query is a long-lived object owned by the engine. There
is no seam to put delta-rs behind. This is genuine Sail engine work, upstream or
in the overlay.

### 3. Interceptable — the reachable part

Bounded, path-scoped, session-free statements. `OPTIMIZE`, `VACUUM`, `MERGE` and
CTAS already route this way. Added here:

| Statement | Was | Now |
|---|---|---|
| `DESCRIBE DETAIL` | ❌ absent from Sail's grammar | answered from the Delta log |
| `DESCRIBE TABLE` | ❌ right schema, **zero rows**, no error | real column list, Spark type names |

Still reachable and not yet built:

- **CDF write** — enable the table feature through delta-rs so a Sail-authored
  table is CDF-readable. The read path already works.
- **`CREATE TABLE` defaulting to Delta** — see below; this one is special.

### 4. Neither engine — a Fabric property, not a Spark one

`CREATE TABLE` with no `USING` is ❌ on **both** Sail and the JVM (ᵍ): OSS Spark
defaults to Hive, Fabric defaults to Delta. Closing it on the interception path
would make the emulator **more faithful to Fabric than the JVM overlay is** —
the one place where the "weaker" engine can be the more correct one.

## So: what is the ceiling

| Bucket | Reachable with Sail default? |
|---|---|
| Bounded statements (bucket 3) | **Yes** — this is where the work is |
| Fabric-vs-OSS semantics (bucket 4) | **Yes**, and it beats the JVM |
| Streaming sinks (bucket 2) | **No** — engine work or the overlay |
| `sc` / `_jvm` (bucket 1) | **Never** — the protocol forbids it |

Two buckets close, two do not. That is the answer to "100% if possible": the
answer is no, and the residue is exactly `sc`/`_jvm` and streaming sinks.

**Which is why the JVM overlay stays** — not as an admission of defeat but as
the documented route for the irreducible two. `make up-jvm` swaps the engine;
the cost is 943 MB → 2.1 GB, seconds → minutes, 125 → 78,040 log lines, which is
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

## Next

1. **CDF write** — bucket 3, closes a measured ❌.
2. **`CREATE TABLE` → Delta by default** — bucket 4, makes Sail beat the JVM on
   a Fabric property.
3. Regenerate `engine-matrix.md` so the middle column reflects all of it; CI
   already diffs that file, so the matrix cannot drift from the code.
