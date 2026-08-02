# examples

Working code that *uses* the emulator, kept deliberately apart from the code
that *is* the emulator.

Four medallions, on two axes. **Across:** one source system, or three that
share no key. **Down:** which engine builds the star — `dbt-fabric` over TDS
into a **Warehouse**, or `dbt-fabricspark` over Livy into the **Lakehouse**.
Fabric offers both and real shops pick one, so both are shown building the same
data from the same fixtures.

| Example | Sources | Gold built by | CI witness |
|---|---|---|---|
| [`medallion-pyspark/`](medallion-pyspark/) | one | `dbt-fabric` → Warehouse | [`e2e/medallion`](../e2e/medallion/) |
| [`medallion-advanced-pyspark/`](medallion-advanced-pyspark/) | **three** | `dbt-fabric` → Warehouse | [`e2e/medallion-advanced`](../e2e/medallion-advanced/) |
| [`medallion-dbt-fabricspark/`](medallion-dbt-fabricspark/) | one | `dbt-fabricspark` → Lakehouse | [`e2e/medallion-dbt-fabricspark`](../e2e/medallion-dbt-fabricspark/) |
| [`medallion-advanced-dbt-fabricspark/`](medallion-advanced-dbt-fabricspark/) | **three** | `dbt-fabricspark` → Lakehouse | [`e2e/medallion-advanced-dbt-fabricspark`](../e2e/medallion-advanced-dbt-fabricspark/) |

Start with [`medallion-pyspark/`](medallion-pyspark/) — it is the code
[docs/28](../docs/28-tutorial-end-to-end.md) walks through. Go to
[`medallion-advanced-pyspark/`](medallion-advanced-pyspark/) for the problems a
second and third source create: identity that must be *resolved* rather than
joined, change over time, and conformance that is real.

The `contoso-fixtures*` directories are not examples. They are the **shared
seeded generators** every example builds from — one copy, so two examples can
never assert against different data.

## The conventions

**One directory per example, paired 1:1 with an `e2e/` harness of the same
name.** The harness owns the compose file and the CI plumbing; the example owns
every line of pipeline code. The harness runs the example *unmodified* — only
endpoints differ, passed as environment variables the example already reads. So
nothing can pass in CI that would fail for a reader typing the steps by hand.

**Each example owns its `pyproject.toml` and `uv.lock`.** Two reasons. It can be
copied out of this repo and run anywhere — `cp -r examples/medallion-pyspark ~/mine &&
cd ~/mine && uv sync && uv run python pipeline.py`. And its dependencies
(pandas, dbt, pyodbc, …) never enter the emulator's own dependency graph, which
stays about building and testing the emulator.

**Examples do not share helper modules.** Each one carries its own `common.py`
even where that duplicates another. Copy-paste independence is the property that
makes an example useful; DRY across examples would trade it away for nothing a
reader benefits from.

**Every example is executable end to end and asserts its own results.** An
example that merely *looks* right is a liability — these fail loudly instead.

## Running one

Start the family from the repo root, then follow the example's README:

```sh
docker compose up -d
cd examples/medallion-pyspark && uv sync && uv run python pipeline.py
```

Or run its CI harness, which does the same thing in containers:

```sh
python3 e2e/medallion/run.py
```
