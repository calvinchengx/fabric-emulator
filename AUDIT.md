# Implementation-vs-docs audit

## 2026-08-04 — code audit (this update)

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

### Open — triaged, deliberately not changed
- **The trigger `firingSet` is process-global** where it models a per-chain call
  stack, so two *independent* concurrent writes matching one trigger start one
  job instead of two. Dispatch is synchronous on the writer's goroutine, so a
  cycle genuinely is one stack — but Go has no goroutine-locals, and threading a
  chain token through the store's write API to scope it properly would reach into
  every OneLake mutation. Losing a duplicate firing is silent and bounded; losing
  the cycle guard is unbounded recursion, so the trade points this way on
  purpose. `TestTriggerCycleIsCut` pins it; the code now names the cost so this
  stays a decision rather than something to drift into.
  `internal/api/triggers.go`.
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

### Open — filed, not yet done: two engine-matrix probes
Reported 2026-08-04 from `contoso-data-platform` while building a second source
system. **Sail accepts two JSON/text reader options and silently ignores them** —
the false-green class the matrix exists to catch, not a missing feature. Real
Fabric Spark honours both, so code that works on Fabric fails here, and code
written against Sail can pass locally for the wrong reason.

- `read.text(wholetext=True)` — **the higher-value probe, because it never
  raises.** Measured on a multi-line NDJSON file: plain `.text()` → 255000 rows,
  `wholetext=True` → 255000 rows. Honoured would be one row per FILE. JVM gives
  1 row. Silent divergence; worth a `T.` capability flag, since nothing in the
  API tells a caller the option did not take effect.
- `read.json(multiLine=True)` on a file that is one JSON array — Sail raises
  `Expected JSON record to be an object, found Array`; JVM parses. Loud, easy
  to assert.

**The fixture is the whole difficulty.** The reporter's first probe used one
line per file, where "honoured" and "ignored" produce an identical row count —
it reported `wholetext` as working. Any probe added here MUST use an input with
more than one line per file or it will certify a capability that is not there.
`delta_change_data_feed` (`e2e/engine-matrix/probes.py:108`, registered as
`delta.cdf` at line 395) is the existing accepted-but-inert precedent to follow.

Not done because `docs/engine-matrix.md` is generated and CI diffs it
(`git diff --exit-code` in the `engine-matrix` job, `ci.yml:152`), so this needs
a real run of all three engines — `sail`, `sail-delta`, `jvm` — not a hand-edit.
The probe count in the prose derives from `len(order)`, so it moves on its own.
Verified before filing: neither `wholetext` nor `multiLine` appears anywhere in
`e2e/` or `docs/` today, so this is a genuine gap rather than a duplicate row.

Workaround worth documenting wherever this lands: read the page as text and
parse in-engine (`F.from_json` + `explode`), keeping the data path distributed.
Caveat — `from_json` returns NULL on a schema mismatch rather than raising, and
the row count comes from the array's length rather than its contents, so every
count-based assertion still passes while every column is empty. Guard it with a
"no column may be entirely NULL" check plus a test pinning the declared field
names against the vendor's OpenAPI spec.

### Open — raised in severity by this pass
- **MEDIUM (was LOW) — the witness checker proves presence, not coverage.**
  Promoted because it cost real time twice in one session. `TestWarehouseSQLServerRelayE2E`
  skips without `WAREHOUSE_MSSQL_DSN`, so it never ran locally and a CRITICAL fix
  broke it undetected until CI; and the Direct Lake parity row sat 🟢 with its
  code at 0%. A test whose *name* exists is not a test that *ran*, and a witness
  that exists is not a witness that *covers*. Worth having the checker report
  skipped tests, or the manifest record which witnesses are gated.

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
