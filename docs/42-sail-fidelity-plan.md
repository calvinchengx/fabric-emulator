# 42 — Sail stays the default: how close to 100% that can get

**Status: the reachable bucket is closed; CDF is blocked on a decision (§3b).** `DESCRIBE TABLE` and
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

- **`CREATE TABLE` defaulting to Delta** — see below; this one is special.

**CDF was listed here and does not belong** — see §3b. An earlier revision of
this plan called it "bucket 3, cheap"; that was wrong, and finding out cost less
than building it would have.


### 3b. CDF: not reachable through this seam, and blocked on a decision

The seam wraps **`spark.sql` only**. The CDF probe — and real Fabric notebook
code — never touches SQL:

```python
spark.sql("SELECT 1 AS id").write.format("delta") \
     .option("delta.enableChangeDataFeed", "true").mode("overwrite").save(p)
spark.read.format("delta").option("readChangeFeed", "true").load(p)
```

Both halves go through the **DataFrame writer and reader**. Wrapping those is
technically precedented — `input_file.py` already patches `type(spark.read)` and
`_ConnectDataFrame.write` — so this is not a capability problem.

It is a **design** problem, and the design was already decided. From
`read_change_feed`'s own docstring:

> Sail rejects `option("readChangeFeed", "true")` outright, so this is exposed as
> a helper rather than by intercepting the DataFrameReader: silently rewriting a
> user's `spark.read` chain would hide which engine answered.

**Nothing is currently dishonest.** The write fails loudly
(`Unsupported table features required: [ChangeDataFeed]`) and the read has an
explicit helper. Neither fabricates a result. Closing the row buys *convenience*
and pays in transparency — which is why it is a decision rather than a task.

#### The case for reversing

Fabric has no `spark.delta_change_feed(...)`. Requiring an emulator-only API
means **the notebook you test is not the notebook you ship**, which is itself a
fidelity break — arguably a worse one than an executor mismatch, because it is
the exact class of divergence this emulator exists to catch.

#### The middle path

The recorded objection is to **silence**, not to interception. Intercepting the
standard API and *announcing* it — a stderr line, the executor named in the
session output, the divergence documented as `delta_ops` already documents
OPTIMIZE and VACUUM — would satisfy the stated principle rather than reverse it.
Gate it on `SPARK_REMOTE`, or the interception replaces a *working* native path
on the JVM overlay and makes the better engine worse.

#### The crux, still undecided

`read_change_feed` returns a **materialised** frame via `createDataFrame`, not a
lazily-planned scan. Backing `spark.read…load()` with it means `.explain()` shows
a LocalRelation, no predicate pushdown, no partition pruning, and the whole feed
through the driver. **No amount of announcing fixes that**, and it is what a user
hits at scale having written ordinary-looking Spark.

That is the open question: whether that consequence is acceptable. Until it is
answered, this stays unbuilt on purpose.

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

**The reachable bucket is now empty.** DESCRIBE and the explicit-LOCATION CTAS
are delivered, and `engine-matrix.md` is regenerated — the `CREATE TABLE` row
reads ❌ Sail / ✅ emulator / ❌ JVM, the one row where this emulator is more
faithful to Fabric than the JVM overlay.

What remains is not incremental work:

1. **CDF** — blocked on the decision in §3b, not on effort.
2. **Streaming sinks** — genuine Sail engine work, upstream or in the overlay.
3. **`sc` / `_jvm`** — never, while the transport is Spark Connect.
