# 53 — dbt_expectations: measured on both of this repo's dbt adapters

**The question.** A data product built here runs dbt twice: `dbt-fabricspark`
builds bronze → silver in the Lakehouse, and `dbt-fabric` builds silver → gold in
the Warehouse. Is there a Great Expectations library to reach for?

**The short answer.** There is no GE-for-dbt library. There is a *port* of GE's
vocabulary into dbt macros — [`dbt_expectations`](https://github.com/metaplane/dbt-expectations),
maintained by metaplane since calogica went quiet (0.10.10, Dec 2025 vs 0.10.4,
Sep 2024). It contains no GE code and no Python: it is Jinja and SQL. Great
Expectations itself declares no dbt integration — `great_expectations` 1.20.0 has
no dbt dependency or extra, and PyPI has nothing at `dbt-expectations`,
`great-expectations-dbt` or `dbt-great-expectations`.

**It works completely on Spark and barely at all on the Warehouse**, and the
reason is not the one you would predict from its adapter list.

## Measured, not predicted

Spiked against `examples/medallion-dbt-fabricspark` on the real stack: ten tests
on `silver_orders` through dbt-fabricspark, eight on `fct_daily_revenue` through
dbt-fabric.

| test | dbt-fabricspark | dbt-fabric (T-SQL) |
|---|---|---|
| `expect_compound_columns_to_be_unique` | PASS | **PASS** |
| `expect_column_values_to_be_in_set` | PASS | **PASS** |
| `expect_column_values_to_not_be_null` | PASS | ERROR — `Incorrect syntax near the keyword 'is'` |
| `expect_table_row_count_to_be_between` | PASS | ERROR — `Incorrect syntax near '='` |
| `expect_column_values_to_be_between` | PASS | ERROR — `Incorrect syntax near '='` |
| `expect_column_mean_to_be_between` | PASS | ERROR — `Incorrect syntax near '='` |
| `expect_column_value_lengths_to_equal` | PASS | ERROR — `Incorrect syntax near '='` |
| `expect_column_values_to_match_regex` | PASS | ERROR — `'regexp_instr' is not a recognized built-in function name` |
| `expect_column_values_to_be_unique` | PASS | not run |
| `expect_column_values_to_be_between` (quantity) | PASS | not run |

Silver: `Done. PASS=13 WARN=0 ERROR=0 SKIP=0 TOTAL=13` — ten expectations and
three models. Gold: **2 of 8**.

## Why gold fails, and why the obvious diagnosis is the minor one

The package dispatches per adapter for 24 macros, and ships `default__` for all
24 plus `spark__` for four. There is no `sqlserver__` and no `fabric__`, so the
prediction is "regex breaks on the Warehouse". That prediction is right and
accounts for **one** of the six failures.

The other five are structural. `dbt_expectations` compiles nearly every test
through its `truth_expression` shape, which **selects a boolean as a column**:

```sql
with grouped_expression as (
    select revenue is not null as expression
    from [<db>].[dbo].[fct_daily_revenue]
)
```

T-SQL has no boolean type and cannot select a comparison as a value, so this
fails wherever it appears — independently of dialect functions, and independently
of anything this emulator does. The two tests that pass are the two that never
build that shape: `expect_column_values_to_be_in_set` compiles to a `not in`, and
`expect_compound_columns_to_be_unique` to `group by … having count(*) > 1`.

So the boundary is not "avoid the regex tests on gold". It is that the package's
core pattern is not T-SQL, and adopting it there would mean upstream
`sqlserver__` overrides for `truth_expression` / `expression_is_true`, not a
list of tests to avoid.

## What to use instead, per layer

* **silver (dbt-fabricspark)** — `dbt_expectations` is usable in full.
* **gold (dbt-fabric)** — dbt's builtins, which `examples/medallion-dbt-fabricspark/gold/models/schema.yml`
  already uses (`unique`, `not_null`, `accepted_values`, `relationships`), plus
  singular tests for anything shaped. Note those builtins already exercise a
  T-SQL edge this emulator closes deliberately: `accepted_values` and
  `relationships` compile to nested CTEs that Fabric Warehouse runs and vanilla
  SQL Server rejects (see [29-tsql-parity.md](29-tsql-parity.md), T6).

## Two obstacles to adopting it, recorded so the next attempt does not rediscover them

1. **`dbt deps` cannot run inside the example harness.** The harness points
   `REQUESTS_CA_BUNDLE` at the emulator's certificate so dbt trusts the emulator's
   TLS, which REPLACES the system trust store — so `hub.getdbt.com` then fails
   with `CERTIFICATE_VERIFY_FAILED`. Vendoring `dbt_packages/` beforehand, or
   running `deps` with the unmodified environment, is the way through.
2. **The row-count bound is a real test.** The first spike run failed
   `expect_table_row_count_to_be_between` with `max_value: 100000` against a
   fixture seeding `N_ORDERS = 250_000` (`examples/contoso-fixtures/source_system.py`).
   That was the tool working. Any adopted bound has to be derived from the
   fixture rather than guessed.

## Scope of this measurement

One adapter version each (dbt-fabric 1.11.0, dbt-fabricspark, dbt 1.12.0),
`dbt_expectations` 0.10.10, against **SQL Server 2022** — which is what
`e2e/medallion/docker-compose.yml` pins. `REGEXP_*` functions arrived in SQL
Server 2025 and Azure SQL, so a newer Warehouse engine may resolve the regex
failure. It would not resolve the other five, which are about the language, not
the function library.
