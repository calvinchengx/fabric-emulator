# Implementation-vs-docs audit

## 2026-07-14 — reconciliation pass (this update)

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
