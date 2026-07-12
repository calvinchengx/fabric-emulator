# Implementation-vs-docs audit — 2026-07-13

Snapshot audit comparing code to docs across all subsystems (6 parallel
reviews: control plane/identity, OneLake, Spark/Livy/notebooks, warehouse/TDS,
pipelines, and cross-cutting docs/CI). **Headline:** code is in good shape and
runs *ahead* of the docs — most gaps are docs lagging a fast-growing codebase
(reality exceeds claims), plus a short list of genuine over-claims to reconcile.
`15-parity.md` is the most accurate doc; `14-real-compute.md`, `13-roadmap.md`,
`12-e2e-matrix.md`, and `README.md` are the stalest.

> Note: a concurrent session is actively rewriting docs (13/15/16), so some
> items below may already be resolved — re-check before acting.

## P1 — genuine over-claims (docs promise more than code delivers)

Each needs a decision: **[doc]** correct the wording to match code, or **[code]**
implement the promised behavior.

- [ ] **Shortcut *writes* don't follow target RBAC.** Only GET/HEAD resolve
  shortcuts (`internal/onelake/onelake.go:374,390`); PUT/PATCH/DELETE
  (`:357,371,404`) write the *source* item, no target check. Also: shortcuts are
  invisible in `resource=filesystem` listings (`list()` never calls
  `ShortcutFor`), and the API emits no `isShortcut` field. Doc: `08-onelake.md:62-67`.
  → decide **[doc]** (document read-only shortcut resolution) or **[code]**.
- [ ] **Pipeline retry backoff is not applied.** `runWithPolicy`
  (`internal/pipeline/activities.go:23-56`) retries *instantly*;
  `Policy.RetryIntervalInSeconds` is parsed (`pipeline.go:38`) but dead. Doc
  claims "backoff exercised in milliseconds on the clock" (`15-parity.md:100`).
  → **[doc]** drop the backoff claim, or **[code]** apply virtual backoff via the clock.
- [ ] **ForEach parallel mode absent.** `runForEach`
  (`activities.go:244-261`) is sequential-only; no `isSequential`/`batchCount`.
  Doc lists "ForEach (sequential/parallel)" (`14-real-compute.md:226`).
  → **[doc]** or **[code]**.
- [ ] **Workspace `state` documented but unimplemented.** No `state` column
  (`internal/store/db.go:53-59`) or field (`types.go:6-13`). Doc: `06:13,66`
  (6-value enum). → **[doc]** remove, or **[code]** add.
- [ ] **List pagination documented but absent.** `GET /workspaces` (and items /
  capacities / connections / folders) return the full set, no continuation
  token (`internal/api/workspaces.go:13-23`). Doc: `07:18`. → **[doc]** or **[code]**.
- [ ] **pyodbc warehouse oracle over-claimed.** CI runs only `go-mssqldb`
  (`ci.yml` `warehouse-tds`); pyodbc is still an optional T5. Doc `16:238-245`
  presents it as exercised. → **[doc]** move to "future", or **[code]** add the pyodbc e2e.

## P2 — hygiene

- [x] **`cover.out` untracked + gitignored; `/tmpprobe/` gitignored.** Done
  (commit eb56141).
- [ ] **Duplicate doc number 15.** Both `docs/15-parity.md` and
  `docs/15-entra-companion.md` exist (sequence 14, 15, 15, 16). Rename
  `15-parity.md` → `17-parity.md` — coordinated 3-touch: the file, the Starlight
  sidebar (`website/astro.config.mjs` uses **explicit slugs** → add
  `{ slug: '17-parity' }`), and cross-refs in `13`/`16`. Verify with
  `pnpm --filter fabric-emulator-portal-site build` (name TBD) so the site still builds.
- [ ] **Dangling e2e dir references** (dirs don't exist): `e2e/sql-endpoint-spike/`
  (in `13`, `15-parity`, `16`) and `e2e/warehouse-tds/` (in `16`). Real warehouse
  tests live in `internal/server/tds_*_test.go`; the PolyBase dead-end lives in
  doc prose, not a dir.

## P3 — stale docs (code does more; update to catch up)

- [ ] **`README.md` understates the project.** Status says "P0–P3 shipped",
  omitting the entire **R (real-compute)** track (Spark/ABFS, native Livy + HC
  packing, DuckDB, TDS warehouse, pipeline interpreter, notebook cell execution).
  (`README.md:35`.)
- [ ] **`12-e2e-matrix.md` badly stale** — lists delta-rs/Spark/ADLS as "queued,
  not yet wired" (`12:22-28`); **8 e2es run in CI** (delta-rs, adls-sdk, spark,
  duckdb, livy-native, notebookutils, notebook-run, warehouse-tds) + a new
  untracked `e2e/dbt-fabricspark/`. Rewrite against the actual CI jobs.
- [ ] **`14-real-compute.md` describes proxy-only Livy + aspirational pipelines.**
  Code is *native* Livy driving a Spark agent (`internal/api/livy_native.go`;
  `livy.go:86-89` checks the agent first). The Track-E leaf table (`14:231-237`)
  over-claims vs the honest shipped subset — Lookup reads OneLake CSV/JSON (not a
  warehouse), Web is a stub (not real HTTP), Copy is OneLake→OneLake only. Align
  with `15-parity.md` (the accurate one).
- [ ] **`13-roadmap.md` narrates proxy-only Livy and defers the "sidecar e2e"**
  that's now done (`e2e/livy` + `--spark-agent-url`). Update the R-track boxes.
- [ ] **`15-entra-companion.md:39-43`** calls azure-keyvault-emulator a *planned*
  third member; it's integrated (roadmap `[x]`, parity 🟢, in the notebookutils CI job).
- [ ] **Connection credential model documented as "planned"** (`07:163-187`) but
  fully built: per-type validation, write-only secrets, entra test-probe, AKV
  resolution (`internal/api/git.go:388-529`).
- [ ] **OneLake Blob surface entirely undocumented.** `onelake.blob.*` + `/onelake`
  path: Put Blob, block staging, Copy, List Blobs XML (`internal/onelake/blob.go`);
  also x-ms-range/206, DFS rename, PUT-append/flush, HEAD-on-file, and the
  Contributor/ReadAll RBAC gate. Missing from `08`/`05`/`03`; cert-SAN list
  (`05:19-24`) omits `onelake.blob.fabric.microsoft.com` (code cert has it).
- [ ] **Warehouse doc 16 header/§4 contradict its own body** — header "T4 next"
  and §4 "deferred: schema isolation, type fidelity" are both **done** (per-item
  `EnsureDatabase`, INTN/FLTN/BITN). Only write-back is genuinely deferred. Also
  undocumented: the FEATUREEXTACK(FEDAUTH) token emitted for ODBC compatibility.
- [ ] **Undocumented endpoints/config** — `queryactivityruns`, `notebookRun(Result)`,
  folders, typed collections; flags `FABRIC_SPARK_AGENT_URL`, `FABRIC_SQL_TDS_ADDR`,
  `FABRIC_WAREHOUSE_SQL_URL` (missing from `04`/`07`).

## Minor / cosmetic

- [ ] Coverage figure disagrees across docs (95% / 93% / 91.6% / 91%+). Keep only
  the 90% floor as source of truth.
- [ ] Entra provision body key: code sends `workspaceName`, doc `09:15` shows `name`.
- [ ] `guid()` returns a constant zero UUID (`funcs.go:69-70`) — note under "faithful subset".
- [ ] `07` path-prefix style is mixed (`/v1` on some rows, not others).
- [ ] `16` diagram typo "SSML" → SSMS.
- [ ] `getDefinition`/`listRoleAssignments` min-role (Contributor/Member) not labeled in `07`.
- [ ] Multi-hop shortcut cycles not detected (only direct self-target); `08:70` overstates.

## What matches well (no action)

Token validation (issuer/audience/JWKS/exp-nbf), RBAC ranks + Admin-gating, the
full identity handshake, the LRO envelope, verbatim definition round-trip; DFS
surface + x-ms-range + managed folders + name/GUID addressing; the pipeline
control-flow interpreter + expression language + dependsOn + Invoke recursion;
the warehouse two-surface design (reflection vs the PolyBase dead-end, per-item
DB isolation, RBAC, connect-by-name, type fidelity); notebook cell execution and
HC-Livy packing. PolyBase is confirmed **not** implemented anywhere (a recorded
dead-end / non-goal only).

## Context notes

- `docs/` is canonical; `website/src/content/docs/` is **generated** by
  `scripts/sync-docs.mjs` on `pnpm dev`/`build`. Only ever edit `docs/`.
- Doc fixes above were **held** on 2026-07-13 because a concurrent session was
  live-editing `13`/`15`/`16`. Re-check those before editing.
