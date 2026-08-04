# 35 — Warehouse time travel: the history is already on disk

**Status: not implemented, and NOT blocked on the hard part.** The hard part of
time travel is retaining versions, and the emulator already retains them —
`_delta_log` keeps every commit and Delta's `remove` is a tombstone, not a
delete. What is missing is the ability to *ask* for one: the T-SQL surface
(`OPTION (FOR TIMESTAMP AS OF …)`), and a decision about the warehouse tables
that live in the sidecar rather than in Delta.

This document is the design. Nothing here is built.

[29-tsql-parity.md](29-tsql-parity.md) supplies the Class A/B/C vocabulary this
plan is written in and is the doc that must be updated when it ships.
[16-warehouse-tds.md](16-warehouse-tds.md) settles the architecture it lands in.
[20-lakesail-engine.md](20-lakesail-engine.md) records the half that already
works — Spark-side time travel — and is the reason this gap is narrower than it
looks.

## Why anyone wants it

Time travel is the difference between a warehouse that holds data and one that
holds a *record*. Every use Microsoft lists — stable reporting while ETL runs,
audit, reproducing a model's training set, comparing two versions to find when a
number changed — reduces to one question a consumer cannot otherwise ask: *what
did this table say before?*

For this project the demand is narrower and more concrete. `dbt` rebuilds gold
on every run. When a number moves, the previous value is gone, and the only way
to find out what changed is to have written it down beforehand. A consumer
testing against the emulator today cannot write the query that would answer it,
because the syntax does not parse.

## What Fabric actually specifies

Read off [the documentation](https://learn.microsoft.com/en-us/fabric/data-warehouse/time-travel),
not remembered:

```sql
SELECT *
FROM [dbo].[dimension_customer] AS DC
OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35.28');
```

| | Specified |
|---|---|
| Form | `OPTION (FOR TIMESTAMP AS OF '<ts>')`, a query hint |
| Timestamp | `YYYY-MM-DDTHH:MM:SS[.fff]` — **at most three** fractional digits |
| Scope | The **whole statement**, including every joined table |
| Time zone | **UTC only** |
| Retention | 1–120 calendar days, **default 30**, from warehouse creation |
| Read-only | `INSERT`/`UPDATE`/`DELETE` cannot run under the hint |
| Frequency | **Once** per `SELECT` |
| Statements | Only statements that **begin with `SELECT`** |
| Views | Cannot appear in a view *definition*; a view **can** be queried with it |
| Temp tables | Session-scoped `#temp` is **unaffected** by the hint |
| Determinism | The value must be deterministic — no expressions |
| Schema | Returns the **latest** schema; referencing a column that did not exist at that timestamp **fails** |
| Applies to | Warehouse **and** SQL analytics endpoint |

Two of these are worth reading twice. The hint is **statement-wide**, so this is
not a per-table-reference rewrite. And a `SELECT *` at a past timestamp returns
today's column list — which means the schema rule is not "restore the old
schema", it is "answer with the current schema and fail if you are asked for a
column that had not been invented yet".

## The insight: the emulator already keeps the history

[`activeFiles`](../internal/warehouse/delta.go) replays the `_delta_log` commits
in order, accumulating `add` and `remove` actions to compute the set of live
Parquet files. It always replays to the end.

```go
for _, c := range commits {
    …
    case a.Add != nil:    active[a.Add.Path] = true
    case a.Remove != nil: active[a.Remove.Path] = false
}
```

**Stopping that loop early is time travel.** A Delta `remove` is a tombstone —
the Parquet file it names is still on disk — so replaying to commit *N* yields
exactly the file set that was live at commit *N*. No new storage, no shadow
copies, no second write path. The reader gains a parameter and the rest of the
function is unchanged.

That is the whole reason this feature is cheap on the Lakehouse side and
expensive on the warehouse side: one has a version history and the other does
not.

## The blocking prerequisite, and it is a bug today

[`write.go`](../internal/warehouse/write.go) stamps every Delta commit with:

```go
time.Now().UnixMilli()
```

**Wall clock.** Every other time-derived value in the emulator comes from the
controllable clock (`store.Now()` → `Clock.Now()`, [db.go](../internal/store/db.go)),
which is the property the whole project is built on: LRO completion, job status,
schedule firing. A commit timestamped from the host clock cannot be moved, so:

- a test cannot write v1, advance an hour, write v2 and query the midpoint —
  it would have to *wait* an hour;
- `-clock-offset` and `POST /_emulator/clock` would silently not apply to the
  one feature whose entire subject is time;
- the emulator would disagree with itself about what time it is, in a file whose
  timestamps are the index for a user-facing query.

This is worth fixing whether or not time travel is ever built, and nothing here
should be started before it is. It is also the cheapest item in the plan.

## Two surfaces, and they are not equally hard

| Surface | Where the data lives | History today | Cost |
|---|---|---|---|
| **SQL analytics endpoint** (over a Lakehouse) | Delta in OneLake, materialised into the sidecar by [`Reflect`](../internal/warehouse/reflect.go) | **Real**, multi-commit | Small |
| **Warehouse** (dbt-built gold) | The SQL Server sidecar, written over TDS | **None** | Large |

`Mirror` does not rescue the second row: it writes "a fresh single-commit
snapshot per table (a full re-sync, not incremental)", so it produces a Delta
table with no past.

The consequence for phasing is that the endpoint surface is worth shipping on
its own. It is the faithful one — the versions are real Delta history that a
Spark job could read with `versionAsOf` and get the same answer — and it needs
none of the warehouse write-path work.

## The approach to reject, and why

SQL Server 2022 has temporal tables. `FOR SYSTEM_TIME AS OF` is close enough to
Fabric's semantics that the emulator could enable `SYSTEM_VERSIONING` on every
warehouse table, rewrite the hint onto each table reference, and let the engine
answer. It is exactly the shape of work [`internal/tsql`](../internal/tsql/)
already does, and it would be a small diff.

**Do not do this.** Three reasons, in increasing order of severity:

1. **Period columns change the schema.** `HIDDEN` keeps them out of `SELECT *`,
   so this one is survivable — but it is a permanent divergence to maintain in
   a table shape that is supposed to match Fabric's.
2. **Temporal DDL restrictions collide with dbt.** The `table` materialization
   builds into `x__dbt_temp` and swaps with two `sp_rename` calls
   ([29-tsql-parity.md](29-tsql-parity.md), T8). Renaming and dropping tables
   under system versioning is restricted. The main warehouse consumer would
   break to add a warehouse feature.
3. **The history would be stamped by the sidecar's clock.** This is the one that
   settles it. SQL Server writes `SysStartTime` from the container's own clock,
   which the emulator does not control. Advancing `/_emulator/clock` would move
   schedules, jobs and LROs but not table history — and the feature would be
   untestable by the exact lever this project built for testing time.

Reason 3 generalises into a rule worth stating: **the emulator must own any
timeline a user can query.** Delegating one to a backend buys a fast
implementation and loses determinism, which is the thing being sold.

## What exists to build on

| Asset | Why it matters |
|---|---|
| [`activeFiles`](../internal/warehouse/delta.go) | Already replays the commit log. Time travel is a stopping condition, not a new reader |
| The controllable clock ([db.go](../internal/store/db.go)) | Makes `AS OF` deterministically testable — *more* testable here than in real Fabric, where you must wait |
| [`Adapt`](../internal/tsql/ctas.go) | The established hook for every dialect rewrite, in a fixed order, including inside `EXEC('…')` |
| [`hasOptionHint`](../internal/tsql/restrictions.go) | Already recognises an `OPTION(...)` hint in the tokenizer |
| [`-tsql-strict`](../internal/tsql/strict.go) | The existing home for Class B refusals, off by default because removing capability is the operator's call |
| Spark time travel (`versionAsOf`, SQL `VERSION AS OF`) | Already 🟢 on both engines ([parity.md](parity.md), [engine-matrix.md](engine-matrix.md)). The gap is *only* the T-SQL spelling |

The last row narrows the problem usefully. This is not "the emulator cannot time
travel" — it is "the emulator cannot be *asked* to, in T-SQL".

`restrictions.go` also already records the shape of the parsing gap, in the list
of rules it deliberately does not enforce:

> `AS OF` in a nested definition (Fabric rejects) needs temporal-clause parsing
> this lexer does not do; the construct cannot arise from the tooling T6 targets.

That second clause stops being true the moment a consumer writes a time-travel
query.

## Class A — Fabric accepts it, the sidecar rejects it

Exactly one entry, and today it is the whole feature: **`OPTION (FOR TIMESTAMP
AS OF …)` does not parse.** SQL Server fails it as an unrecognised query hint, so
a consumer's time-travel query dies with a syntax error that names nothing about
time travel.

Closing it means `Adapt` must:

1. **Recognise and strip** the hint from the statement.
2. **Resolve** the timestamp to a version per referenced table — for Delta-backed
   tables, the last commit at or before it.
3. **Materialise** those versions into session-scoped temporaries and **rewrite
   the references** to point at them.
4. Leave everything else alone.

Step 3 is where the statement-wide scope is honoured: every table in the
statement, including joins, resolves at the same timestamp. It is also what
makes the result read-only by construction, without a separate check — a
temporary populated from a historical snapshot has nothing to write back to.

Note that this is *not* the same rewrite shape as CTAS or nested-CTE flattening,
both of which are local transformations. This one needs the set of table
references in the statement, which is new work for the tokenizer.

## Class B — Fabric rejects it, the sidecar would accept it

Each of these is a way a local build could go green on SQL that real Fabric
refuses. They belong behind `-tsql-strict`, like every other Class B entry.

| Rule | Fabric's behaviour | Why the sidecar would not catch it |
|---|---|---|
| More than 3 fractional second digits | `Msg 22440` — *"An error occurred during timestamp conversion. Please provide a timestamp in the format yyyy-MM-ddTHH:mm:ss[.fff]"* | `datetime2` happily takes 7 |
| Hint used twice in one `SELECT` | Rejected | The hint is stripped before the sidecar sees it |
| Statement does not begin with `SELECT` | Rejected | ditto |
| `INSERT`/`UPDATE`/`DELETE` under the hint | Rejected | ditto |
| Hint inside a **view definition** | Rejected (querying a view *with* it is fine) | ditto |
| Non-deterministic value | Rejected | ditto |
| Column added after the timestamp | Query **fails** | The materialised table has today's columns, so it would silently succeed |

The last row is the dangerous one and deserves its own note. Because Fabric
returns the *latest* schema, the naive implementation — materialise the old
files, hand them to the sidecar — answers a query about a column that did not
exist by returning `NULL`s. That is a Class B failure of the worst kind: a
plausible answer to a question Fabric would have refused. Enforcing it needs the
schema as of the timestamp (recoverable from the `metaData` actions
`activeFiles` already reads for schema evolution) compared against the columns
the statement references.

The `#temp` rule is the one place the emulator is likely to agree for free:
session temporaries are not Delta-backed, so a rewrite that only touches
resolved Delta tables leaves them alone by construction. Worth an assertion
rather than an assumption.

## Phases

**Phase 0 — the clock.** Stamp Delta commits from `store.Now()`. One line, plus a
test that a commit written under an offset clock lands at the offset time. **Do
this regardless of whether any later phase happens**, because a wall-clock
timestamp in a commit log is wrong on its own terms.

**Phase 1 — read a version.** `ReadDeltaTableAsOf(st, itemID, name, ts)`:
`activeFiles` with a stopping condition, plus the schema as of that commit. Pure
Go, no SQL, no protocol — unit-testable against a fixture log with three commits
and no server at all. This is the phase that proves the premise.

**Phase 2 — parse the hint.** Recognition, extraction, and every Class B refusal
in the table above, in `internal/tsql`. Also pure, also unit-testable, and
independently useful: even with no execution behind it, a consumer gets
Fabric's error instead of a syntax error.

**Phase 3 — the SQL analytics endpoint.** Wire 1 and 2 into the reflect path.
This is the first phase with a user-visible feature, it is the faithful surface,
and it needs nothing from the warehouse write path. **Ship-able alone.**

**Phase 4 — warehouse write versioning.** Give warehouse tables a history by
committing to Delta on each data-changing statement. The TDS front already
observes accepted writes — that is how `ProducerWarehouse` lineage is recorded
([`warehouselineage.go`](../internal/server/warehouselineage.go)) — so the hook
exists; what is new is writing Delta from it. This is the phase to scope
separately and price properly, because it changes the write path gold depends
on.

**Phase 5 — retention.** Configurable 1–120 days, default 30, and expiry of
files past it. Cheap once versions are addressable, and meaningless before.

Phases 0–3 are the bulk of the value. A reader should be able to stop after 3
and have a real, honest feature covering the SQL analytics endpoint, with the
warehouse recorded as 🟠 rather than claimed.

## Risks, stated rather than discovered later

- **Statement-wide scope is a parser change, not a string substitution.** The
  hint applies to every joined table, so `Adapt` needs the statement's table
  references — something the current lexer does not extract. Underestimating
  this is the most likely way Phase 2 slips.
- **Phase 4 touches the path that builds gold.** `e2e/dbt-fabric` and the
  medallion examples are the regression surface, and a mistake there is
  expensive in a way Phases 0–3 are not.
- **A materialised snapshot is not a Fabric MPP snapshot.** The emulator would
  answer from a temporary populated at query time; Fabric answers from
  versioned storage directly. Behaviour matches; performance characteristics do
  not, and nothing here should claim otherwise.
- **Retention that silently deletes is worse than none.** Phase 5 removes files
  a user could previously query. Until expiry is implemented, the emulator
  retains everything — which is *more* permissive than Fabric and therefore a
  Class B entry of its own: a query that works locally and fails in production
  because the window had passed. It belongs in the table above the day Phase 3
  ships, not the day Phase 5 does.

## Non-goals

- **`CLONE TABLE`** is the other half of Microsoft's time-travel page and a
  separate feature — table-level, not statement-level, with its own DDL. Out of
  scope here; worth its own note if demand appears.
- **Power BI Desktop DirectQuery.** Real Fabric does not support the hint there
  either, so the emulator agrees for free. This is a **Class C** entry — record
  it in [29-tsql-parity.md](29-tsql-parity.md) as agreeing rather than leaving it
  to look like a gap.
