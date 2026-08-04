# Implementation-vs-docs audit

## 2026-08-05 — what the previous pass got wrong (this update)

Everything below was written while `v0.16.0` was being prepared. One of its
claims did not survive contact with the released binary, and the way it failed
is the most useful thing in this file.

### The fix was real; the claim about it was not

The entry further down — "nested types corrupted the whole row" — records the
displacement bug and calls it fixed. Half of that is right, and the released
image confirms it: `flat` and `after_flat` both carry their own values, where
`0.15.3` returned `"SG"` for a `bigint` and dropped the real `999`.

But the *other* half of that entry, the part that says the column is dropped,
was false in the shipped binary. Measured by contoso-data-platform against
`ghcr.io/…:0.16.0`:

    probe_nested columns: ['web_order_id', 'lines', 'addr', 'tags']
    values:               web_order_id='W-1'  lines=None  addr=None  tags=None

`readParquet` skipped them correctly. `ReadDeltaTable` then re-projected each
part onto the logical schema from the Delta log — which still names the nested
fields — so every one was re-added with a nil value, took `sqlType`'s `varchar`
default (no non-null value is ever seen) and served as NULL.

**Why the test suite could not see it.** `types_test.go` asserted on
`readParquet`'s output. The projection happens one stage later. A map-level
assertion cannot observe what a downstream stage does to the thing it asserted
on — the same map-vs-route distinction the type-map probe was built for, this
time biting inside our own tests. The route-level test now exists and fails on
the old code with exactly the consumer's symptom.

**The second drop site, which nobody reported.** `Skipped` was also lost at
`tbl = &Table{Columns: part.Columns}`, whether or not any projection ran. So the
"not representable … omitted" warning — written specifically because "it is a
miserable thing to debug without the name" — was unreachable from the read path
entirely. Worth keeping as a general lesson, in the consumer's words: *a warning
nobody can trigger reads exactly like a warning nobody has triggered.* A quiet
log is not evidence of a quiet path.

**The order-dependence trap, which the obvious fix walks into.** Deriving the
nested set from the first data file's `Skipped` passes every test anyone had.
It breaks after a schema evolution that ADDS a nested column: the oldest file is
first in commit order, does not carry that column at all, and therefore skips
nothing — so the nested column is re-added for every later file. The set now
comes from the Delta `schemaString` (primitives are JSON strings, struct/array/
map are JSON objects), which is order-independent by construction rather than by
happening to read the right file first.

**Direction matters.** NULL is a safe failure where a fabricated value was not,
so this was milder than what it replaced. But against Fabric `SELECT lines`
fails with an invalid column name, and against the emulator it returned NULL and
kept going — the emulator was **more permissive than the thing it emulates**,
which is the one asymmetry an emulator must not have: code passes locally and
fails in production.

Fixed in `v0.16.1`. `v0.16.0`'s release notes, both in-repo and the published
GitHub body, now carry the correction at the point of the claim.

### Three numeric widths, verified against Microsoft before changing anything

Reported off the same page. All three failed in one direction — **one width too
wide, with nothing raised**:

| Delta | Parquet | annotation | was | Fabric |
|---|---|---|---|---|
| `tinyint` | INT32 | `INT(8,true)` | `int` | `smallint` |
| `smallint` | INT32 | `INT(16,true)` | `int` | `smallint` |
| `real` | FLOAT | — | `float` | `real` |

The integer widths are the `date` bug's exact shape: physically an INT32 like
any other, with the width living only in the annotation the reader discarded.
`real` is milder in cause — FLOAT and DOUBLE differ in the physical kind — and
identical in effect: `real` and `double` were indistinguishable at the endpoint.

The consumer flagged these as *possibly* unreachable rather than as bugs, which
was the right call to make and the wrong conclusion to act on: measuring took
one scratch test and showed all three live.

### Two findings from chasing where the values go

Neither was reported; both came from asking what consumes `Table.Rows`.

- **The bulk-copy encoder rejects `int16` outright.** Its integer arm accepts
  `int/int32/int64/float32/float64` and defaults to
  `mssql: invalid type for int column`, failing the entire copy. Only
  `WAREHOUSE_MSSQL_DSN`-gated tests execute that encoder, so narrowing to
  `int16` was green on a laptop and would have broken CI. `bulkValue` widens
  back to `int64`; `sqlType` has already declared the column `SMALLINT` by then,
  so only the wire encoding is affected.
- **Pipeline expressions read every integer column as `0`** — pre-existing,
  unrelated to the widths, and present in every build up to `v0.16.0`.
  `toNumber` listed only `float64` and `int`, but a Lookup over a Delta table or
  Parquet file puts the READER's types into the row map: `int32` for a Delta
  int, `int64` for a bigint. So `@activity('L').output.firstRow.amount` on a
  bigint column evaluated to `0`, silently, and every expression built on it was
  wrong. `toBool` had the same hole, where it costs a wrong branch rather than a
  wrong number.

### What this pass changes about how the claims below should be read

Three artifacts carried the same disproved sentence — the repo notes, the
published release body, and this file. Correcting one is not correcting the
claim. The published GitHub release body in particular is a **separate artifact
from the file it was generated from**, and editing the file does not touch it;
that gap survived because the body was published before the correction was
written.

---

## 2026-08-04 — code audit

A different kind of pass from the ones below. Those reconciled **docs against
code**; this one audited the **code itself** across five dimensions in parallel
— security/auth, concurrency/data-integrity, T-SQL & TDS correctness, API
fidelity/input-validation, and docs/parity accuracy — with every finding traced
to a concrete input→wrong-output before being believed.

**The docs came out clean.** The parity checker passes (76 🟢 claims, every one
with a manifest entry, no dangling refs), and a sample of ~30 claims across
`parity.md`, `07-control-plane-api`, `08-onelake`, `12-e2e-matrix`,
`22-openmetadata` and `31-flow-observability` found **zero overstatements** — no
🟢 row that is secretly a stub, no named witness that does not exist. The event
kinds, producers and endpoints the docs describe are all really emitted.

The code found four defects that could not be seen from the docs.

### Fixed in this pass (each pinned by a test that fails without the fix)
- **CRITICAL — TDS empty database bypassed all workspace RBAC.**
  `OnConnect` — the only place a TDS connection's role and read-only-ness are
  decided — ran only when a database name was present. Without one, `readOnly`
  stayed false and `targetDB` stayed empty, which also failed the splice gate,
  dropping the session into the re-encode relay where `DB("")` is the backend's
  **default pool** under the emulator's privileged credential. A token with no
  role on any workspace could run arbitrary read/write T-SQL. Real Fabric
  requires a database on the connection, so rejecting is the faithful answer as
  well as the safe one. `internal/tds/server.go`; tests
  `TestLoginWithoutDatabaseIsRejected` (asserts the login fails **and** the
  backend runs nothing) and
  `TestConfiguredAuthorizerNeverReachesTheReEncodeRelay`, which pins the
  structural property that keeps it true.
- **CRITICAL — CTAS rewrite spliced `INTO` into the wrong statement.**
  The body ran to the end of the *batch*, so a CTAS whose own SELECT had no
  depth-0 `FROM` found one belonging to a later statement:
  `CREATE TABLE t AS SELECT 1 AS x; SELECT b FROM other` became
  `SELECT 1 AS x; SELECT b into t FROM other` — which executes without error and
  fills `t` from `other`. Silent data corruption, on every SQLBatch, RPC param
  and `EXEC('…')` literal. The body is now bounded at its own depth-0 `;`.
  `internal/tsql/ctas.go`; test `TestRewriteCTASStopsAtTheStatementBoundary`.
- **HIGH — a self-referential DAX measure crashed the process.**
  Measure expressions are re-parsed and evaluated in place, so `M = [M]` (or an
  A→B→A cycle) recursed without end. Go treats stack overflow as a **fatal**
  error `net/http`'s per-request recover cannot catch, so one `executeQueries`
  request killed the emulator and every other in-flight request. Expansion now
  carries a depth budget. Same file: `SUM()`/`COUNTROWS()` indexed `args[0]`
  before checking arity, panicking on a query a client can send.
  `internal/semanticmodel/dax.go`; tests `TestDAXMeasureRecursionIsBounded`,
  `TestDAXMeasureNestingStillWorks`, `TestDAXZeroArgFunctionsAreRefused`.
- **HIGH — the event bus panicked on shutdown.**
  `publish` checked `stopped` under the lock, released it, then sent on `b.raw`;
  `stop()` could close the channel in that window, and a send on a closed
  channel is a *ready* select case, so the `default:` did not save it. It
  panicked on the writer's own goroutine — for a OneLake write, crashing a
  request that had already committed. The send now happens under the same lock.
  `internal/store/bus.go`; test `TestPublishDuringCloseDoesNotPanic`.

### Also fixed in this pass (the mediums)
- **A JWT with no `exp` never expired.** The guard was `Exp != 0 && now > Exp`,
  so a token without the claim skipped expiry entirely — honored forever here,
  refused by real Entra, which requires it. That also made the controllable
  clock's expiry scenarios unenforceable for such tokens. `nbf` keeps its
  zero-skip, being genuinely optional. `internal/auth/auth.go`; case added to
  `TestValidateRejections`.
- **`AppendOneLakePath` lost data.** The length read and the `UPDATE` were
  separate statements: one connection serializes each STATEMENT, not the pair,
  so two appends at the same offset both read length N, both passed the position
  check, and the second overwrote the first. Now one transaction, as
  `RenameOneLakePath` already was. `internal/store/onelake.go`; test
  `TestConcurrentAppendsDoNotLoseData` asserts the file's length equals the sum
  of the appends that reported success — it reproduced the loss 3 runs of 3.
- **Staged blocks grew without bound.** Freed only on commit, so an upload that
  never committed held its bytes for the process lifetime — routine with
  delta-rs, which retries a lost `_delta_log` commit under a NEW blob key and
  abandons the previous staging. Now size-bounded, evicting whole abandoned
  blobs oldest-first and never the one being written; an evicted blob fails its
  commit with an unknown block id, which is what Azure answers for expired
  blocks. Azure's real 7-day expiry is not emulated: a process rarely lives that
  long, and tying eviction to the emulator clock would stop it whenever a test
  freezes time. `internal/onelake/blob.go`; tests `TestBlockStageIsBounded`,
  `TestBlockStageFreesOnCommit`.
- **Nested-CTE aliases became phantom lineage edges.** Names were collected from
  the leading `WITH` only while `FROM`/`JOIN` are read at every depth, so a CTE
  in a nested `WITH` was emitted as a source *table* — and
  `warehouseLineage.resolve` does not check that a table exists, so a real edge
  was written for one that never did. Collected at every level now; a `with`
  that is not a CTE clause still collects nothing. `internal/tsql/dataflow.go`;
  tests `TestDataFlowExcludesNestedCTENames`, `TestDataFlowWithClauseThatIsNotACTE`.

### Fixed — dbt's cached manifest could silently shrink the test set
Reported 2026-08-04 from `contoso-data-platform`, and explicitly NOT a
fabric-emulator bug — it is dbt-core behaviour. It is here because the shape is
the same as the other two reports: a suite that reports PASS without reporting
how much it ran.

Measured by the reporter on dbt-core/dbt-fabric 1.9.10: after editing
`schema.yml`, a build against a cached manifest reported `Found 8 models, 19
data tests` / `PASS=27 ERROR=0`, fully green. `rm -rf target/` on the identical
tree reported `46 data tests` / `PASS=44 ERROR=1 SKIP=9`. So 27 of 46 tests had
never run, five of eight models had no assertions executed against them, and one
of the skipped tests was genuinely failing.

Fixed by adding `--no-partial-parse` to **every** dbt invocation — 20 call sites
across 15 files, not the 5 the report listed: the medallion examples exist in
four parallel variants, and `e2e/data-science-loop/driver.py` spreads its
argument list over several lines. Applied uniformly because
`scripts/check_example_parity.py` requires the variants to differ only in
silver, so a partial fix would have failed `make check` — a useful forcing
function. Written in the global position (`dbt --no-partial-parse build`) rather
than trailing, since the pin is `>=1.9` with no ceiling.

CI is unaffected either way — its runners are fresh containers with no cache.
The exposure is a warm workspace, which is exactly what `docs/12-e2e-matrix.md`
and `docs/28-tutorial-end-to-end.md` tell a reader to use, and it is the worse
case because that is where someone decides a change is safe.

**One place already failed safe**, and is worth not "fixing" into silence:
`dq_gate.py` asserts `rebuild() != 0` — it REQUIRES the build to fail on
poisoned data. A shrunken test set would make dbt exit 0 there and trip the
assertion loudly. Inverse gates are self-protecting; forward ones are not.

**The report's general suggestion — assert the expected probe COUNT — is already
satisfied for the engine matrix, by a stronger mechanism.** `run.py` regenerates
`docs/engine-matrix.md` and CI runs `git diff --exit-code` on it, so a probe that
stopped running would drop out of `out/jvm.json`, shrink `order`, remove a table
row and fail the diff. That pins the IDENTITY of all 25 probes rather than their
number. The suggestion stands for suites with no such committed artifact, which
is precisely what `dbt build`'s exit code is.

### Open — triaged, deliberately not changed
- ~~**The trigger `firingSet` is process-global**~~ — **resolved 2026-08-04 by
  checking what Fabric actually does**, rather than by picking a side of the
  trade. The entry below was written as "losing a duplicate firing is silent and
  bounded, losing the cycle guard is unbounded", which is true and was the wrong
  frame: it never asked whether Fabric suppresses at all.

  It does not. Activator's limitations page bounds a runaway by RATE — `Fabric
  item — Activations/user/minute — 50`, and "if an action exceeds the limit,
  Activator might throttle or cancel the action" — plus 10,000 events/second/rule
  above which "Activator stops your rule". Neither loop detection nor dedup
  appears anywhere in those docs. And the introduction states the concurrency
  case outright: Activator "sends information about what happened and continues
  monitoring without waiting for the action to complete", which "enables scalable
  workflows that can process many events simultaneously".

  So the suppression was an artifact of this emulator's synchronous dispatch, not
  a model of Fabric, and the duplicate-firing loss was a fidelity gap rather than
  a neutral cost. `firingSet` now counts activations instead of holding
  identities, cut at `maxTriggerActivations = 50` — Fabric's own number.
  Independent events each fire; a cycle climbs to the cap and stops.

  **Residual gap, stated rather than papered over:** the counter still cannot
  separate 50 NESTED activations from 50 CONCURRENT independent ones, for the
  same reason as before — synchronous dispatch, no goroutine-locals, and a chain
  token would reach every OneLake mutation. Raising the bound from 1 to 50
  shrinks the gap fiftyfold without paying for that plumbing; it does not close
  it. `TestIndependentEventsBothFire` pins the behaviour Fabric has,
  `TestTriggerCycleIsCut` pins the bound — including that ONE `leave` releases
  exactly ONE slot, which mutation testing showed the first version did not
  check, and a `delete` there would let a cycle run forever.
- **The witness checker proves presence, not coverage.**
  `scripts/check_witnesses.py` confirms a test *function name* or CI *job id*
  exists; it does not check that the test runs (some `t.Skip` without a DSN),
  that it asserts the claim, or that the job exercises the credited suite. A
  green row can cite a real-but-irrelevant witness — which is why the manifest
  carries hand-written `note` corrections. `boundary:` witnesses are counted but
  never dangling-checked. **Raised to MEDIUM by the coverage pass below**, which
  found two concrete instances.

### Also fixed (the lows)
- **The MLflow proxy read its upstream response unbounded**, alone among this
  package's body reads. It is relayed verbatim, so an attached server could drive
  allocation without limit and a truncated read would serve a partial result as a
  complete one. Bounded like the Kusto proxy beside it, and tested the same way —
  a server streaming past the ceiling, plus the companion proving an in-ceiling
  response still relays whole. `internal/api/mlflow.go`; tests
  `TestAnOversizedMLflowResponseIsRefusedNotRelayedShort`,
  `TestAnMLflowResponseInsideTheCeilingIsRelayedWhole`.
- **`RenameOneLakePath` counted its source rows outside its own transaction.** A
  concurrent delete in the gap produced a committed success that moved nothing.
  Counted inside now; `TestRenameIsAtomicWithItsExistenceCheck` asserts that every
  rename reporting success left a destination, and reproduced the phantom success
  5 runs of 5 beforehand. `internal/store/onelake.go`.
- **The TDS validator guard failed open.** `if s.Auth != nil` meant a server built
  without a validator accepted every token unvalidated, on a surface that hands
  out read/write T-SQL. It only fires on a construction mistake, which is when a
  security check should be loudest; rejecting matches `api.withAuth`.
  `internal/tds/server.go`; test `TestLoginWithNoValidatorIsRejected`.

### Coverage pass — what severity triage did not reach
Total statement coverage **90.70% → 91.13%** (deduplicated cross-package
profile, measured with a real SQL Server; the floor is 90%, so the margin was
thinner than the README's "~90%" suggests). The number moved little because most
of the work covered *branches* inside already-partly-covered functions. What it
surfaced is the point:

- **The lineage migration was broken and would fail any upgrade.**
  `relaxLineageJobFK` rebuilds `lineage_edges` so a warehouse write can be
  recorded without a Fabric job. Its `INSERT` supplied **12 values positionally
  into a 13-column table** — `source_kind` was added to the new shape after the
  migration was written — so the rebuild died with "has 13 columns but 12 values
  were supplied" and took startup with it. Invisible because every test opens a
  FRESH database, where the function returns at its first check and the rebuild
  never runs; only someone opening a database from an older build would hit it.
  Columns are named now. `internal/store/db.go`; tests
  `TestRelaxLineageJobFKUpgradesAnOlderDatabase` (old schema on disk, rows
  survive, job-less edges insert, the partial index still dedupes) and
  `TestRelaxLineageJobFKIsIdempotent`.
- **A 🟢 parity claim had no witness at all.** `parity.md` claims gold → semantic
  model with `producer: DirectLake`; `directLakeSource` sat at **0%** and nothing
  anywhere asserted such an edge. The existing Direct Lake fixtures create items
  through the store, which bypasses the handler that records lineage. 0% → 81%,
  driving the real `createItem` path, with the two absences that are half the
  claim also pinned: an import model records nothing, and a binding naming a
  lakehouse that does not exist records nothing. `internal/api/modellineage_test.go`.
- **The VS Code surface's authorization was never exercised.** `vscodeDo`
  hardcodes `admin`, so every refusal branch behind `vscodeAuthorizedItem` was
  dead. It is easy to read that file as a translation layer rather than a door,
  but every handler reaches the same item store the public API guards. Weakening
  the guard to resolve the item while discarding the role check produces **10
  distinct failures**. `internal/api/vscode_test.go`.
- **`viewFlow` had eight unreached branches** (`CREATE OR ALTER`, column lists,
  every malformed shape). A view is remembered and later resolved THROUGH, so a
  shape that silently fails to parse loses every downstream edge that would have
  gone via it — gold attributed to a scaffold instead of silver. 50% → 94%.
- **The view-rename lineage path.** dbt renames views as well as tables; if the
  memory does not move with the name the view is forgotten under the old one and
  unknown under the new. Deleting the three lines that move it makes the new test
  fail. Also pinned: a rename or `DROP` naming an unresolvable object leaves the
  graph alone — retiring provenance on a guess deletes something real, the
  destructive twin of inventing it. `rename` 58% → 92%.
- **`deployment.go`'s description was never written by a test.** The existing
  PATCH test proves an *absent* description is preserved, which passes just as
  well if the field is ignored. Clearing to `""` is asserted too: the field is a
  `*string` so "absent" and "set me to empty" stay distinguishable, and a handler
  testing the value rather than the pointer would silently refuse to clear.
- **`runPipeline` was deleted**, not tested: a one-line wrapper with no callers
  anywhere, including tests. Dead code at 0% is not a gap to fill.

**Uncovered is not the same as untested behaviour**, and the difference matters
more than the percentage. Of `deployment.go`'s 21 uncovered blocks, 17 are
`err != nil → return` plumbing and one is *structurally unreachable*: the
`ErrPairingAmbiguous` branch cannot fire through the store because
`ux_items_ws_name_type` forbids the duplicate that would cause it — `pairing.go`
documents this, `PairItems` is at 100% including that branch via hand-built item
sets, and both halves were verified before accepting it. Forcing a test through
it would prove nothing. The same is true of the `WriteMessage(...) → return err`
residue in `internal/tds`. Chase the behaviour, not the number.

### Done — two engine-matrix probes, and half the report was wrong
Reported 2026-08-04 from `contoso-data-platform`: Sail supposedly ACCEPTS AND
IGNORES two reader options. Measuring it against all three engines found **one
real gap and one false alarm**, and the false alarm is the more useful half.

- **`read.json(multiLine=True)` — real.** Sail's JSON reader is NDJSON-only, so
  a file that is one JSON array cannot be parsed; Spark JVM reads it. Now a red
  row in `docs/engine-matrix.md`.
- **`read.text(wholetext=True)` — NOT a Sail limit.** It passes on all three
  engines. The reported symptom (one row per LINE) reproduces exactly, but it is
  a PySpark spelling artifact: `DataFrameReader.text(path, wholetext=False, ...)`
  passes that DEFAULT into `_set_opts`, overwriting any `.option("wholetext", …)`
  set beforehand, so the `.option()` form cannot take effect on ANY engine.
  Measured on one JVM session against one file: `.option()` → 3 rows,
  `text(p, wholetext=True)` → 1 row. Kept as a GREEN row to keep it that way,
  the same reason the `_rn` row exists.

**The reference column is the tell.** Written the `.option()` way, the probe
turned every engine red — including Spark JVM, which honours the option. A red
cell on the reference engine means the probe is measuring itself, and that
check is what caught this before it was published as a Sail defect. It is the
mirror of the reporter's own first mistake (a single-line fixture, where
honoured and ignored give an identical row count, which is how `wholetext` was
first concluded to WORK). Both probes now use a multi-line fixture AND assert
the plain read first, so a broken fixture reports itself as a broken fixture.

Fixtures are written through the ENGINE rather than a client-side `open()`: on
the `sail` profile the probe runs in a different container and shares no volume
with the engine, so a client-written file would not be there to read.

Worth passing back to `contoso-data-platform`: their `wholetext` workaround is
unnecessary, and any code that reaches for `.option("wholetext", …)` is a no-op
wherever it runs. The `multiLine` workaround stands — read the page as text and
parse in-engine (`from_json` + `explode`) — with its own documented trap, that
`from_json` returns NULL on a schema mismatch rather than raising and the row
count comes from the array's length rather than its contents, so every
count-based assertion passes while every column is empty.

### Fixed — List Blobs could panic, and could never finish paging
Found by chasing coverage into `listBlobs`, not by a report. Both are reachable
from `?comp=list&delimiter=/&maxresults=N` — the shape object_store issues for a
directory listing — and both live in one line that computed the continuation
marker.

- **A page filled entirely by common prefixes panicked.** `next =
  blobs[len(blobs)-1].Name` indexes the emitted BLOBS, which is empty when
  everything folded into a directory. `index out of range [-1]`, from a query
  parameter any authenticated caller controls; net/http turns it into a dropped
  connection with no explanation.
- **Worse: paging never advanced.** When it did return a prefix as the marker,
  the next request filtered `name <= marker` — but a prefix is a DIRECTORY name
  and every blob under it sorts AFTER it, so nothing was skipped, the same
  prefix was re-derived, and the same marker came back. A client walking pages
  loops forever.

The marker is now the last entry CONSUMED rather than the last thing emitted,
and when a page ends inside a directory it already reported, it resumes past
that directory using the prefix's byte-wise upper bound (`dir` ends with the
delimiter, so incrementing the final byte bounds every name under it). A walk of
`a/1, a/2, b/1, c.txt` at `maxresults=1` now terminates in three pages with each
directory reported exactly once; before the fix it panicked, and with only the
panic repaired it looped.

`listBlobs` 90.5% -> 93.6%. Three mutations, three caught: restoring the index,
returning the bare prefix as the marker, and listing directory rows as blobs.

### Partly fixed — nested types corrupted the whole row, and `string` was the wrong type
Two findings, one from a consumer and one found while checking the first against
Microsoft's docs.

> **Read with the 2026-08-05 entry at the top of this file.** The displacement
> half below is fixed and confirmed on the released image. The omission half is
> not: `v0.16.0` served nested columns as `varchar` NULL rather than dropping
> them, and the warning this entry describes could not fire. Fixed in `v0.16.1`.

**Nested types did not fail to map — they corrupted.** `docs/16` called them
"not yet mapped", which reads as a blank. Measured in-repo on
`flat, lines array<struct<…>>, addr struct<…>, tags map<…>, after_flat bigint`:

    flat       -> "control"   correct, it is leaf 0
    lines      -> 8           lines.line_no of the SECOND element
    addr       -> "P-200"     lines.product_id
    tags       -> 4           lines.quantity
    after_flat -> "SG"        addr.country — and its real 999 was DROPPED

`readParquet` assigned parquet LEAF values by position into a slice sized by
TOP-LEVEL field count, so one nested column took someone else's leaf and shifted
every column after it. Nothing raised. Unlike the date bug there was **no loud
half at all** — a `SELECT` returns plausible values and a report carries them.

The consumer's report got the direction right and three details wrong, which is
why it was worth measuring rather than transcribing: it read the array as
becoming its LENGTH (their fixture had one element whose `line_no` was also 1,
so "length" and "first leaf" were indistinguishable); its `bigint`/`nvarchar`
types were pre-fix 0.15.3 surfaces; and it scoped the damage to one column when
in fact every column after the nested one is destroyed. The scrambling predates
the type-map fix — `git log -S` puts that guard at `7ce9e89`, and `f26c182` only
changed goValue's argument.

**Fabric's answer, from the docs rather than from taste:** "Types that aren't
listed in the table aren't represented as the table columns in the SQL analytics
endpoint", and "Some columns that exist in the Spark Delta tables might not be
available". So the column is OMITTED and the rest stay correct — not an error,
and certainly not a fabrication. `Table.Skipped` records the names, because
"some columns might not be available" is miserable to debug without one.

**And checking that turned up a divergence of my own.** The same mapping table
says `STRING -> varchar(8000)`, and of nvarchar: "Use char and varchar
respectively, as there's no similar unicode data type in Parquet." We emitted
`NVARCHAR(4000)`, this repo's own doc table asserted `nvarchar` as Fabric's
mapping, and I had told the consumer to PIN `nvarchar` in their probe and drop
their `varchar`/`nvarchar` tolerance. Their looseness was right and my
instruction was wrong. Fixed in `sqlType`, the doc table, the e2e, and the
probe. The collation Fabric declares (`Latin1_General_100_BIN2_UTF8`) is
deliberately NOT emitted — it changes comparison and sort semantics, which is a
larger change than a type name.

Four mutations, four caught: ignoring leaf indices (the original bug), keeping
nested columns, a constant `leafCount`, and reverting to nvarchar.

### Fixed — the Delta→SQL type map lost date, timestamp, binary and int
Reported 2026-08-04 from `contoso-data-platform`: a Spark `DateType` column
surfaced through the SQL analytics endpoint as `bigint`. Measuring it found
**three more of the same shape**, sharing one cause — `readParquet` kept only the
DECIMAL logical annotation and discarded the rest, so `date`, `timestamp` and
`int` all arrived as Go `int64` and all three reflected as `BIGINT`, while
`binary` arrived as a Go string. `sqlType`'s `[]byte → VARBINARY` arm was
therefore unreachable for anything read from Parquet.

It fails two ways and the quiet one is worse: a join dies with `Operand type
clash: date is incompatible with bigint`, naming neither the column nor the
cause, but `SELECT rate_date` just returns `20627` — a plausible integer that
nothing marks as a date. `Decimal` already had a special case here, added for
exactly this failure family ("reflecting a decimal as BIGINT drops the scale").

Fixed in both directions:
- **Read**: the whole logical annotation is carried, and `Date`/`Timestamp`
  become distinct Go types — the same shape as `Decimal` — so `sqlType` and
  `bulkValue` can both switch on them. INT32 keeps its width.
- **Write**: `colKind` gains date/timestamp/binary/int, and the kind now comes
  from the driver's **column metadata** rather than the scanned value, because
  `DATE` and `DATETIME2` both scan as `time.Time` and `INT` and `BIGINT` both as
  `int64` — value inference collapses each pair.

**A test that passed against the code it claimed to pin, again.** The first
end-to-end version built its `Table` with `Date{…}` values by hand, so deleting
the reader's date arm did not fail it — it exercised only the reflect half.
Rebuilt to start from Parquet bytes, it reproduces the report exactly:
`INFORMATION_SCHEMA` reports `int`, and the join fails with `Operand type clash`.
Three occurrences of this pattern in one session; mutation caught all three.

**Four expectations were pinning the old collapsing** and were updated rather
than worked around: two in `internal/warehouse`, plus the mirror e2es in
`internal/api` and `internal/server`. Those last two fail only with a SQL Server
attached, and their `if v, ok := r[i].(int64); ok` guard reported a type change
as `sum(id) = 0` — pointing at the data rather than the type. They now name the
mismatch.

**Decimal in the mirror direction followed**, closing the other half. It could
not be expressed by a kind at all — precision and scale are part of the type —
so the carrier became a `colType` struct, and the precision/scale come from the
driver's `DecimalSize()` rather than the value: the driver returns the printed
string, so `1.5` in a `DECIMAL(10,2)` has to become the unscaled `150`, not
`15`, and the two are indistinguishable by value. `MONEY`/`SMALLMONEY` report no
`DecimalSize` and are named explicitly, or they fall through to text. All three
physical widths are asserted, since delta-rs picks the encoding by precision.

One more self-inflicted trap worth recording: the first version of the
real-engine decimal test indexed the read-back table with the SOURCE table's
column positions. `encodeParquet` builds a `parquet.Group`, which is a map, so
the written schema is ordered by NAME — the mistake surfaced as a type panic
rather than a wrong value, which is the lucky version.

Documented as a table in `docs/16-warehouse-tds.md`. Still unmapped: the nested
types (`struct`/`array`/`map`), which have no kind at all.

**The honest type broke the catalog, one commit later.** Reporting a Delta
column as `binary` instead of collapsing it to `string` made the governance job
fail with `For column data types char, varchar, binary, varbinary dataLength
must not be null` — OpenMetadata refuses the WHOLE table for one such column,
so the table simply vanishes from the catalog rather than arriving degraded.
The fix belongs in the ingest, not in a revert: a real Fabric table with a
binary column would have broken it identically, so the emulator was merely the
first thing to produce one. `scripts/govern_ingest.py` now sends the width the
SQL analytics endpoint reflects (`VARBINARY(4000)`), since Delta declares none.

Nothing connected the two ends: the change was to a Go type map, the failure
appeared in an OpenMetadata container, and the only witness was an e2e needing
the full OpenMetadata stack — which cannot be run here at all while another
session holds port 8585. So the constraint now has a second, container-free
witness: `scripts/check_govern_types.py` drives the real ingest over every type
in its own map and fails on any column OpenMetadata would refuse. It is wired
into `make check` and CI, and it fails when the fix is removed. It does not
replace the e2e — it cannot observe OpenMetadata's actual behaviour, only that
the payload satisfies the documented constraint.

### Fixed — the Spark image installed a JDK that could not coexist with its own
The engine-matrix job went red and STAYED red on 2026-08-04, and it was not the
Maven 429 that had flaked it twice earlier the same day:

    openjdk-11-jdk-headless : Depends: openjdk-11-jre-headless (= 11.0.27+...)
                              but it is not going to be installed
    E: Unable to correct problems, you have held broken packages.

`apache/spark:3.5.3` ships a **Temurin JRE as a tarball** under
`/opt/java/openjdk` (11.0.24) rather than as an apt package, so Ubuntu 20.04's
JDK insists on installing its OWN `openjdk-11-jre-headless` at a different
version, and apt will not reconcile two JREs. The install was therefore never
sound — it worked only while the versions happened to line up, and broke the
moment the 20.04 index moved to 11.0.27. A hard break caused by someone else's
release, which is worse than a flake because retrying cannot clear it.

The JDK existed to `javac` one file. It now comes from `eclipse-temurin:11-jdk`
into a **build stage**, so no package manager is involved and no compiler
reaches the runtime image. `curl` was in the base all along, so the apt line was
redundant for that too, and the image no longer touches the Ubuntu index at all.

Verified by rebuilding and re-running the JVM probe leg: 24 of 25 pass — the
one failure is the row the committed matrix already records as ❌ — and
`docs/engine-matrix.md` regenerates byte-identical, which is precisely what
CI's `git diff --exit-code` asserts. The cold path (empty jar cache,
`--no-cache`) builds clean too, and `javac` is confirmed absent from the
runtime image.

**The guard then became the thing it was guarding against.** Importing the real
ingest also imports `requests`/`urllib3`/`yaml`, which live in the `governance`
dependency group, so `make check` died with `ModuleNotFoundError` on Windows and
macOS — a check added to catch a governance break, breaking the build itself on
two platforms. It passed locally for the reason these keep passing locally: this
machine happens to have `requests`. It now stubs ONLY what is genuinely absent,
so the real modules are used where they exist, and the absent case is verified
by blocking those imports outright rather than by hoping.

### Fixed — the witness checker now resolves which witnesses can skip
The MEDIUM raised by the coverage pass. It cost real time twice in one session:
`TestWarehouseSQLServerRelayE2E` skips without `WAREHOUSE_MSSQL_DSN`, so it never
ran on a laptop and a CRITICAL fix broke it undetected until CI; and the Direct
Lake parity row sat 🟢 with its code at 0%. A test whose *name* exists is not a
test that *ran*.

`scripts/check_witnesses.py` now resolves gating to a **fixed point**, which is
the part that matters: three of the nine gated witnesses do not skip and contain
no gate — `TestReflectDecimalColumn` calls `testsupport.OpenMSSQL(t)`, which
skips on its behalf, and `TestMirrorSnapshotsBaseTables` reaches the same skip
*two* levels down through `newMirrorDB()`. That transitive form is the one no
reader spots, and a one-level check misses it. Three rules, all enforced by the
existing `--strict` in `make check` and `ci.yml`, and each verified by breaking
the manifest and watching it fail:

- an **undeclared** gate — every skippable witness must be named in the new
  `_gated` map with its reason, so adding one is deliberate rather than a silent
  downgrade of the evidence behind a green row;
- a **stale** declaration, for a witness that no longer skips — a stale note is
  how the map drifts back out of step;
- a claim whose witnesses are **all** gated, which is a green row a default
  `go test ./...` proves nothing about. That count is 0 today and the rule keeps
  it there.

All nine current gates are the same one — `WAREHOUSE_MSSQL_DSN`, satisfied by
the `build` job's SQL Server service — so the report groups by reason rather
than repeating it nine times and burying the one that is undeclared.

**The new rule immediately caught the checker itself.** `make check` went red on
windows-latest reporting every gate stale — because it had parsed **0 claims**.
`Path.read_text()` uses the LOCALE encoding on Windows, and cp1252 decodes the
parity map's 🟢 bytes to mojibake *without raising*, so no row ever matched: 95
rows match when the file is read as UTF-8, none when it is read as cp1252. Every
count printed 0 and every rule was vacuously satisfied, so the job had been
green for its whole life while checking nothing. Fixed by reading every file as
UTF-8 explicitly, and by failing when the parse yields zero claims — a checker
that parses nothing must never pass, which is the same "presence is not
evidence" principle it exists to enforce, applied to itself.

**And that fix was half a fix.** Reading was pinned to UTF-8; PRINTING was not,
so the next Windows run died on `UnicodeEncodeError: 'charmap' codec can't
encode character '\u2192'` — the arrow in the very report added alongside the
first fix. The general problem is not the arrow: this script prints text taken
from the parity map, which is full of em dashes, so any claim name reaching an
error list would do the same. `sys.stdout` is now reconfigured to UTF-8, and the
decoration is ASCII regardless. Reproducible without Windows —
`PYTHONIOENCODING=cp1252` fails the old script and passes the new one — and all
three check scripts are verified under it.

**Still not proved**, and worth stating plainly rather than implying the gap is
closed: that a witness ASSERTS its claim, and that the code behind the claim
executes at all. Coverage answers the second — the Direct Lake row is the
recorded case — and nothing answers the first.

### Second coverage pass — the untested capability surfaces
Total **89.35% → 89.84%** measured without a SQL Server sidecar (`ci.yml` records
the sidecar's effect: 91.2% with an engine against 89.4% without, so the whole
`internal/warehouse` family at 0% locally is *gated*, not untested — the top of a
naive "uncovered statements" ranking is entirely that gate and is a trap).

The pass targeted the functions whose *capability* nothing witnessed, and found
one defect:

- **A single-file TMDL semantic model was rejected.** `definitionPart` falls back
  to an item's sole definition part when the requested path is absent — correct
  for notebooks, whose content part is named inconsistently by publishing
  clients, and wrong for a lookup whose bytes are then parsed as a *specific
  format*. Asking for `model.bim` returned the only `.tmdl` file, which failed as
  TMSL, so the user saw `invalid TMSL model: invalid character 'a'` about a file
  containing no JSON — and the TMDL branch was never reached. The identical model
  split across two `.tmdl` files always worked, which is exactly why nothing
  caught it: every TMDL fixture in the tree is a folder with more than one part.
  Fixed with `definitionPartExact` at the one call site that misfires;
  `TestExecuteQueriesReadsASingleFileTMDLModel` fails without it.
- **TMDL had no consumer witness at all.** `internal/semanticmodel` proves
  `ParseTMDL` correct in isolation, but neither consumer executed it:
  `parseModelDefinition` 50% → 93.8% and `parseModelParts` 50% → 83.3%. The only
  test that mentioned TMDL asserted that it *loses* to TMSL. Since a `.pbip`
  project from Power BI Desktop **is** TMDL, this was the serialisation a real
  client is most likely to arrive with, unproven end to end.
- **What the emulator tells the Spark agent.** `registerLakehouseTables` 32% →
  92%, `bindDefaultLakehouse` 0% → 100%. Both are best-effort — they log and
  return rather than failing session creation — so nothing upstream observes
  them and a silent regression stays silent. Neither needs a real agent: an
  `httptest` server is a complete stand-in for the request the emulator decides
  to send. The mutation that binds the catalog to the engine's own warehouse
  directory instead of OneLake reproduces the exact bug the source comment
  describes, and is now caught.
- **The flow stream's transport edges.** `emulatorEvents` 79.1% → 89.6%: the
  non-Flusher refusal, `Last-Event-ID` resume, giving up on a dead connection,
  and the dropped-gap notice. `net/http` always supplies a Flusher and a healthy
  client never stalls, so none of it is reachable over real HTTP — driving
  `Server.Handler()` with a `ResponseWriter` under test control is what made it
  observable.

- **Lineage from a source system was never rendered.** `portalLineage` 66.7% →
  90%. An edge whose source is a *connection* resolves its name through a
  different table — `GetItemByID` returns nothing for a connection id — and that
  entire branch sat at 0%. It is the medallion's FIRST hop: every ERP/CRM/
  reference feed in the advanced demo reached the graph through code checked
  only by eye. Weakening it labels the node `""` instead of "Contoso ERP (SQL)".
- **The tenant-settings refusals.** `updateTenantSetting` 70.7% → 87.8%: the
  documented `enabled`-is-required rule, the malformed-body 400, the unknown-
  setting 404, and the reference's own contradiction rule (canSpecifySecurityGroups
  false MEANS org-wide, so naming groups contradicts itself). The three
  delegation flags are set ONE AT A TIME, because three near-identical patch
  blocks are the shape where a copy-paste error assigns the wrong field and every
  value is a bool, so a swap still round-trips something plausible.

**Not every low percentage is a gap.** `internal/tlscert` sits at 77.1% and was
left there: all 8 uncovered statements are crypto/IO error returns
(`ecdsa.GenerateKey`, `rand.Int`, `CreateCertificate`, `MarshalECPrivateKey`,
`WriteFile`) unreachable without fault injection, and the real behaviours —
persist, reuse for a stable fingerprint, MkdirAll failure, regeneration over
corrupt PEMs — were already tested. What it was *actually* missing was
behavioural: **nothing asserted the private key's file mode**. `TestPersistedKeyIsNotWorldReadable`
fails when 0600 becomes 0644, and adding it moved coverage by exactly **zero
percent**, because the `WriteFile` call was already executed by every other test
in the file. The mode argument is not a branch. This is the counter-lesson in
its sharpest form: the number would never have pointed here.

**A test can pass against the code it claims to pin.** `TestFlowStreamStopsWhenTheClientIsGone`
passed with the event-send path's write-error check deleted, because the handler
was escaping through the *keepalive* write instead — the fixture sets a 100ms
keepalive. Pushing it out of reach (`EventKeepalive = time.Hour`) makes the send
path the only exit, and the mutation then fails it. Same class as the witness
finding above: green is not evidence unless something was verified to turn it
red. Every test added in this pass was checked by mutating the source it covers.

### Context for the entry below
The 2026-07-14 pass reported "docs now track implementation", and that claim went
**104 commits** without re-verification — `executeQueries`, RTI/Kusto, Reflex
event triggers, MirroredDatabase, MLflow, and the whole flow-observability and
lineage surface all landed after it. `parity.md` itself kept pace (last updated
2026-08-03 and it covers all of them), so this was a stale *log*, not code ahead
of parity. Its reference to `17-parity.md` is also stale: the file is now
`docs/parity.md`.

## 2026-07-14 — reconciliation pass

A second audit (4 parallel doc-vs-code reviews) plus a full reconciliation pass.
Since the 2026-07-13 snapshot, warehouse **T4/T5** shipped (TDS session splice +
Microsoft ODBC Driver 18 / dbt-fabric), and new e2e witnesses landed (`azcopy`,
`dbt-fabric`, `dbt-fabricspark`). The docs were then updated to match code
throughout. **Docs now track the implementation.**

### Resolved in this pass (docs corrected to match code)
- **03-architecture** — non-goals no longer claim "notebooks/pipelines don't run"
  (real compute reframed as opt-in sidecars); TDS + OneLake-Blob surfaces added to
  the surfaces table + mermaid; P2 identity handshake no longer "future".
- **07-control-plane-api** — Jobs section reflects real pipeline/notebook execution;
  added `queryactivityruns` / `notebookRun` / `notebookRunResult` / folders endpoints
  and the Livy/Spark data plane; connection credentials + `/v1/connections` flipped
  from "planned" to shipped; `GET /workspaces` no longer claims pagination it lacks.
- **08-onelake** — Shortcuts flipped "planned" → shipped, with honest caveats
  (read-only resolution; writes don't follow target; no `isShortcut` field; not in
  listings). Added the Blob dialect + Delta put-if-absent commit section.
- **06-data-model-and-seed** — removed the fictional `workspace.state` enum/column
  and `items.folder_id`; added `pipeline_runs` / `notebook_runs` / `shortcuts`;
  fixed `operations` (no stored `status`; `fail_with`) and `folders.parent_id`.
- **04-configuration** — added the four real-compute flags (`-spark-livy-url`,
  `-spark-agent-url`, `-sql-tds-addr`, `-warehouse-sql-url`).
- **05-tls-and-hosts** — added the `onelake.blob.fabric.microsoft.com` SAN.
- **12-e2e-matrix** — added the `dbt-fabric` (ODBC Driver 18) witness; two-driver TDS note.
- **10-testing** — coverage figure aligned to "90% floor (currently ~95%)".
- **13-roadmap** — R3 marked T1–T5 done with the session splice; native Livy
  (`--spark-agent-url`) path documented (was "deferred, with cause").
- **14-real-compute** — reframed "Status: design" → "largely shipped (R0–R5)";
  Track C FedAuth-over-TDS shipped in-repo (not a future sibling repo); Track B
  native Livy; Track E honest leaf-activity subset; dbt matrix + non-goals + phasing.
- **16-warehouse-tds** — header "T4 next" → "T1–T5 shipped"; removed the
  deferred-type-fidelity contradiction; fixed the borrowed-oracles section (two
  driver witnesses) and the dangling `e2e/warehouse-tds/` / `e2e/sql-endpoint-spike/`.
- **Structural** — renamed `15-parity.md` → **`17-parity.md`** (resolves the
  duplicate `15-` prefix) and added `16-warehouse-tds` + `17-parity` to the
  Starlight sidebar (both were orphaned from nav).
- **README** — status now covers the R (real-compute) track.

### Implemented in the code pass (each with a correctness test)
Three genuine missing Fabric features were implemented and verified:
- **Pipeline retry backoff.** `retryIntervalInSeconds` is now applied as virtual
  wall-clock folded into the run's `durationInSeconds` (deterministic, no real
  sleep). `internal/pipeline/activities.go`; tests `TestRetryBackoffAccumulates`
  + assertion in `TestRetryPolicySucceedsAfterRetries`.
- **ForEach sequential/parallel.** `isSequential` + `batchCount` honored; the
  container reports the right wall-clock (sequential = sum, parallel = sum of
  per-batch maxima). `internal/pipeline/activities.go`; test
  `TestForEachParallelDuration`.
- **List pagination.** Opt-in `?maxPageSize` continuation-token paging on all
  list endpoints (`writePage`). `internal/api/pagination.go`; tests
  `TestListPagination` + `TestPageTokenDecodeGarbage`.

### Still open — deliberate non-implementations (documented honestly)
Not "docs lagging code" — these are decisions, documented as-is:
- **Shortcut *writes* don't follow target RBAC**, aren't in DFS listings, no
  `isShortcut` field. Write-through-shortcut semantics vary by target type in real
  OneLake; the read path resolves with target RBAC, writes hit the source. Kept as
  a documented limitation rather than guessing the write semantics.
- **Web activity real HTTP / Script / Stored procedure leaves** — Web would make
  arbitrary outbound HTTP from a pipeline definition (out of scope for the
  hermetic emulator); Script/StoredProcedure need the warehouse SQL sidecar wired
  into the pipeline executor (a container weight class). Documented as unwired.
- **Workspace `state`** — the documented 6-value enum was fictional; removed from
  docs. Not implementing a made-up field.
- **E1 real Airflow sidecar** — not built (a heavy sidecar; roadmap/real-compute
  mark it planned).
- Minor / faithful-subset: `guid()` returns a constant zero UUID (deterministic
  distinctness would need a per-run counter threaded through the pure expression
  funcs — disproportionate); multi-hop shortcut cycles aren't rejected on create,
  but read resolution is single-hop so there is no loop at runtime.

## Context notes
- `docs/` is canonical; `website/src/content/docs/` is **generated** at build time
  by `website/scripts/sync-docs.mjs` (not git-tracked). Only ever edit `docs/`.
- Renames/additions must update the Starlight sidebar slugs in
  `website/astro.config.mjs`, or the docs-site build fails.
