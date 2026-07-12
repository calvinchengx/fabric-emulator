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
- [ ] Shortcuts: OneLake-to-OneLake symlinks with data-plane resolution and
      target-side RBAC (the trusted-workspace-access smoke path); external
      targets 501. Designed in [07-control-plane-api.md](07-control-plane-api.md)
      (`## OneLake shortcuts`).
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
    - **A2** — real PySpark + delta-spark write Delta via ABFS onto the
      OneLake plane; cross-engine read-back with delta-rs (A1) and pyarrow —
      Spark's JVM ABFS client and delta-rs's Rust object_store agreeing on
      our bytes.
    - **B** — Livy passthrough on the documented endpoint
      (`…/lakehouses/{id}/livyapi/versions/2023-12-01/{sessions,batches}`)
      delegating to the sidecar; opt-in real `RunNotebook` mode
      (`--spark-livy-url`) with job status from the actual batch; token
      passthrough scopes incl. `Code.AccessAzureKeyvault.All` →
      azure-keyvault-emulator.
- [ ] **R3** — warehouse: DuckDB+delta SQL over the same OneLake files (SQL
      analytics endpoint semantics); Babelfish/SQL Server sidecar per
      Warehouse item (SQL-auth compromise documented); TDS-FedAuth proxy only
      by demand, as its own repo.
- [ ] **R4** — the notebook developer loop: functional `notebookutils` shim
      (fs → OneLake, credentials → entra + azure-keyvault-emulator, notebook
      run → jobs/Livy); default-lakehouse session semantics on the Spark
      sidecar; VS Code Fabric-extension compatibility investigation.
      Designed in [14-real-compute.md](14-real-compute.md) (Track D).
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
