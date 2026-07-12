# Feature parity: fabric-emulator vs. real Microsoft Fabric

How the emulator's surface maps to real Fabric (as documented at
[learn.microsoft.com/fabric](https://learn.microsoft.com/en-us/fabric/) /
[MicrosoftDocs/fabric-docs](https://github.com/MicrosoftDocs/fabric-docs)), and
— the point of this table — **whether real work happens or just the API shape**.

The emulator's design bet is that the durable, testable surface is
*contracts + storage + identity + orchestration*, and those are done for real
(real signed JWTs, real Delta bytes on disk, real RBAC, a real pipeline
interpreter, real cross-engine SQL). The heavyweight or proprietary **compute
engines** are either bring-your-own (Spark behind the Livy proxy — which is how
Fabric itself layers a Livy endpoint over Spark) or honestly stubbed.

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
| Item **job execution** (`jobs/instances`) | Status derived from the controllable clock | 🟡 Emulated |

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
| Notebook **cell execution** | On the Spark sidecar (e2e); a scheduled RunNotebook job is clock-derived | 🟠 / 🟡 |
| Livy **High-Concurrency** (5-REPL) sessions | In progress | 🟡 Partial |
| Environments, Spark Job Definitions | Item management only | 🟡 Emulated |

## Data Warehouse (`data-warehouse/`)

| Fabric feature | Emulator | Type |
|---|---|---|
| SQL-analytics-endpoint semantics over lakehouse Delta | DuckDB runs real SQL (aggregation / join / filter), e2e | 🟢 Real (engine in e2e) |
| Warehouse item management | Full | 🟢 Real |
| **T-SQL over TDS + Entra FedAuth** | — | 🔴 Not implemented (deferred, with cause) |

## Data Factory (`data-factory/`)

| Fabric feature | Emulator | Type |
|---|---|---|
| Data Pipeline control flow (If / ForEach / Until / Switch / Filter / Fail, expression language, `dependsOn`) | Pure-Go interpreter that really executes | 🟢 Real (orchestration) |
| Pipeline → notebook activity (TridentNotebook) | Chains a real RunNotebook job | 🟢 Real chain |
| `queryactivityruns` detail | Full | 🟢 Real |
| Copy / Lookup / Web leaf activities | Orchestration recorded; storage effects proven by the OneLake e2es | 🟡 Emulated |
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

## Why the boundary sits where it does

Real Fabric's own Livy endpoint is *Microsoft's implementation of the Livy REST
contract* over their Spark platform — they honor the protocol, not the retired
Apache Livy server. The emulator takes the same stance: the **protocol and
control plane are the durable, real things**, and the compute engine is
attached (Spark) or deferred when proprietary/heavyweight (Dataflow Gen2's M
engine, KQL, Power BI rendering, T-SQL/TDS). Every deferral fails loudly rather
than pretending to succeed. See [13-roadmap.md](13-roadmap.md) for the
milestone history and the deferred-with-cause rationale.
