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

### Still open — genuine code gaps (documented honestly; implement on demand)
These were over-claims; the docs now state the real behavior. Implementing them is
optional future **[code]** work:
- **Shortcut *writes* don't follow target RBAC**, and shortcuts aren't in DFS
  listings (only GET/HEAD resolve; no `isShortcut` field). `internal/onelake/onelake.go`.
- **Pipeline retry *backoff* not applied** — `retryIntervalInSeconds` parsed but
  dead; retries fire instantly. `internal/pipeline/activities.go`.
- **ForEach parallel mode absent** — sequential-only. `internal/pipeline/activities.go`.
- **List pagination absent** — `GET /workspaces` (+ items/capacities/…) return the
  full set, no continuation token. `internal/api/workspaces.go`.
- **Workspace `state` unimplemented** — no column/field (removed from docs).
- **Pipeline leaf activities:** only Lookup / GetMetadata / Copy (OneLake→OneLake) /
  Invoke are real; Web is a stub, Script/StoredProcedure unwired.
- **E1 real Airflow sidecar** — not built (roadmap/real-compute mark it planned).
- Minor: `guid()` returns a constant zero UUID (faithful-subset note); multi-hop
  shortcut cycles not detected (only direct self-target).

## Context notes
- `docs/` is canonical; `website/src/content/docs/` is **generated** at build time
  by `website/scripts/sync-docs.mjs` (not git-tracked). Only ever edit `docs/`.
- Renames/additions must update the Starlight sidebar slugs in
  `website/astro.config.mjs`, or the docs-site build fails.
