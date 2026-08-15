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
      capacityId check. **ARM consume** is opt-in `FABRIC_ARM_URL`: ARM-created
      `Microsoft.Fabric/capacities` appear on `GET /v1/capacities`; standalone
      seed is unchanged.)
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
- [x] e2e: the **real `fabric-cicd` Python tool** (v1.3.x) publishes into the
      emulator — `e2e/fabric-cicd/run.py` (self-contained: both emulators +
      venv + driver). Works unmodified via its own `FABRIC_API_ROOT_URL` /
      `DEFAULT_API_ROOT_URL` overrides + in-process DNS pin (our TLS cert
      covers `api.fabric.microsoft.com`). Driving it surfaced and fixed real
      gaps: `/v1/workspaces/{id}/folders` (now implemented), `description`
      always present on item wire shapes, result-less LROs must not advertise
      a result Location, and fabric-cicd refuses workspaces with no
      `capacityId`. Remaining: wire into CI once the GitHub remote exists.
- [x] e2e: **Microsoft's Fabric CLI (`fab`)** drives the control plane —
      `e2e/fabric-cli` (containerized, `fab` v1.6+): service-principal auth
      (its MSAL flow against entra-emulator, which *is* `login.microsoftonline
      .com` via a compose alias + `FAB_API_ENDPOINT_FABRIC` for the API), then
      workspace + item CRUD (Notebook / SemanticModel / Report / DataPipeline /
      Lakehouse), `ls` / `get`, and the raw `api` passthrough — all unmodified.
      Driving it surfaced and fixed a real gap in **entra-emulator**: MSAL/ADAL
      validate the authority via `GET /common/discovery/instance` before every
      token, which returned 404; entra-emulator now serves it (v0.2.2). That
      same fix removes the root cause of `azcopy`'s static-token workaround
      (its MSAL authority validation now succeeds against the emulator).
- [x] **Deployment pipelines** — the third item in this phase's own header,
      now shipped end to end (D0–D3).
      **D0** (pipeline/stage model + read surface: 2–10 ordered stages,
      default Development/Test/Production, per-pipeline RBAC where the creator
      is Admin and non-members get 404, stage→workspace assignment with live
      name resolution and `ON DELETE SET NULL`). **D1** (assign/
      unassign workspace + real item pairing: pairs are item-id edges between
      adjacent stages that survive renames on either side, recomputed only at
      assign — never lazily at read time). **D2** (Deploy Stage
      Content over the existing LRO engine, deploy-all and selective, both
      directions between adjacent stages, per-item detail on
      `/operations/{id}/result`, plus the deployment-operations history).
      **D3** (role-assignment CRUD; Admin is the only role a pipeline
      defines, mutations require Admin while reads require membership).
      e2e: **Microsoft's `fab` CLI drives the whole promotion flow**
      (`e2e/fabric-cli`), following the call order of Microsoft's own
      `DeploymentPipelines-DeployAll.ps1` — list → stages → deploy → poll →
      result — which independently confirmed the wire contract.
      Fabric has exactly two CI/CD mechanisms, git integration is one and
      stage-to-stage promotion is the other. Designed in
      [23-deployment-pipelines.md](23-deployment-pipelines.md) (D0 model+read →
      D1 assignment+pairing → D2 deploy over the existing LRO engine → D3 role
      assignments). Most of the work is *pairing*, which is persistent state
      surviving renames — not a name match — and deployment copying **metadata
      only, never data**, and **not** deleting target-only items the way
      `updateFromGit` does. D1 turned the first open fidelity question into a
      measurement: the documented ambiguous-pairing case is **structurally
      unreachable** here — the `28e4a4c` UNIQUE index forbids duplicate
      name+type in a workspace, and items carry no folder membership, so the
      documented tie-breaker has no data. `PairItems` is a pure function so
      that branch is still unit-tested against inputs the store cannot
      produce. The second question (whether deploy overwrites the target's
      display name) stays open for the conformance oracle.

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
      workspace); ADLS Gen2/Amazon S3 external read-through via Connections;
      Dataverse 501; dangling target 404; self-cycle rejected. Store + API +
      OneLake resolution and container-network reads tested.
- [x] e2e: the **real Azure Blob SDK** (`azure-storage-blob`) round-trips
      through the emulator — `e2e/adls-sdk` (3-OS): uploads a pyarrow Parquet,
      downloads it byte-identical (found + fixed the `x-ms-range` gap), lists
      blobs, DFS sees the same file.
- [x] e2e: the **real `azcopy` binary** transfers through the emulator —
      `e2e/azcopy` (Linux): multi-block upload (Put Block + Put Block List),
      byte-identical download, DFS sees the same object. Auth is a forged
      Storage token handed to azcopy in its static-token mode (`TokenStore`),
      since azcopy's own MSAL flow validates the authority against public AAD.

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
- [x] **R1+R2 (merged) — containerized Spark**, its own focused runway.
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
    - [x] **B (high-concurrency Livy sessions)** — Fabric's *own* layer on top
      of the Livy contract (`highConcurrencySessions`, current — it gained HC
      support in 2026): the emulator implements the packing manager directly
      (`internal/api/livy_hc.go`), since a vanilla Livy server has no REPL/HC
      concept. `sessionTag` packs REPLs into a shared underlying Livy session,
      capped at **5 REPLs/session** with spill-to-new-session; acquire is
      non-idempotent (same tag → distinct HC ids, shared `sessionId`);
      acquire/get/delete are pure control-plane (no Spark); a REPL's statements
      run on **real Spark** via the native agent (`--spark-agent-url`) — or proxy
      to an external Livy backend (`--spark-livy-url`), honest 501 without either.
      Unit-tested (packing, cap, spill, slot-reuse-after-release, RBAC) + a server
      e2e proving the HC routes win over the classic catch-all on the real mux,
      race-clean.
    - [x] **B (native Livy sessions on real Spark)** — Apache Livy is **retired
      to the Apache Attic** (no maintained image to bundle for the protocol), so
      rather than proxy it, the emulator *terminates* the Livy contract itself
      and drives a **Spark statement-executor agent** via `--spark-agent-url` /
      `FABRIC_SPARK_AGENT_URL` (`internal/api/livy_native.go`,
      `python/spark_agent/agent.py`): sessions, statements, and batches run on **real
      Spark**, session state persists across statements, and HC REPLs each get
      their own agent namespace — so the 5-REPL model is real end to end
      (`e2e/livy`, containerized). `--spark-livy-url` still reverse-proxies an
      external Livy backend if a user brings one; unset → honest 501.
- [x] **R3 (SQL analytics endpoint — DuckDB)** — real DuckDB runs SQL
      (aggregation, join, filter) over Delta tables in the OneLake plane,
      `e2e/duckdb` (3-OS): delta-rs writes two Delta tables into OneLake,
      DuckDB queries them and the results match — the lakehouse↔warehouse SQL
      interop, cross-engine. (DuckDB embeds via CGO, which the pure-Go
      distroless build forbids, so the SQL engine runs in the e2e, not the
      binary; the storage read is byte-proven by the delta-rs e2e.)
    - [x] **R3 (T-SQL / TDS warehouse)** — **T1–T5 done.** Designed in
      [16-warehouse-tds.md](16-warehouse-tds.md). A pure-Go TDS endpoint
      (`internal/tds`, `-sql-tds-addr`) terminates Entra FedAuth (token
      validated vs entra's JWKS, `database.windows.net` audience); **T3**
      reflects a lakehouse's Delta into a real **SQL Server** sidecar
      (`-warehouse-sql-url`) — read `Tables/<t>` Delta in pure Go
      (`internal/warehouse`), `CREATE`+`INSERT` the rows; `SELECT` matches DuckDB
      (R3/C1), the cross-engine oracle. **T4** routes each item to its own
      database (Lakehouse read-only / Warehouse read-write, isolated), enforces
      RBAC→SQL permissions, and delivers `information_schema` parity, native
      per-column type fidelity, and connect-by-name. **T5** replaces the
      per-batch relay with a **session splice** (`internal/tds/splice.go`,
      `client.go`): after terminating FedAuth, the client's post-login session is
      **byte-forwarded** to a real per-item SQL Server connection, so the engine
      emits every token natively (transactions, RPCs, prepared statements). That
      unlocks a second, independent driver family — Microsoft **ODBC Driver 18**
      — so Microsoft's real **dbt-fabric** adapter passes `debug→seed→run→test`
      end to end (`e2e/dbt-fabric`), alongside `go-mssqldb`. **Not PolyBase:** a
      spike proved SQL Server reading OneLake Delta directly is a dead-end on
      Linux (the object-storage connector components aren't shipped), so
      reflection is the permanent design. (Front in this repo; engine a compose
      sidecar.)
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
    - [x] **R4 (real notebook cell execution)** — a RunNotebook job is parsed
      by the emulator (real Go parser, `internal/notebook`: `notebook-content.py`
      → ordered code cells, magics/markdown handled) and executed by **real
      Spark**: `e2e/notebook-run` publishes a Fabric notebook, real JVM Spark
      runs its cells against the OneLake plane (a Delta table actually lands),
      and the engine reports per-cell results + exit value back so the job's
      terminal status reflects the real run — not the clock. The
      parse/record/report contract is Go-side + unit-tested; the compute is
      real Spark (Linux-only e2e, reusing the spark-a2 image). Without an engine
      the cells are honestly "parsed, Pending".
    - [x] **R4a (default-lakehouse session binding)** — notebook metadata is
      resolved and validated; Sail and JVM witnesses run unqualified table APIs
      against the attached lakehouse's OneLake `Tables/` directory.
    - [x] **R4b (VS Code Fabric-extension compatibility)** — the private
      shared-backend/MWC protocol used by Microsoft's Fabric Data Engineering
      extension 1.18.1 is implemented for workspace/artifact discovery,
      Notebook/SparkJobDefinition/Environment authoring, notebook content with
      ETag conflicts, notebook resources, Spark-job history/cancel, and
      lakehouse table discovery. `e2e/vscode-extension` replays the pinned
      extension contract through the real `api.powerbi.com` host alias with an
      entra-emulator Power BI-audience token. Interactive Jupyter kernel
      websockets and table preview remain out of scope; notebook execution is
      available through the existing jobs/Livy/Sail surfaces.
- [x] **R5 (DataPipeline interpreter)** — a real, pure-Go interpreter
      (`internal/pipeline`) for Fabric/ADF Data Pipeline definitions: the full
      expression language (a faithful subset — `pipeline()`, `variables()`,
      `activity()`, `item()`, and the string/logic/math/array function library
      with ADF-loose coercions), control flow (IfCondition, ForEach, Until,
      Switch, Filter, Fail), variables (Set/Append), `dependsOn` with all
      four dependency conditions (Succeeded/Failed/Completed/Skipped),
      **Invoke pipeline** (ExecutePipeline — real recursive interpretation of a
      referenced DataPipeline, with parameter flow, `waitOnCompletion`, and a
      cycle guard), and per-activity **policy** (retry re-runs a failed activity
      and records `retryAttempt`; timeout fails an over-running attempt —
      deterministic, no real sleeping). Wired
      into the jobs API: a `POST …/jobs/instances?jobType=Pipeline` on a
      DataPipeline item executes the definition now, a pipeline failure sets
      the job's terminal status, and `…/jobs/instances/{jid}/queryactivityruns`
      returns the per-activity run detail. The notebook leaf activity
      (TridentNotebook) chains a real RunNotebook job — pipeline → jobs →
      notebook, end to end. Proven by interpreter unit tests, API-level job
      tests, and a server e2e (real entra token → auth → RBAC → interpreter →
      queryactivityruns). Coverage floor held at ≥90%. A malformed expression
      fails the activity (recovered), never the server.
    - [x] **R5 (real data-plane leaf activities)** — leaves that can run for
      real, hermetically where possible: **Copy** moves real bytes
      OneLake→OneLake (a file, or a directory subtree, with expression-resolved
      `{workspaceId?, itemId, path}` locations); **Lookup** reads real rows from
      a CSV/JSON/Parquet file or a lakehouse **Delta** table (`Tables/<name>`,
      auto-detected) and feeds `@activity(…).output`; **GetMetadata** stats a
      real path (`exists`/`itemType`/`size`/`lastModified`/`childItems`);
      **Script**/**SqlServerStoredProcedure** run real T-SQL against a
      Warehouse/SQLDatabase item's own SQL Server database (Track C's backend),
      targeted the same `{workspaceId?, itemId}` way as Copy/Lookup.
      Web, WebHook, REST, Salesforce and Custom (Azure Batch) run for real
      (Web and Custom can be refused with `FABRIC_WEB_ACTIVITY=stub` /
      `FABRIC_CUSTOM_ACTIVITY=off`). External stores on Copy stay
      refused by name.
    - [x] **R5 (Apache Airflow + Dataflow Gen2 boundary)** —
      `ApacheAirflowJob` item/file APIs sync Python DAGs into an attached
      Airflow 2.10.5/Python 3.12 sidecar, unpause and trigger them through the
      upstream REST API, then derive Fabric job completion from the real DAG
      run. `e2e/airflow` proves real scheduler + executor + task execution with
      a shared DAG volume. The sidecar is opt-in through `--airflow-url` and
      `--airflow-dag-dir`; an unattached engine fails explicitly. `Dataflow`
      management/definition round-trip is supported, while Refresh/Publish and
      in-pipeline Dataflow execution fail with
      `DataflowEngineNotImplemented`: Power Query M is proprietary, so there is
      no engine to attach and no result is faked. Designed in
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
      (Fabric's ten: Anonymous / Basic / Key / KeyPair / OAuth2 /
      ServicePrincipal / SharedAccessSignature / Windows /
      WindowsWithoutImpersonation / WorkspaceIdentity)
      with write-only secrets, SP validation against entra at create
      (`skipTestConnection` bypass), and the WorkspaceIdentity kind gated on a
      provisioned identity.
- [x] **Vault-backed credentials**: a `KeyVaultSecretReference` on
      `keyReference`/`passwordReference`/`tokenReference`/`servicePrincipalSecretReference`
      resolves the secret via the workspace identity's vault-audience token
      against [azure-keyvault-emulator](https://github.com/calvinchengx/azure-keyvault-emulator)
      — `workspace identity → entra token → vault secret → connection`, offline
      (`internal/akv`; only the pointer is stored, never the value).

## S — Engine swap: LakeSail replaces JVM Spark

Designed in [20-lakesail-engine.md](20-lakesail-engine.md): Sail (Rust Spark
Connect) becomes the engine behind every compute surface — PySpark with no JVM.

- [x] **S0** — proof: `e2e/sail` (CI `sail`, Linux): real PySpark Connect
      client → Sail → Delta write/SQL/append through the OneLake Blob surface
      (az:// + endpoint override, the delta-rs recipe), entra
      client-credentials minted by the `docker/sail` launcher. Conditional-PUT
      Delta commits ride the R0 contract unchanged.
- [x] **S4 (pulled forward)** — user-facing compose: the default
      `docker-compose.override.yml` and the explicit
      `docker-compose.compute.yml` both run Sail + the thin Connect agent
      (JVM `apache/spark` image dropped); validated end-to-end over the
      compose network incl. self-signed-TLS storage writes. Fabric builds
      from the tree until a post-Blob-surface release is tagged.
- [x] **S1** — `e2e/livy` + `e2e/dbt-fabricspark` composes swapped to Sail
      (`apache/spark:3.5.3` gone; agents are Spark Connect clients; under
      Sail `sc` is a guide-rail stub, and the local-relation limit override
      keeps `createDataFrame` working for user code).
- [x] **S2** — `e2e/notebook-run` runner connects via `SPARK_REMOTE`; the
      notebook fixture executes **unmodified** (abfs:// URLs and
      `createDataFrame` included); JVM image build dropped.
- [x] **S3** — `e2e/spark` (A2) reborn on Sail with the same
      production-shaped `abfs://` URLs; `EntraTokenProvider.java` + the JVM
      Dockerfile deleted. The **default** path has no JVM; the opt-in
      overlay (`docker-compose.spark-jvm.yml` / `make up-jvm`) later
      brought JVM Spark back for RDD, checkpointed streaming, and
      Java/Scala UDFs. Tradeoff accepted on the default: the Hadoop-ABFS
      driver witness lives on that overlay, not on Sail.

## After S — shipped on the same contract

These landed after the Sail swap and are CI-verified. They are not a new
phase letter; they extend surfaces the earlier phases already named.

- [x] **Eventstream** — Apache Kafka KRaft sidecar (`--profile eventstream`),
      Fabric notebook API (`format("kafka")` + `eventstream.*`), Custom HTTP
      produce. **Lakehouse destination** appends produce payloads as Delta;
      **Reflex destination** fires `Microsoft.Fabric.Eventstream.EventReceived`
      as a real `EventTriggered` job. **Eventhouse destination** ingests
      via Kusto `.create-merge` + `.ingest inline` (direct, not Fabric
      streaming ingest). **Operators** (Filter, GroupBy, tumbling Window)
      run on the produce batch. [51-eventstream-kafka.md](51-eventstream-kafka.md).
- [x] **Custom activity on by default** — Azure Batch `command` runs on the
      Spark agent; `FABRIC_CUSTOM_ACTIVITY=off` restores the refusal.
- [x] **Fabric Core MCP** — `POST /v1/mcp/core`, unmodified Python `mcp` SDK
      in CI.
- [x] **Optional `msmdsrv` DAX oracle** — `FABRIC_DAX_URL` relays
      `executeQueries` to a pump in front of Desktop's engine on a machine
      you own. Not a compose default. [52-msmdsrv-hosts.md](52-msmdsrv-hosts.md).

## Sequencing note

Build the **LRO engine before anything that mutates** — every workspace/item/git
call returns through it, so getting `202` → poll → terminal right once makes all
later endpoints trivial. P2's entra-side dependency (#16) has already shipped,
so phase order is a pure prioritization choice, not a blocking one.
