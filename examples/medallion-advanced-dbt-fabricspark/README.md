# Example: the advanced medallion, silver built by dbt

Three source systems, resolved into one customer identity, joined into a star in
the Warehouse, and served to Power BI — the same 22 steps as
[`../medallion-advanced-pyspark`](../medallion-advanced-pyspark/), with **one**
difference: `bronze → silver` is **Microsoft's `dbt-fabricspark`** over the
emulator's Livy High-Concurrency surface, with **Sail** behind it, instead of
imperative PySpark.

```
dbt-fabricspark → fabric-emulator (Fabric REST + Livy HC) → agent → Sail
```

```sh
docker compose up -d          # from the repo root
uv sync --frozen && uv run python pipeline.py
```

Open <https://localhost:9443/#flow> while it runs.

## What differs, and what deliberately does not

Exactly one step changes. Everything else — provisioning, the Key Vault secret,
landing, bronze, the notebook run, identity resolution, the star, the contracts,
the semantic model — is the same code, and that is enforced rather than
intended: [`scripts/check_example_parity.py`](../../scripts/check_example_parity.py)
fails CI if the two examples drift anywhere but the files listed below.

| | `../medallion-advanced-pyspark` | here |
|---|---|---|
| bronze → silver | PySpark DataFrames | dbt models, Spark SQL |
| transport | Spark Connect | Livy HC (REST) |
| compute | Sail (Rust Spark Connect, no JVM) | Sail — the same engine |
| silver → gold | `dbt-fabric` over TDS | `dbt-fabric` over TDS — **identical** |
| gold lives in | a Warehouse item | a Warehouse item — **identical** |
| `reflect` | twice | twice — **identical** |

**Gold is a Warehouse in both, and that is not an oversight.** `dbt-fabricspark`
materialises into a Lakehouse and has no path to a Warehouse, so an example that
built gold with it would be demonstrating something Fabric cannot do. The engine
choice a Fabric team actually makes is confined to the Lakehouse-to-Lakehouse
half of the medallion, and this example is confined the same way.

The silver assertions are the **same oracle**, not a relaxed one — the same
`EXPECTED_*` values from [`../contoso-fixtures`](../contoso-fixtures/) that the
PySpark example asserts against. Two engines asked for the same transform must
produce the same answer, or comparing them measures nothing.

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
model. Measured on both engines, so it is not a Sail workaround waiting for a
better one. Do not delete it because the engine improved.

**The models ask for their column list with `select … limit 0`,** not
`DESCRIBE`. Silver is the conformed customer-360 and stays ~100 columns wide, so
the projection is generated rather than typed out — and `adapter.get_columns_in_relation`
issues `DESCRIBE`, which returns the right schema and **zero rows** here. The
adapter reads that as "no columns", the model compiles to `select` followed by
`from`, and it fails much later with a message naming neither `DESCRIBE` nor the
empty list. A `limit 0` result carries the schema in its envelope instead.

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

## How big is it

Identical to the PySpark example, because the fixtures and every step but one
are identical — see
[its README](../medallion-advanced-pyspark/README.md#how-big-is-it) for the
measured feed sizes, table and column counts, and row counts. They are not
repeated here on purpose: two copies of the same numbers drift, and the one that
is wrong is never the one you are reading.

## The steps

`pipeline.py` runs 22 steps in order and stops at the first failure — the same
list, in the same order, as the PySpark example
([its table](../medallion-advanced-pyspark/README.md#the-steps)). Only step 6's
label differs, naming this engine. The step *sequence* is compared across both
examples by the parity check, so this claim is enforced too.

## What this example does not have

**No `compare.py`.** The two-engine equivalence claim — *the adapter choice does
not change the answer* — is made by the **simple** pair, whose
[`medallion-dbt-fabricspark/compare.py`](../medallion-dbt-fabricspark/compare.py)
runs in CI as `Both silver builds agree, row for row`. No equivalent exists for
the advanced pair yet, so this example asserts its own results against the
fixtures but is not diffed against its sibling's output.

## Requirements

- **Docker** for the emulator family (`docker compose up -d` at the repo root)
- **Microsoft ODBC Driver 18** for the `dbt-fabric` and TDS steps
  (macOS: `brew tap microsoft/mssql-release && brew install msodbcsql18`)
- **uv** for this example's own locked dependencies

> **Running from a git checkout rather than a release?** `docker compose up`
> starts the **published** image, which can lag `main`. These steps assert
> against the emulator's current behaviour, so a stale image fails in ways that
> look like example bugs. Build from your checkout:
>
> ```sh
> docker build -t ghcr.io/calvinchengx/fabric-emulator:dev .
> FABRIC_EMULATOR_VERSION=dev docker compose up -d
> ```

## Configuration and re-running

Endpoints default to the local stack and are all overridable — that is how the
CI harness ([`e2e/medallion-advanced-dbt-fabricspark`](../../e2e/medallion-advanced-dbt-fabricspark/))
points this same code at compose service names. See
[`../medallion-pyspark/README.md`](../medallion-pyspark/README.md#configuration)
for the full table; this example reads the same variables.

`provision.py` fails if the workspace already exists, since display names are
unique. Reset with `docker compose down -v && docker compose up -d`.
