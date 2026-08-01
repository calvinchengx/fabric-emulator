# Example: the medallion, gold built by Spark

The same medallion as [`../medallion`](../medallion/), diverging at gold: this
one builds the star with **Microsoft's `dbt-fabricspark`** over the emulator's
Livy High-Concurrency surface, with **Sail** behind it — where the Warehouse
example uses `dbt-fabric` over TDS against a SQL Server sidecar.

Fabric offers both. Real shops pick one, or run both, and the choice is
consequential — but an example that shows only one path presents it as *the*
path. This exists so the choice is demonstrated rather than assumed.

## What actually differs

Steps 00–05 are **identical** and are run from `../medallion` unmodified, because
provisioning, the Key Vault secret, landing, bronze, the notebook run and silver
do not care which engine builds gold. Copying them here would mean two versions
of the same eight files drifting apart. The paths diverge from step 06:

| | Warehouse (`../medallion`) | Lakehouse (here) |
|---|---|---|
| adapter | `dbt-fabric` | `dbt-fabricspark` |
| transport | TDS / ODBC Driver 18 | Livy HC (REST) |
| compute | SQL Server sidecar | Sail (Rust Spark Connect, no JVM) |
| gold lives in | a **Warehouse** item | the **Lakehouse**, as Delta |
| reading silver | needs `reflect.py` to copy every Delta table into SQL first | reads the Delta files directly — **no reflection step exists here** |
| databases | two (lakehouse + warehouse), joined by three-part names | one |
| dialect | CTAS rewritten to `SELECT … INTO`; nested CTEs flattened ([docs/29](../../docs/29-tsql-parity.md)) | none — the SQL dbt emits runs unmodified |

That "no reflection step" row is the structural one. The Warehouse path must
materialise silver a second time before dbt can see it; the Spark path reads
what silver already wrote.

## Run it

```sh
docker compose up -d          # from the repo root — no overlay needed
```

Then, in **`../medallion`** first (the comparison needs both halves):

```sh
uv run python pipeline.py
```

and then here:

```sh
uv sync --frozen
uv run python pipeline.py
```

## The comparison

`compare.py` reads the `gold_summary.json` each example writes and reports
three things:

1. **Equivalence** — both stars have identical row counts and the same revenue
   to the cent. This is the claim worth making: *the adapter choice does not
   change the answer.* It is asserted, so a divergence fails the run.
2. **Dialect divergence** — which statements the emulator had to adapt on the
   wire for each path. It is not symmetric, and that asymmetry is the real
   portability cost of choosing Warehouse-mode dbt.
3. **Wall-clock** — and read the caveat, which is not boilerplate.

> **The timings are not a Fabric benchmark.** They compare a vanilla **SQL
> Server container** against a **Sail container** on your laptop. Fabric
> Warehouse is a distributed MPP engine; Fabric Spark is a managed pool.
> Neither is represented here, and a ratio measured on this stack says nothing
> about which is faster on Fabric. The numbers are useful for one thing only:
> knowing what each path costs you locally, in a loop you run all day.

## A note on the Livy agent

This example runs on the standard stack with no overlay, which was not true
before it existed. The shipped agent (`e2e/livy/agent.py`) was a **Python** REPL
— right for notebooks, useless to a SQL client — so `dbt-fabricspark` had
nothing to talk to and the dbt e2e carried its own SQL-only agent.

Two things changed. The emulator's Livy surface now forwards a statement's
`kind` (it was decoding only `code` and dropping it), and the agent dispatches
on it: `sql` runs Spark SQL and returns the structured `application/json`
envelope a result set needs, anything else runs Python and returns REPL text.
One agent, both consumers.
