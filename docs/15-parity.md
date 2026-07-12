# Feature parity: fabric-emulator vs. real Microsoft Fabric

How the emulator's surface maps to real Fabric (as documented at
[learn.microsoft.com/fabric](https://learn.microsoft.com/en-us/fabric/) /
[MicrosoftDocs/fabric-docs](https://github.com/MicrosoftDocs/fabric-docs)), and
— the point of this table — **whether real work happens or just the API shape**.

The emulator's design bet is that the durable, testable surface is
*contracts + storage + identity + orchestration*, and those are done for real
(real signed JWTs, real Delta bytes on disk, real RBAC, a real pipeline
interpreter, real cross-engine SQL, real Livy high-concurrency session packing).
The heavyweight or proprietary **compute engines** are either bring-your-own
(Spark behind the Livy proxy — which is how Fabric itself layers a Livy endpoint
over Spark) or honestly stubbed.

**"Real via our own wire-protocol implementation."** A row is 🟢 **Real** not
only when an external engine/client does the work, but also when the emulator
*itself* implements Fabric's wire protocol and the logic behind it — so a real,
unmodified client gets byte- and behaviour-identical responses. Fabric's
control plane, OneLake's ADLS/Blob surfaces, the Data Pipeline expression
language + control flow, and the Livy **high-concurrency** session-packing layer
are all in this category: no engine is being proxied, yet the observable
contract matches real Fabric because we built the protocol, not a mock of it.
Where a row's *execution* still needs a heavyweight engine (a REPL's Spark
statements, a notebook's cells), that part is split out as 🟠 BYO-engine or 🔴.

## Legend

| | Meaning |
|---|---|
| 🟢 **Real** | Genuine work: real signed JWTs, real bytes on disk, a real engine/client computes, real logic enforced — no pretending. |
| 🟡 **Emulated** | Faithful API contract + persisted state, but no engine — status is clock-derived / management-only. |
| 🟠 **Bring-your-own-engine** | Real when a real external engine is attached (Spark via the Livy proxy; notebook cells on the Spark sidecar); contract-only (honest 501) otherwise. |
| 🔴 **Not implemented** | Honest 501 or absent. |

## Platform / fundamentals

| Fabric feature | Emulator | Type |
|---|---|---|
| Workspaces CRUD | Full | 🟢 Real (state persists) |
| Items CRUD + 12 typed collections | Full | 🟢 Real |
| Role assignments / workspace RBAC | Enforced from the validated bearer principal | 🟢 Real |
| Folders | Full | 🟢 Real |
| Capacities (list, assign / unassign) | Full state, no billing/SKU enforcement | 🟢 Real state |
| Long-running operations (202 → poll) | Clock-derived | 🟡 Emulated |
| Item **job execution** (`jobs/instances`) | Generic items: status clock-derived. **DataPipeline** jobs really run the interpreter (see Data Factory) and set terminal status from the run | 🟡 Emulated / 🟢 Real (pipelines) |

## Identity & security (`security/`, `admin/`)

| Fabric feature | Emulator | Type |
|---|---|---|
| Entra OAuth2 tokens / JWKS / client-credentials | entra-emulator mints **real signed JWTs** | 🟢 Real |
| Workspace managed identity handshake | Provisioned via entra admin API; the identity's own token passes RBAC | 🟢 Real |
| Key Vault references in connections | Resolved against azure-keyvault-emulator | 🟢 Real |
| Tenant settings / audit / admin-portal APIs | — | 🔴 Not implemented |
| Purview / lineage / sensitivity labels (`governance/`) | — | 🔴 Not implemented |

## OneLake (`onelake/`)

| Fabric feature | Emulator | Type |
|---|---|---|
| ADLS Gen2 DFS surface (create → append → flush, ranged read, list) | Full, incl. the `x-ms-range` dialect | 🟢 Real (real bytes) |
| Blob surface | Full | 🟢 Real |
| Delta commits (put-if-absent atomicity) | Real; `-race`-tested concurrent-commit race | 🟢 Real |
| Shortcuts (OneLake → OneLake) | Symlinks with target-side RBAC (trusted-workspace-access) | 🟢 Real |
| Shortcuts to external targets (S3 / ADLS Gen2 / Dataverse) | — | 🔴 501 |

## Data Engineering (`data-engineering/`)

| Fabric feature | Emulator | Type |
|---|---|---|
| Lakehouse item + Tables/Files storage | Full (via OneLake) | 🟢 Real |
| Notebook authoring / definition round-trip | Full | 🟢 Real |
| `notebookutils` / `mssparkutils` (fs, credentials, getSecret, lakehouse, runtime) | Functional stdlib shim (`python/notebookutils`) | 🟢 Real |
| Spark session / batch via the **Livy API** | Reverse-proxy to real Spark when `--spark-livy-url` is set, else 501 | 🟠 BYO-engine |
| Notebook **cell execution** | The emulator parses the notebook into cells (real Go parser) and records/serves the run; **real Spark executes the cells** against OneLake and reports back, finalising the job's status + exit value (`e2e/notebook-run`, real Delta lands). Cells stay "parsed, Pending" if no engine runs | 🟢 parse+run-record / 🟠 Spark exec |
| Livy **High-Concurrency** (5-REPL) sessions | Fabric's own packing layer, implemented for real (not proxied): `sessionTag` packing into a shared session, 5-REPL cap + spill, non-idempotent acquire, independent get/delete, slot reuse on release. REPL statements proxy to real Spark (BYO) | 🟢 Real (contract) / 🟠 exec |
| Environments, Spark Job Definitions | Item management only | 🟡 Emulated |

## Data Warehouse (`data-warehouse/`)

| Fabric feature | Emulator | Type |
|---|---|---|
| SQL-analytics-endpoint semantics over lakehouse Delta | DuckDB runs real SQL (aggregation / join / filter), e2e | 🟢 Real (engine in e2e) |
| Warehouse item management | Full | 🟢 Real |
| **T-SQL over TDS + Entra FedAuth** | Pure-Go TDS front (`internal/tds`) terminates the FedAuth handshake (real Entra token, `database.windows.net` audience) and relays to a **SQL Server** sidecar; unmodified `go-mssqldb`/`pyodbc` clients connect and run T-SQL. Verified against a real SQL Server | 🟢 Real (front) / 🟠 SQL Server sidecar |
| **Lakehouse SQL analytics endpoint — Delta → engine** | The emulator reads the lakehouse's `Tables/<t>` Delta in pure Go and reflects (CREATE+INSERT) it into the sidecar on connect, so `SELECT` hits real OneLake data (matches DuckDB), **read-only** (writes rejected). *Not PolyBase* — SQL Server reading Delta in place is a proven dead-end on the Linux container (`e2e/sql-endpoint-spike/`) | 🟢 Real (reflection) |
| **Warehouse — read-write T-SQL** | Client `CREATE`/`INSERT`/`SELECT` relay straight to the sidecar; the warehouse owns its data (no reflection) | 🟢 Real (relay) |
| Per-item isolation (each item = its own SQL Server database) | Lakehouse/Warehouse routed by type; per-item databases so they never collide | 🟢 Real |
| RBAC → SQL permissions | Workspace role enforced on connect: no role → rejected; Viewer → read-only; Contributor+ → read-write (warehouse) | 🟢 Real |
| `information_schema` / `sys.*` introspection | Relays natively — reflected/warehouse tables are real SQL Server tables | 🟢 Real (relay) |
| Per-column type fidelity (real SQL types over the wire) | Integer/float/bit columns carry their real TDS type (INTN/FLTN/BITN); other types fall back to NVARCHAR text | 🟢 Real (numeric/bool) |
| Connection by item *name* (vs GUID) | Workspace read from the server name (`<workspace>.datawarehouse.fabric.microsoft.com`), item resolved by display name; a GUID still resolves by id (back-compat). Verified with a real `go-mssqldb` client | 🟢 Real |

## Data Factory (`data-factory/`)

| Fabric feature | Emulator | Type |
|---|---|---|
| Data Pipeline control flow (If / ForEach / Until / Switch / Filter / Fail, expression language, `dependsOn`) | Pure-Go interpreter that really executes | 🟢 Real (orchestration) |
| Per-activity **policy — retry + timeout** | Applied to every activity type: `policy.retry` re-runs a failed activity (each retry re-runs it from scratch; only the final outcome is recorded, carrying `retryAttempt`); `policy.timeout` fails an attempt whose virtual duration exceeds the limit (a `Wait` past its timeout is deterministically testable). No real sleeping — a backoff is exercised in milliseconds on the controllable clock | 🟢 Real |
| **Invoke pipeline** (ExecutePipeline) | Resolves the referenced `DataPipeline` (GUID or name, optional other workspace) and runs it through a fresh interpreter — **real recursive interpretation**, one level deeper on the same engines. `waitOnCompletion` (default) gates the parent on the child's terminal status; parameters flow into the child; a cycle or excessive nesting fails loudly | 🟢 Real |
| Pipeline → notebook activity (TridentNotebook) | Resolves the notebook reference and creates a **real RunNotebook job instance** the pipeline gates on — the pipeline→jobs linkage is real; the notebook's **cells** execute only on the Spark sidecar (otherwise the job is clock-derived, like any RunNotebook job) | 🟢 Real chain / 🟠 exec |
| `queryactivityruns` detail | Full | 🟢 Real |
| Copy activity — **OneLake → OneLake** | Really moves the bytes through the storage layer: a file, or a directory subtree preserving structure; source/sink locations `{workspaceId?, itemId, path}` are expression-resolved (GUID or name); returns real `filesWritten` / `dataWritten`. External stores / format transformation are out of scope and **fail loudly** | 🟢 Real (in-family) / 🔴 external |
| Lookup activity — **OneLake CSV/JSON** | Reads **real rows** from a CSV or JSON file in OneLake (format from hint or extension); honors `firstRowOnly`; the result flows into `@activity(…).output` for downstream steps. Parquet + SQL-endpoint sources are a follow-up (Parquet needs a reader dep; SQL is the warehouse engine's job) | 🟢 Real (CSV/JSON) |
| GetMetadata activity — **OneLake path** | Stats a **real** OneLake path: `exists` / `itemType` / `size` / `lastModified` / `childItems`; a missing path honestly returns `exists:false` | 🟢 Real |
| Web / SQL-script / external-connector leaves | **Stubbed success** — reached in `dependsOn` order and inputs resolved, but nothing executes: a SQL engine can't embed under the pure-Go/no-CGO build, and Web calls to arbitrary URLs would break the offline/deterministic guarantee | 🟡 Emulated |
| **Dataflow Gen2** (Power Query M engine) | An in-pipeline Dataflow activity fails with an explicit "not implemented" | 🔴 Honest fail |
| Connectors / on-prem gateways | — | 🔴 Not implemented |

## CI/CD (`cicd/`)

| Fabric feature | Emulator | Type |
|---|---|---|
| Git integration (connect / status / commit / update / disconnect) | Full, real state | 🟢 Real |
| `fabric-cicd` tool publishing | The real client round-trips definitions (e2e) | 🟢 Real |
| Deployment pipelines | — | 🔴 Not implemented |

## Item types present, engine absent

| Fabric area | Emulator | Type |
|---|---|---|
| Real-Time Intelligence — Eventhouse / KQL DB / Eventstream (`real-time-intelligence/`) | Item management only; no KQL / streaming engine | 🟡 mgmt / 🔴 exec |
| Mirroring — Mirrored Database (`mirroring/`) | Item management only | 🟡 mgmt / 🔴 exec |
| Power BI — Semantic Models / Reports | Item management only; no modeling / rendering engine | 🟡 mgmt / 🔴 render |
| Data Science — ML models / experiments / MLflow (`data-science/`) | — | 🔴 Not implemented |
| Fabric SQL Database (`database/`), Graph (`graph/`), Real-Time Hub, Copilot / IQ (`iq/`), Embed, Workload Dev Kit | — | 🔴 Not implemented |

## Emulator-only (no Fabric equivalent — these exist for testing)

| Capability | Purpose |
|---|---|
| Controllable clock (`/_emulator/clock`) | Advance virtual time to drive LRO / job status transitions deterministically. |
| Fault injection (`/_emulator/faults`, `/_emulator/permissions`) | Force failures / throttling / RBAC denials to test client resilience. |
| Svelte management portal | Dashboard, workspaces, operations, clock, and fault controls. |

## Ecosystem conformance: real OSS/vendor clients as witnesses

Parity isn't claimed from our own tests alone — each 🟢 surface is pinned against
the **real, unmodified client** a Fabric user runs, executed against the emulator
in CI (`e2e/<client>/`). If Microsoft's own tool round-trips unchanged, the
contract holds better than any assertion we could write ourselves.

| Real client (pinned) | Surface exercised | Status |
|---|---|---|
| `fabric-cicd` (Microsoft) | Control plane / CI-CD publish | 🟢 `e2e/fabric-cicd` |
| `deltalake` (delta-rs) | OneLake Delta write/read | 🟢 `e2e/delta-rs` |
| `azure-storage-file-datalake` + Blob SDK | OneLake ADLS **Gen2 DFS** + Blob | 🟢 `e2e/adls-sdk` |
| DuckDB | Lakehouse SQL over Delta/Parquet | 🟢 `e2e/duckdb` |
| PySpark behind the **Livy API** | Spark sessions / statements | 🟢 `e2e/spark`, `e2e/livy`, `e2e/notebook-run` |
| `notebookutils` | Notebook utility shim | 🟢 `e2e/notebookutils` |
| `go-mssqldb` | Warehouse/Lakehouse **TDS + FedAuth** | 🟢 `internal/server`, `internal/tds` |
| **`dbt-fabricspark`** (Microsoft) | Fabric **Spark** via Livy sessions | ⏳ **Planned** — a 2nd real client over the Livy HC layer |
| **`dbt-fabric`** (Microsoft) | Warehouse **TDS via ODBC Driver 18** | ⏳ **Planned** — closes the TDS driver-diversity gap |

The TDS surface has exactly **one** driver witness today (`go-mssqldb`).
`dbt-fabric` matters because the Microsoft **ODBC Driver 18** is an *independent*
TDS implementation with its own FedAuth/prelogin handshake: passing it is a
stronger parity claim than any number of go-mssqldb tests. `dbt-fabricspark`
drives the just-built high-concurrency Livy layer over its real Livy-session
protocol (`method: livy`, service-principal auth via entra-emulator).

## Scope boundary: Fabric, not the predecessor Azure products

The emulator targets **Microsoft Fabric** — the *convergence/successor* product —
not the earlier Azure analytics services Fabric replaced. That boundary is why
some adjacent dbt adapters and Azure surfaces are intentionally **not** built:
they belong to predecessor (often retired) products, and their Fabric-native
successors are what we emulate instead.

| Adjacent product / client | Why out of scope | Fabric-era equivalent (in scope) |
|---|---|---|
| **Azure Synapse** dedicated SQL pool (`dbt-synapse`) | Different product: its own control plane (Synapse workspaces) **and** an MPP T-SQL dialect (`DISTRIBUTION = HASH`, clustered-columnstore / resource-class DDL) that our vanilla SQL Server sidecar rejects. `dbt-synapse` layers on `dbt-fabric`, so the *shared* SQL path is already covered by the `dbt-fabric` witness | **Fabric Warehouse** — 🟢 TDS relay |
| **Azure Data Lake Analytics** — U-SQL / SCOPE (`dbt-scope`) | Retired service (EOL **Feb 2024**), proprietary batch language, no Fabric embodiment. The only overlap (Delta on a lake) is Spark/OneLake, already witnessed | Fabric **Spark** — 🟠 Livy |
| **ADLS Gen1** | Retired (**Feb 2024**), superseded by Gen2 | — |
| **ADLS Gen2** (standalone storage account) | **Not missing — OneLake *is* the Gen2 endpoint**: hierarchical namespace, the `dfs` filesystem API, `onelake.dfs.fabric.microsoft.com`. Fabric has no separate storage account to emulate | **OneLake** — 🟢 `e2e/adls-sdk` |

Rule of thumb: if a capability exists only in a product Fabric replaced, it's out
of scope; its Fabric-native successor is what we build. "We already have the
TDS/SQL Server foundation" makes Synapse *cheaper*, not *done* — the remaining
delta is a whole MPP dialect plus a second control plane, for a superseded
target. So the two dbt adapters we build (`dbt-fabricspark`, `dbt-fabric`) are
exactly the two that hit live Fabric surfaces; the other two (`dbt-synapse`,
`dbt-scope`) target predecessor products outside the emulator's remit.

## Why the boundary sits where it does

Real Fabric's own Livy endpoint is *Microsoft's implementation of the Livy REST
contract* over their Spark platform — they honor the protocol, not the retired
Apache Livy server. And where Fabric adds its *own* layer on top of that
protocol — high-concurrency REPL packing, which a vanilla Livy server has no
concept of — the emulator implements that layer directly rather than proxying,
because there is nothing to proxy it to. That is the same stance throughout: the
**protocol and control plane are the durable, real things** (built, not mocked,
so real clients can't tell the difference), and the compute engine is attached
(Spark) or deferred when proprietary/heavyweight (Dataflow Gen2's M engine, KQL,
Power BI rendering, T-SQL/TDS). Every deferral fails loudly rather than
pretending to succeed. See [13-roadmap.md](13-roadmap.md) for the milestone
history and the deferred-with-cause rationale.
