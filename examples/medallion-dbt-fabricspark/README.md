# Example: the medallion, silver built by dbt

The same medallion as [`../medallion-pyspark`](../medallion-pyspark/), diverging
at **silver**: this one builds `bronze → silver` with **Microsoft's
`dbt-fabricspark`** over the emulator's Livy High-Concurrency surface, with
**Sail** behind it, where the sibling uses imperative PySpark.

```
dbt-fabricspark → fabric-emulator (Fabric REST + Livy HC) → agent → Sail
```

Fabric offers both. Real teams pick one, or run both, and the choice is
consequential — but an example that shows only one path presents it as *the*
path. This exists so the choice is demonstrated rather than assumed.

## What actually differs

Exactly one step. Provisioning, the Key Vault secret, landing, bronze, the
notebook run, `reflect`, gold, the DQ gate, the semantic model and the lineage
assertions are all the **same files**, byte for byte.

| | `../medallion-pyspark` | here |
|---|---|---|
| bronze → silver | PySpark DataFrames | dbt models, Spark SQL |
| transport | Spark Connect | Livy HC (REST) |
| compute | Sail (Rust Spark Connect, no JVM) | Sail — the same engine |
| silver → gold | `dbt-fabric` over TDS | `dbt-fabric` over TDS — **identical** |
| gold lives in | a **Warehouse** item | a **Warehouse** item — **identical** |
| `reflect` step | yes | yes — **identical** |
| steps | 11 | 12 — the extra one is `compare` |

**Gold is not the fork, and cannot be.** `dbt-fabricspark` materialises into a
Lakehouse and has no path to a Warehouse, so an example that built gold with it
would be demonstrating something Fabric cannot do. The genuine fork is
bronze → silver, which is Lakehouse-to-Lakehouse and where a Fabric team really
does choose. Because gold is shared, `reflect.py` is needed here exactly as it
is in the sibling: silver's Delta must reach the lakehouse SQL endpoint before
`dbt-fabric` can read it by three-part name.

The silver assertions are the **same oracle**, not a relaxed one — the same
`EXPECTED_*` values from [`../contoso-fixtures`](../contoso-fixtures/). Two
engines asked for the same transform must produce the same answer, or the
comparison below measures nothing.

## Run it

```sh
docker compose up -d          # from the repo root — no overlay needed
```

The comparison needs both halves, so run the sibling first:

```sh
cd ../medallion-pyspark && uv sync --frozen && uv run python pipeline.py
```

then here:

```sh
uv sync --frozen && uv run python pipeline.py
```

Running this example alone is fine — `compare.py` skips with a nudge rather than
failing. See the caveat about that skip below.

## The comparison

`compare.py` reads the `silver_summary.json` each example writes and reports
three things:

1. **Equivalence** — both silver builds have identical row counts. This is the
   claim worth making: *the tool choice does not change the answer.* It is
   asserted, so a divergence fails the run.
2. **Dialect divergence — and there is none.** Both summaries record an empty
   `dialect_adaptations`, and the comparison asserts that: neither path needed a
   statement rewritten on the wire. This used to be the headline asymmetry
   between the two examples, back when this one built *gold*. It no longer is,
   because gold is now dbt-fabric over TDS in **both** — so the T-SQL rewrites
   (`CREATE TABLE … AS SELECT` becoming `SELECT … INTO`, nested CTEs flattened;
   [docs/29](../../docs/29-tsql-parity.md), T6 and T8) are a cost both examples
   pay, not a reason to prefer one.
3. **Wall-clock** — and read the caveat, which is not boilerplate.

> **The timings are not a Fabric benchmark.** Both halves run against the same
> Sail container on your laptop; Fabric Spark is a managed pool and is not
> represented here. A ratio measured on this stack says nothing about which is
> faster on Fabric. The numbers are good for one thing: knowing what each path
> costs you locally, in a loop you run all day.

**On the skip.** The counterpart summary is produced by a different example, so
in CI — where the two are separate matrix legs on separate runners — neither leg
can see the other's output and `compare.py` skips. That skip is only honest
because the assertion still runs somewhere: the `medallion-compare` job in
[`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) collects both
summaries as artifacts and runs this file with both present, reported as *Both
silver builds agree, row for row*. Remove that job and the skip becomes an
assertion nobody makes — so remove them together or not at all.

## The dbt project

```
silver_dbt/
  models/silver_customers.sql          dedupe, lower-case email, conform country
  models/silver_orders.sql             latest event per order, quarantine the bad
  models/silver_quarantine_orders.sql  the rows silver refused, kept not dropped
  macros/file_format.sql               forces `using delta` — see below
```

Two things in there look odd and both are load-bearing.

**`macros/file_format.sql` overrides the adapter.** `dbt-fabricspark`'s own
`file_format_clause` treats `delta` as the one value that emits *nothing*, which
is correct for Fabric — Delta is the default there and saying so is redundant —
and correct nowhere else. Without a `USING` clause, Spark creates a **Hive**
table: dbt reports success, rows are queryable through the catalog, and OneLake
never receives a `_delta_log`, so nothing reading the lakehouse can see the
model. Measured on both Sail and the JVM overlay, so it is not a Sail workaround
waiting for a better engine. Do not delete it because the engine improved.

**The models ask for their column list with `select … limit 0`,** not
`DESCRIBE`. Silver is the conformed customer-360 and stays ~100 columns wide, so
the projection is generated rather than typed out — and
`adapter.get_columns_in_relation` issues `DESCRIBE`, which returns the right
schema and **zero rows** here. The adapter reads that as "no columns", the model
compiles to `select` followed by `from`, and it fails much later with a message
naming neither `DESCRIBE` nor the empty list.

## What you will see in the log that is not an error

- **`OPTIMIZE failed and was skipped; the model itself succeeded.`** `OPTIMIZE`
  is a Delta *extension* registered in JVM Spark by delta-spark, not part of
  Spark SQL, so it is not in Sail's grammar. The emulator routes it to delta-rs
  ([docs/engine-matrix.md](../../docs/engine-matrix.md) marks it ✅ for
  "Sail + delta-rs"), but `dbt-fabricspark` emits it as raw SQL.
- **A two-CTE shape in `silver_quarantine_orders.sql`** where one CTE would read
  more naturally. A `row_number()` CTE filtered on its own helper column fails on
  the Livy path; the same shape passes against Sail over Spark Connect, so the
  fault is on the path or in dbt's generated SQL and **not** in Sail's SQL
  support. The rewrite is portable and costs nothing.

## A note on the Livy agent

This example runs on the standard stack with no overlay, which was not true
before it existed. The shipped agent ([`python/spark_agent/agent.py`](../../python/spark_agent/agent.py))
was a **Python** REPL — right for notebooks, useless to a SQL client — so
`dbt-fabricspark` had nothing to talk to, and the dbt e2e carried its own
SQL-only agent.

Two things changed. The emulator's Livy surface now forwards a statement's
`kind` (it was decoding only `code` and dropping it), and the agent dispatches
on it: `sql` runs Spark SQL and returns the structured `application/json`
envelope a result set needs, anything else runs Python and returns REPL text.
One agent, both consumers.

## Requirements

- **Docker** for the emulator family (`docker compose up -d` at the repo root)
- **Microsoft ODBC Driver 18** for the `dbt-fabric` and TDS steps
  (macOS: `brew tap microsoft/mssql-release && brew install msodbcsql18`)
- **uv** for this example's own locked dependencies

## Configuration and re-running

Endpoints default to the local stack and are all overridable — that is how the
CI harness points this same code at compose service names. See
[`../medallion-pyspark/README.md`](../medallion-pyspark/README.md#configuration)
for the full table; this example reads the same variables.

`provision.py` fails if the workspace already exists, since display names are
unique. Reset with `docker compose down -v && docker compose up -d`.

Three sources instead of one, with identity resolved across them, is
[`../medallion-advanced-dbt-fabricspark`](../medallion-advanced-dbt-fabricspark/).
