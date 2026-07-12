# 13 — Roadmap

Scope chosen: **full** — control plane through the OneLake data plane, matching
entra-emulator's surfaces (portal + Starlight docs + distribution), composed via
docker-compose. Each phase is independently useful and CI-verified (real-SDK
e2e, like entra-emulator's SDK matrix).

## P0 — the spine (token acceptance + workspaces + items + RBAC + LRO)

The minimum that lets someone test **SP → Fabric client-credentials** automation.

- [x] Token acceptance: validate Bearer against entra-emulator JWKS/issuer;
      audience set; `oid`/`appid` extraction. (`--entra-issuer`, `--entra-jwks-url`)
- [x] Store + migrations (`workspace`, `item`, `role_assignment`, `operation`).
- [x] **LRO engine** on the controllable clock (`202`/`x-ms-operation-id`/
      `Location`/`Retry-After`, `GET /operations/{id}` + `/result`).
- [x] Workspaces CRUD. (`assignToCapacity` deferred at P0; the capacity model
      is now designed in [07-control-plane-api.md](07-control-plane-api.md)
      (`## Capacities`) — seeded default capacity, auto-assign on create,
      assign/unassign LROs — validated as needed by fabric-cicd's
      capacityId check.)
- [x] Generic items CRUD (create-with-definition → 202 LRO).
- [x] RBAC: role assignments CRUD + enforcement (Admin/Member/Contributor/Viewer;
      creator becomes Admin; Member grants ≤ Member).
- [x] Fault injection + clock control (`/_emulator/clock`, `/_emulator/faults`).
- [x] Health; Docker image; docker-compose with entra-emulator.
- [x] e2e: in-process entra-emulator mints a real client-credentials token for
      the Fabric audience; full workspace/RBAC/item/LRO flow over HTTP.

## P1 — CI/CD (the primary draw)

Makes `fabric-cicd`, git integration, and deployment pipelines run offline.

- [x] Item **definitions**: `getDefinition` (200) / `updateDefinition` (202 LRO),
      parts round-trip verbatim.
- [x] Typed item aliases (12 collections: notebooks, lakehouses, warehouses,
      dataPipelines, semanticModels, reports, environments, eventhouses,
      kqlDatabases, sparkJobDefinitions, mirroredDatabases, eventstreams).
- [x] **Connections**: `GET/POST /v1/connections` — git `connect` with a
      service principal requires a `connectionId` (SPs may not use `Automatic`).
- [x] **Git integration**: connect / initializeConnection / status / commitToGit /
      updateFromGit / disconnect / myGitCredentials, backed by a local
      per-branch definition store (logical ids preserved across commits;
      updateFromGit mirrors: creates, replaces definitions, deletes stale).
- [x] Jobs: `jobs/instances?jobType=` trigger (202 + Location) + clock-derived
      `NotStarted→InProgress→Completed/Failed` + cancel (`Cancelled`).
- [x] e2e: two-workspace git round-trip over HTTP (commit from one, update
      into another, definitions intact); job lifecycle on the frozen clock.
- [x] e2e: the **real `fabric-cicd` Python tool** (v1.2.x) publishes into the
      emulator — `e2e/fabric-cicd/run.py` (self-contained: both emulators +
      venv + driver). Works unmodified via its own `FABRIC_API_ROOT_URL` /
      `DEFAULT_API_ROOT_URL` overrides + in-process DNS pin (our TLS cert
      covers `api.fabric.microsoft.com`). Driving it surfaced and fixed real
      gaps: `/v1/workspaces/{id}/folders` (now implemented), `description`
      always present on item wire shapes, result-less LROs must not advertise
      a result Location, and fabric-cicd refuses workspaces with no
      `capacityId`. Remaining: wire into CI once the GitHub remote exists.

## P2 — the identity handshake (deepest entra integration)

The "works seamlessly with entra-emulator" payoff. Its dependency —
entra-emulator roadmap #16 — **has already shipped**: the workspace-identity
object (`internal/store/fabric.go`, states `Active/Provisioning/Failed/
Deprovisioning`, name-follows-workspace, cascade delete), admin CRUD at
`/admin/api/workspace-identities`, internal token minting at
`GET /fabric/workspaceidentities/{id}/token`, and acceptance of both Fabric
audiences. P2 can start any time; it consumes those endpoints over HTTP.

- [x] Workspace-identity lifecycle: `POST /v1/workspaces/{id}/provisionIdentity`
      / `deprovisionIdentity` (202 LRO) drive entra's admin API over HTTP
      (`internal/entra` client, origin derived from the issuer). Rename
      follows the workspace; workspace delete cascades the identity; the
      identity appears as `workspaceIdentity{applicationId,servicePrincipalId}`
      on the workspace shape.
- [x] The provisioned identity's SP is granted **Admin on its workspace**, so
      tokens entra mints for it (`GET /fabric/workspaceidentities/{id}/token`
      — customer never holds a credential) pass RBAC back here. Deprovision
      revokes the grant.
- [x] Audit event parity: entra-side — its token mint emits
      `Retrieved Fabric Identity Token for Workspace` (covered by its tests).
- [x] e2e: provision → entra mints for the identity → the identity's token
      reads its workspace and creates items in fabric-emulator; rename-follows
      verified in entra; deprovision revokes; workspace delete cascades.

## P3 — OneLake data plane

- [x] `onelake.` host mux (Host-routed like real Fabric): ADLS-Gen2 subset —
      PUT create file/directory, PATCH append/flush (position-checked), GET
      read, HEAD properties, filesystem listing (`?resource=filesystem`,
      `directory=`, `recursive=`, non-recursive collapses to first-level
      dirs), DELETE (directories take their subtree).
- [x] `Storage`-audience token acceptance (separate validator over the same
      JWKS; fabric-audience tokens are rejected on the data plane and vice
      versa). Workspace RBAC applies: Viewer reads, Contributor writes.
- [x] Managed-folder enforcement (`onelake-api-parity.md`): HEAD-only at
      account/workspace level; item root + first level protected from
      create/rename/delete; `setAccessControl`-class params rejected; banned
      headers ignored + echoed via `x-ms-rejected-headers`; canned
      `$superuser` / `---------` permission response headers.
- [x] Name- and GUID-addressing resolve to the same workspace/item.
- [x] e2e: full write flow (create → append ×2 → flush) via GUID addressing,
      read back via name addressing; listings; RBAC walls; managed-folder
      rejections — against real entra-minted Storage tokens.
- [x] Shortcuts: OneLake-to-OneLake symlinks — create/list/get/delete
      (`internal/api/shortcuts.go`); data-plane read/HEAD resolution through
      the target with **target-side RBAC** (the trusted-workspace-access
      path: a read through a shortcut is authorized against the TARGET
      workspace); external targets 501; dangling target 404; self-cycle
      rejected. Store + API + onelake resolution tested.
- [x] e2e: the **real Azure Blob SDK** (`azure-storage-blob`) round-trips
      through the emulator — `e2e/adls-sdk` (3-OS): uploads a pyarrow Parquet,
      downloads it byte-identical (found + fixed the `x-ms-range` gap), lists
      blobs, DFS sees the same file. (azcopy, a heavier Go binary, is still a
      later add; the SDK path is the higher-value proof and it's done.)

## R — Real compute (PySpark, Delta, warehouse)

Designed in [14-real-compute.md](14-real-compute.md): attach **real engines**
below the emulated planes — never fake results. Lives in this repo (storage
completeness + e2e harnesses + compose sidecars); only a future TDS-FedAuth
proxy would be a separate sibling.

- [x] **R0** — OneLake storage completeness: the Blob-endpoint dialect
      (`internal/onelake/blob.go` — Put Blob / staged blocks / Copy / List
      Blobs XML paging, reached via `onelake.blob.*` or the account-prefixed
      `/onelake/{ws}/…` path), Range reads (206) on both surfaces, ETags +
      put-if-absent conditional writes (Delta `_delta_log` atomicity), DFS
      rename (`x-ms-rename-source`), ETag/Last-Modified on every path. e2e
      **A1** (`e2e/delta-rs`, CI): real `deltalake` writes v0 → reads back →
      appends v1, and the same files list through the DFS surface. Hardened
      with a **concurrent-commit race test** (24 goroutines race one
      `_delta_log` file; exactly one wins — the mechanism-level atomicity
      oracle, `-race`-clean) and `x-ms-range` support (found by the ADLS SDK).
- [ ] **R1+R2 (merged) — containerized Spark**, its own focused runway.
      *Decision:* R1 (in-process PySpark via ABFS) is folded into R2's Spark
      sidecar rather than pursued standalone. The Hadoop **JVM ABFS driver**
      derives its endpoint from the URI authority and takes no host/port
      override — unlike delta-rs's `azure_endpoint` (A1 ✅) or the Blob SDK's
      `account_url` (ADLS-SDK e2e ✅), both of which redirect cleanly. Only a
      **container network** where `onelake.dfs.fabric.microsoft.com` resolves
      to the emulator solves that structurally — and that's the R2 shape
      anyway, the production-faithful path, and the one that gives Windows
      users a real story (JVM stays in a Linux container; the client is thin).
      A separate weight class (a multi-hundred-MB Spark image + Docker
      orchestration), so it gets its own session rather than blocking the
      pure-wheel oracle work.
    - [x] **A2** — real JVM Spark + delta-spark write and read a Delta table
      (2 commits) via the ABFS driver onto the OneLake plane, `e2e/spark`
      (compose: entra + fabric-from-source + Spark; a custom token provider
      bridges ABFS's v1 `resource=` to entra's v2 `scope=`; a seeded storage
      resource app resolves the audience). Found + fixed a real bug: **ABFS
      sends append/flush as `PUT ?action=…`, not `PATCH`** — the flush PUT
      (empty body) was truncating every file to zero, silently corrupting
      Delta commits (regression-tested). Linux-only CI.
    - [x] **B (Livy passthrough contract)** — the documented endpoint
      (`…/lakehouses/{id}/livyapi/versions/2023-12-01/{sessions,batches}/…`)
      is a bearer-validated, RBAC-gated reverse proxy (`internal/api/livy.go`)
      to a real Apache Livy backend set via `--spark-livy-url` /
      `FABRIC_SPARK_LIVY_URL`. Session-create and job-submit need Contributor;
      status reads need Viewer; unknown lakehouse 404s. Unset → honest 501.
      Unit-tested (path rewrite, RBAC matrix, 501, lakehouse) + a server e2e
      (real entra token → auth → RBAC → proxy → backend).
    - [ ] **B (real Livy sidecar e2e)** — *deferred, with cause:* Apache Livy
      is **retired to the Apache Attic** (last release 0.8.0, no maintained
      image), so there is no maintained real engine to bundle for the Livy
      *protocol* specifically — the honest 501 default is correct, and the
      proxy works against any Livy-compatible backend a user brings. Real
      Spark *execution* is already proven by A2 (Spark writes/reads real
      Delta). Revisit only if a maintained Livy-compatible server appears.
- [x] **R3 (SQL analytics endpoint — DuckDB)** — real DuckDB runs SQL
      (aggregation, join, filter) over Delta tables in the OneLake plane,
      `e2e/duckdb` (3-OS): delta-rs writes two Delta tables into OneLake,
      DuckDB queries them and the results match — the lakehouse↔warehouse SQL
      interop, cross-engine. (DuckDB embeds via CGO, which the pure-Go
      distroless build forbids, so the SQL engine runs in the e2e, not the
      binary; the storage read is byte-proven by the delta-rs e2e.)
    - [ ] **R3 (T-SQL / TDS warehouse)** — *deferred, with cause:* a real
      T-SQL engine (Babelfish/SQL Server) over TDS with Entra FedAuth is a
      large separate effort (the C2/C3 sidecar + TDS-FedAuth proxy). DuckDB
      already proves the SQL-analytics-endpoint *semantics*; the TDS wire +
      FedAuth is the heavy add, tackled by demand as its own repo.
- [x] **R4 (notebook developer loop)** — a functional `notebookutils` /
      `mssparkutils` shim (`python/notebookutils`, stdlib-only) that makes
      real Fabric notebook code run unchanged against the emulator family:
      `fs` over OneLake (create→append→flush, ranged reads, ls, cp — abfss
      URIs *and* lakehouse-relative paths), `credentials.getToken` for any
      audience, `credentials.getSecret` brokered through the real
      azure-keyvault-emulator, the `lakehouse` control plane, `runtime.context`,
      and `notebook.run` via the jobs API. Proven by `e2e/notebookutils`
      (3-OS): entra + fabric + azure-keyvault up, a real notebook drives every
      module to a PASS. Designed in [14-real-compute.md](14-real-compute.md)
      (Track D).
    - [ ] **R4 (VS Code Fabric-extension + Spark-session semantics)** —
      *deferred:* default-lakehouse session binding on the Spark sidecar and
      the VS Code Fabric extension are additive; the shim already closes the
      author-run-read loop.
- [ ] **R5** — pipelines: real Apache Airflow sidecar behind ApacheAirflowJob
      items (Fabric's own code-first orchestrator IS Airflow; AKV secrets
      backend → azure-keyvault-emulator); DataPipeline interpreter with
      documented control-flow semantics and real-engine leaf activities
      (Notebook→Spark, SQL→warehouse, Copy→scoped real movement); Dataflow
      Gen2 and unscoped connectors 501. Designed in
      [14-real-compute.md](14-real-compute.md) (Track E).

## Cross-cutting (throughout)

- [x] Svelte portal: dashboard / workspaces (items, role assignments, git
      status drill-down) / operations / clock / fault injection / workspace
      identities — served at `/` on the control-plane origin, reading state
      through unauthenticated `/_emulator/portal/*` endpoints (the /v1
      contract stays bearer-only). `go:embed all:dist` + committed `dist` +
      CI drift guard; 21 Vitest unit tests.
- [x] Starlight docs site on GitHub Pages (this `/docs` = source of truth,
      synced by `website/scripts/sync-docs.mjs`; pinned Astro Starlight;
      deploys via `docs-site.yml`) — live at
      <https://calvinchengx.github.io/fabric-emulator/>.
- [x] GoReleaser: binaries + distroless Docker (GHCR, HEALTHCHECK via the new
      `healthcheck` subcommand) + Homebrew cask + winget (both self-skip
      without their tokens); `version` stamped via ldflags. Channels go live
      at the first `v*` tag.
- [x] Playwright headless mount smoke (catch builds-but-doesn't-mount) — in the portal CI job, with the vite `resolve.conditions` fix baked in.
- [x] Coverage parity with entra-emulator (≥ 70% per package): every package
      77–100% from its own tests; 91.6% total plain / 93.5% cross-package
      (CI floor 90%).
- [x] **Connection credentials**: `credentialDetails.credentialType`
      (Basic / ServicePrincipal / WorkspaceIdentity / Key / SAS / Anonymous)
      with write-only secrets, SP validation against entra at create
      (`skipTestConnection` bypass), and the WorkspaceIdentity kind gated on a
      provisioned identity.
- [x] **AKV-reference connections**: `AzureKeyVaultReference` credential
      resolves the secret via the workspace identity's vault-audience token
      against [azure-keyvault-emulator](https://github.com/calvinchengx/azure-keyvault-emulator)
      — `workspace identity → entra token → vault secret → connection`, offline
      (`internal/akv`; only the pointer is stored, never the value).

## Sequencing note

Build the **LRO engine before anything that mutates** — every workspace/item/git
call returns through it, so getting `202` → poll → terminal right once makes all
later endpoints trivial. P2's entra-side dependency (#16) has already shipped,
so phase order is a pure prioritization choice, not a blocking one.
