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
- **LOW — the witness checker proves presence, not coverage.**
  `scripts/check_witnesses.py` confirms a test *function name* or CI *job id*
  exists; it does not check that the test runs (some `t.Skip` without a DSN),
  that it asserts the claim, or that the job exercises the credited suite. A
  green row can cite a real-but-irrelevant witness — which is why the manifest
  carries hand-written `note` corrections. `boundary:` witnesses are counted but
  never dangling-checked.
- **LOW** — unbounded read of the upstream MLflow response body (the sibling
  Kusto proxy bounds its own); `RenameOneLakePath`'s existence check sits outside
  its transaction; a nil TDS validator fails open rather than closed (latent —
  production always sets one).

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
