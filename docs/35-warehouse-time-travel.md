# 35 — Warehouse time travel: the history is already on disk

**Status: shipped for the SQL analytics endpoint (Phases 0–3) and for the
Warehouse (Phase 4, write versioning) with its retention window (Phase 5).** The hard
part of time travel is retaining versions, and the emulator already retained
them before any of this phase list started — `_delta_log` keeps every commit
and Delta's `remove` is a tombstone, not a delete. What this plan closes is the
ability to *ask* for one: the T-SQL surface (`OPTION (FOR TIMESTAMP AS OF
…)`), now wired into the Lakehouse's SQL analytics endpoint. The warehouse
tables that live in the sidecar rather than in Delta gained a history of their
own in Phase 4: each accepted statement commits a Delta version.

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

## The blocking prerequisite (fixed — Phase 0 is done)

[`write.go`](../internal/warehouse/write.go) used to stamp every Delta commit
with `time.Now().UnixMilli()`.

**Wall clock.** Every other time-derived value in the emulator comes from the
controllable clock (`store.Now()` → `Clock.Now()`, [db.go](../internal/store/db.go)),
which is the property the whole project is built on: LRO completion, job status,
schedule firing. A commit timestamped from the host clock cannot be moved, so:

- a test could not write v1, advance an hour, write v2 and query the midpoint —
  it would have to *wait* an hour;
- `-clock-offset` and `POST /_emulator/clock` would silently not apply to the
  one feature whose entire subject is time;
- the emulator would disagree with itself about what time it is, in a file whose
  timestamps are the index for a user-facing query.

Both writers now stamp from `st.Now()*1000`: `WriteDeltaTableAs` in
[`write.go`](../internal/warehouse/write.go) and `writeDeltaSnapshot` in
[`mirror.go`](../internal/warehouse/mirror.go), covering `metaData.createdTime`,
`add.modificationTime` and `remove.deletionTimestamp`.

Every commit also opens with a `commitInfo` action —
`{"commitInfo":{"timestamp":…,"operation":"WRITE"}}` — which is what Phase 1
reads. Delta records a commit's own time nowhere else: `add.modificationTime`
belongs to the file, not the commit, and an append that adds no file would
carry no time at all. `TestDeltaCommitsStampedFromEmulatorClock`
([commit_clock_test.go](../internal/warehouse/commit_clock_test.go)) pins all of
it against a frozen clock advanced 400 days, so a wall-clock stamp cannot pass
by coincidence.

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
the statement references. **Implemented in Phase 3** —
`checkColumnsExistedAsOf` in
[`timetravel_adapt.go`](../internal/tsql/timetravel_adapt.go) — see the Phase 3
note below.

None of this table's rows ended up gated behind `-tsql-strict`, despite this
section's opening sentence — worth correcting rather than leaving to look like
a decision nobody noticed. Every other Class B entry in the codebase exists
because the sidecar genuinely CAN run something Fabric refuses (a recursive
CTE, say), and strict mode is the operator's choice to additionally forbid it.
None of that applies here: there is no permissive backend behaviour to
preserve by leaving these off, because the emulator itself is the only thing
that understands the hint at all. A malformed timestamp, the hint used twice,
a reference to a column that did not exist yet — none of these are things the
sidecar would otherwise accept; they are things *this feature's own code*
would otherwise get wrong. Phase 2's five lexical rules were already
unconditional (`ParseTimeTravelHint` takes no strict flag), and Phase 3's
schema check follows the same precedent rather than inventing a different one.

The `#temp` rule is the one place the emulator is likely to agree for free:
session temporaries are not Delta-backed, so a rewrite that only touches
resolved Delta tables leaves them alone by construction. Worth an assertion
rather than an assumption.

## Phases

**Phase 0 — the clock. Done.** Delta commits are stamped from `store.Now()`, and
each one opens with a `commitInfo` action carrying that timestamp. Worth doing
regardless of whether any later phase happens, because a wall-clock timestamp in
a commit log is wrong on its own terms.

**Phase 1 — read a version. Done.**
[`ReadDeltaTableAsOf(st, itemID, name, asOf)`](../internal/warehouse/delta.go)
is `activeFiles` with a stopping condition (`commitStop`, consulted before each
commit is applied), plus the schema as of that commit. A commit's time is its
`commitInfo.timestamp`, or else the newest `add.modificationTime` it carries for
a log written by someone else; an undated commit and a timestamp before the
table's first commit are both errors rather than a plausible answer. Pure Go, no
SQL, no protocol — `TestReadDeltaTableAsOf` covers three commits an emulator
hour apart, and their midpoints, with no server at all. This is the phase that
proves the premise.

**Phase 2 — parse the hint. Done.**
[`ParseTimeTravelHint`](../internal/tsql/timetravel.go) reads
`OPTION (FOR TIMESTAMP AS OF '<ts>')` from tokens — case-insensitive, whitespace
and comments allowed, and not a hint at all when the same words sit in a string
literal or a comment — and returns the instant in UTC plus the statement with the
hint cut out. Every Class B row in the table above that a lexer can see is
refused with a `*TimeTravelError{Rule, Detail}`: `timestamp-format` (more than
three fractional digits, or malformed, quoting Fabric's Msg 22440),
`timestamp-timezone`, `hint-once`, `select-only`, `view-definition` and
`non-deterministic`. `TestParseTimeTravelHint`
([timetravel_test.go](../internal/tsql/timetravel_test.go)) pins each one.

It is deliberately not wired into `Adapt` or `CheckStrict` yet: stripping the
hint without resolving the versions behind it would answer a question about the
past with today's data. That wiring is Phase 3.

**Phase 3 — the SQL analytics endpoint. Done.**
[`AdaptWithTimeTravel`](../internal/tsql/timetravel_adapt.go) wires Phases 1
and 2 into `Adapt`: it strips the hint (Phase 2), finds every table the
statement references — `findTableRefs`, the tokenizer work the risk below
named as the likely slip — resolves each one once (a self-join resolves its
table a single time and both aliases point at the same snapshot), and
rewrites the reference to a session `#temp` table materialised from
[`ReadDeltaTableAsOf`](../internal/warehouse/delta.go) (Phase 1). A reference
with no explicit alias gets one spliced in (`#tt0 AS Customer`) rather than
becoming a bare temp-table name, so a later qualifier written against the
table's own name — legal SQL when no alias was given — keeps resolving; this
was caught by a test before it shipped, not after. `checkColumnsExistedAsOf`
is the Class B schema check: it compares the resolver's `Columns` (as of the
timestamp) against `CurrentColumns` (today) and refuses — rather than
silently materialising `NULL`s — a reference to a column in that gap,
including the `SELECT *` case the design note calls out explicitly, by
telling a wildcard select-list star apart from multiplication on the single
token that precedes it rather than tracking parenthesis depth.

The resolver itself —
[`warehouse.TimeTravelResolver`](../internal/warehouse/timetravel.go) — is the
one place that knows both halves: it calls `ReadDeltaTableAsOf` and
`ReadDeltaTable` (today's schema) for one item, resolves a table name
case-insensitively against the item's own `Tables/` folders (SQL Server's
default collation is `CI_AS`; a resolver that matched by exact case would fail
to time-travel `dbo.customer` against a folder named `Customer`), and renders
each row as literal SQL text — there being no other channel available to hand
a materialised snapshot to the engine, since `Adapt` runs at the wire layer on
a statement's text, before the client's session is spliced straight to the
real backend (see "a materialised snapshot is not a Fabric MPP snapshot" in
Risks below). It is bound to one item only when that item is a Lakehouse's
analytics endpoint ([`internal/server/warehouse.go`](../internal/server/warehouse.go)):
a Warehouse connection's `Connection.TimeTravel` is left nil, so the hint
reaches the sidecar unrecognised and fails exactly as it always has — Phase 4
is still the only way to give a Warehouse table a history to travel in.

`#temp` tables are left alone by construction — `findTableRefs` never offers
one to the resolver — and `TestAdaptWithTimeTravelLeavesTempTablesAlone` pins
it as an assertion, per the design note's own instruction, rather than an
assumption. Read-only enforcement needed no new code: a `#temp` table
populated from a historical snapshot has nothing to write back to, and Phase
2's `select-only` rule already refuses `INSERT`/`UPDATE`/`DELETE` under the
hint before this phase's rewrite ever runs.

Deliberately not handled: `CROSS APPLY`/`OUTER APPLY` targets are never
scanned for table references (their right-hand side is overwhelmingly a
correlated subquery or a table-valued function, not a plain table, in real
queries), and a table reached only through a OneLake or external shortcut is
left unresolved (`ok=false`) rather than taught a second reading path —
`ReadDeltaTableAsOf` only knows `Tables/<name>`, and the SQL analytics
endpoint's own tables are where the real multi-commit Delta history lives
either way. Both are named in `timetravel_adapt.go` and `timetravel.go` rather
than discovered later.

**Phase 4 — warehouse write versioning. Done.**
[`warehouseVersioner`](../internal/server/warehouseversioning.go) is the second
consumer of the flows the TDS front hands over (the first is lineage). After the
engine accepts a statement that changed a Warehouse table, it reads the table
back from the sidecar and commits it as the next Delta version under the item's
own `Tables/<name>` — [`SnapshotTable`](../internal/warehouse/versioning.go) —
which is where a real Warehouse keeps its data. The existing reader and
resolver then serve a Warehouse unchanged: the only wiring is that a Warehouse
connection now gets a resolver too
([`WarehouseTimeTravelResolver`](../internal/warehouse/versioning.go)).

What counts as a version: `CREATE TABLE` (empty, so the table exists from that
instant), `CTAS`/`SELECT INTO`, `INSERT` (with or without a `SELECT`), `UPDATE`,
`DELETE`, `TRUNCATE`, `MERGE` and `ALTER TABLE` — one commit per statement, the
schema restated every time so an `ALTER` is visible to a replay stopped there.
`UPDATE x … FROM dbo.t x` resolves the alias through the statement's own `FROM`
list ([`dataflow_modify.go`](../internal/tsql/dataflow_modify.go)).

History belongs to the table *object*, which is Fabric's behaviour and not a
choice made here: `sp_rename` moves it with the table, `DROP TABLE` ends it. A
dbt rebuild (build `x__dbt_temp`, swap it in) therefore starts the new table's
history at the swap, and the table it replaced does not leak into it.

It can never fail the statement. The observer runs after the client already has
its result, so a snapshot that fails is logged with the table's name and leaves
a gap in that table's history; the write stands.

Not versioned, so nobody learns it by surprise (each is logged when it applies,
never silent):

- **Writes the wire cannot see** — a stored procedure's body, `BULK INSERT`/bcp,
  a pipeline Script activity (it uses the control-plane connection), and a
  statement whose response carries a result set (`UPDATE … OUTPUT`). Class B.
- **A table outside `dbo`**, **a view** (no rows), and **a table over
  `MaxVersionedRows` (500,000)** — a snapshot reads the whole table, so a loop
  of single-row `INSERT`s into a large table would be quadratic.
- **A Lakehouse and a SQL Database** — the first is read-only on this wire (its
  history is its Delta log), the second mirrors on demand.

The cost is real and stated: every data-changing statement on a Warehouse now
also reads that table and writes a Parquet file. `-warehouse-versioning=false`
(`FABRIC_WAREHOUSE_VERSIONING=off`) turns it off for a build that does not want
a history.

**Phase 5 — retention. Done.** `-warehouse-retention-days` /
`FABRIC_WAREHOUSE_RETENTION_DAYS`, 1–120, default 30; anything outside the range
is refused at startup rather than clamped. Two halves:

- **The window is checked, by name.** An instant older than it is refused with
  `… is older than this warehouse's N-day data retention window (oldest
  available: …)` — before the reader runs, so it never surfaces as a missing
  data file.
- **Expiry removes bytes, not history.**
  [`ExpireVersions`](../internal/warehouse/versioning.go) runs on the table
  just written, so it is deterministic under the emulator's controllable clock
  (no background timer to wait for). It deletes only the data files that no
  version inside the window can reach, and keeps the state at the cutoff itself
  (a query for exactly that instant must still work). The `_delta_log` is kept
  whole — deleting leading commits would leave a log delta-rs and Spark cannot
  replay, and the log is metadata, not the space. That is Delta's own `VACUUM`.
  Nothing the current table needs can be removed: the newest version is never
  before the base.

The window applies to a **Warehouse**. A Lakehouse's analytics endpoint reads the
Delta log the user owns, and what it retains is the user's `VACUUM` policy, not
this setting.

Found on the way: Phase 3 left a plain batch's `#tt0` temp table alive for the
whole session, so a **second** time-travel query on one connection failed with
"There is already an object named '#tt0'". Every earlier test issued one hint
per connection. The materialisation now drops a stale table of that name first
(`TestAdaptWithTimeTravelDropsAStaleTempTableFirst`; end to end,
`TestWarehouseTimeTravelOverTheWire`). It affected the Lakehouse endpoint too.

Witnessed over the real TDS wire against a real SQL Server:
[`warehouse_versioning_e2e_test.go`](../internal/server/warehouse_versioning_e2e_test.go)
(history across insert/update/delete/truncate, a dbt swap, a drop, the
retention refusal, and the off switch), with the log/expiry mechanics also
pinned without a server in
[`versioning_test.go`](../internal/warehouse/versioning_test.go).

Phases 0–5 are done: a real, honest feature covering the SQL analytics endpoint
and the Warehouse.

## Risks, stated rather than discovered later

- **Statement-wide scope is a parser change, not a string substitution.** The
  hint applies to every joined table, so `Adapt` needs the statement's table
  references — something the lexer did not extract before Phase 3.
  **Materialised in Phase 3** as `findTableRefs`
  ([`timetravel_adapt.go`](../internal/tsql/timetravel_adapt.go)), and the risk
  was real rather than theoretical: two correctness bugs were caught by tests
  before this phase shipped, not after — a resolver call that had silently
  lower-cased a mixed-case table name (breaking a case-sensitive OneLake
  lookup for `Customer` asked about as `customer`), and a rewrite that
  replaced a table reference with a bare temp-table name, which left a WHERE
  clause written against the table's own implicit name (`FROM dbo.A, dbo.B
  WHERE A.id = B.id`, no alias at all) referring to nothing.
- **Phase 4 touches the path that builds gold.** `e2e/dbt-fabric` and the
  medallion examples are the regression surface, and a mistake there is
  expensive in a way Phases 0–3 are not. Mitigated by construction: versioning
  runs after the client has its result and cannot fail the statement; it is
  switchable off; and the whole Go suite plus the dbt/medallion e2e legs run
  with it on.
- **A materialised snapshot is not a Fabric MPP snapshot.** The emulator
  answers from a `#temp` table populated at query time from literal SQL text —
  not even the bulk-copy path `reflectTable` uses for an ordinary reflect,
  because `Adapt` runs on a statement's TEXT at the wire layer, before the
  client's session is spliced straight to the real backend, and that text is
  the only channel available to hand the engine a historical snapshot
  ([`timetravel.go`](../internal/warehouse/timetravel.go)). Fabric answers from
  versioned storage directly. Behaviour matches; performance characteristics do
  not, and nothing here should claim otherwise — a time-travel query's own
  result set is usually far smaller than a full reflect, which is the
  mitigating fact, not a reason to call the trade-off free.
- **Retention that silently deletes is worse than none.** Phase 5 removes files
  a user could previously query, so it is loud: the refusal names the window and
  the oldest instant available, expiry only ever removes what no in-window
  version reaches, and the log stays whole. A Lakehouse endpoint has no window
  (see Phase 5) — still *more* permissive than Fabric there, recorded in
  [29-tsql-parity.md](29-tsql-parity.md).

## Non-goals

- **`CLONE TABLE`** is the other half of Microsoft's time-travel page and a
  separate feature — table-level, not statement-level, with its own DDL. Out of
  scope here; worth its own note if demand appears.
- **Power BI Desktop DirectQuery.** Real Fabric does not support the hint there
  either, so the emulator agrees for free. This is a **Class C** entry — record
  it in [29-tsql-parity.md](29-tsql-parity.md) as agreeing rather than leaving it
  to look like a gap.
