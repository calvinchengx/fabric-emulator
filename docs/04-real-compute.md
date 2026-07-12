# 04 — Real compute: PySpark, Delta, and the warehouse

**Status: design.** This document plans the track that turns fabric-emulator
from a *contract emulator* into a *local Fabric runtime* — by attaching
**real engines**, never by faking results.

## The principle (a refinement, not a reversal)

[01-architecture.md](01-architecture.md) declares "no compute engines" a
non-goal. What that non-goal was always protecting against is *pretend*
compute — a notebook run that "succeeds" without running, a query endpoint
returning canned rows. The refinement, proven by the fabric-cicd and
azure-keyvault-emulator work:

> **Never fake results. Either do it for real (attached real engines) or
> fail honestly (501).**

Real PySpark executing real Delta commits against our OneLake plane is
consistent with that. Emulated Spark would not be.

## Where this lives

**In this repository.** The family's repo boundaries follow service/trust
surfaces (STS / Fabric planes / vault), and real compute is not a new
service — it is:

1. this repo's OneLake plane becoming complete enough for real engines,
2. e2e harnesses in `e2e/` (like `e2e/fabric-cicd/`),
3. compose-level sidecar attachments (Spark, DuckDB, Babelfish).

One future exception: a **TDS-FedAuth proxy** (Track C) would be a standalone
service with its own release cycle — a separate sibling repo *if and when*
it's built.

## The junction: OneLake is where engines meet

In real Fabric, Spark and the Warehouse are engines rendezvousing on **Delta
files in OneLake**. Our P3 data plane is therefore the single point of
leverage: make it complete enough for real storage clients, and every real
engine above it inherits the emulator — including its auth (Storage-audience
tokens from entra-emulator, workspace RBAC).

## Track A — storage completeness (the enabler)

What the P3 wire subset lacks for real engines, in dependency order:

| Gap | Needed by | Notes |
|---|---|---|
| **Range reads** (`Range: bytes=a-b`) | every Parquet reader | Parquet is seek-heavy; 206 Partial Content |
| **ETags + conditional requests** (`If-Match`/`If-None-Match`) | **Delta commit atomicity** | Delta's ADLS log store commits `_delta_log/N.json` with put-if-absent; without this, concurrent commits silently corrupt — the one gap that affects *correctness*, not just compatibility |
| **Rename** (`PUT dst` + `x-ms-rename-source`) | Hadoop committers | staged-file rename; delta-rs does not need it, ABFS does |
| List-paging fidelity (continuation, `etag`/`lastModified` per entry) | ABFS `listStatus` | |
| Blob-endpoint alias (`onelake.blob.…`) | some clients | parity doc: both endpoints exist |

**Milestones** (each an e2e harness in `e2e/`):

- **A1 — delta-rs**: the Python `deltalake` package (explicitly listed as
  OneLake-compatible in `onelake-api-parity.md`) writes and reads a Delta
  table through our DFS surface with an entra Storage token. Object-store
  semantics only — needs Range + conditional writes, not rename. Also closes
  the "azcopy / ADLS SDK e2e" roadmap item's SDK half.
- **A2 — real PySpark**: local `pyspark` + `delta-spark`, ABFS driver
  (`abfss://{workspace}@onelake.dfs.fabric.microsoft.com/{item}/…`, the
  documented URI), OAuth client-credentials token provider pointed at
  entra-emulator. Write a Delta table from Spark; read it back with delta-rs
  (A1) — cross-engine interop on our storage.

## Track B — Spark execution (jobs become real)

Fabric's documented Spark surface is the **Livy API**
(`data-engineering/get-started-api-livy.md`), and its endpoint lives on the
control-plane origin we already serve:

```
https://api.fabric.microsoft.com/v1/workspaces/{ws}/lakehouses/{lh}/livyapi/versions/2023-12-01/{sessions|batches}
```

- **B1 — Livy passthrough**: implement those routes (bearer-validated,
  RBAC-checked like everything else) delegating to a **real Apache
  Livy + Spark sidecar** in docker-compose, with the session pre-configured
  so `abfss://` resolves to our OneLake plane. Fabric-shaped URL outside,
  real Spark inside.
- **B2 — jobs integration**: `POST …/jobs/instances?jobType=RunNotebook`
  gains an opt-in real mode (`--spark-livy-url`): the job submits the
  notebook's definition as a Livy batch, and job status reflects the *actual*
  batch state instead of the clock-derived state machine. Without the flag,
  today's deterministic clock behavior remains the default — CI stays fast.
- **Token passthrough**: the Livy docs define `Code.Access*` scopes for Spark
  code acquiring downstream tokens — including `Code.AccessAzureKeyvault.All`.
  Spark code calling `mssparkutils`-style credential helpers can be pointed
  at entra-emulator's MSI endpoint, and secrets resolve from
  **azure-keyvault-emulator** — the three-emulator chain, from inside a real
  Spark job.

## Track C — the warehouse (two fidelity targets, priced separately)

- **C1 — SQL analytics endpoint *semantics*** (cheap, high value): **DuckDB**
  with its delta extension querying the *same Delta files* Spark wrote in
  Track A — completely real SQL over completely real Delta, the actual
  lakehouse↔warehouse interop story. Exposed initially through the item's
  REST query surface, not TDS.
- **C2 — real T-SQL engine**: sidecar **Babelfish for PostgreSQL** (a real
  TDS + T-SQL implementation) or a SQL Server container, provisioned per
  Warehouse item, with connection info surfaced on the item like real
  Fabric's `sqlEndpoint` properties. Compromise, stated plainly: local
  engines only do SQL auth — Entra FedAuth over TDS is not available in any
  stock engine.
- **C3 — TDS-FedAuth proxy** (future, separate repo): a Go proxy that parses
  the TDS login's FedAuth token, validates it against entra-emulator's JWKS,
  and forwards upstream with SQL auth — restoring the production auth story
  for `pyodbc`/SSMS. Genuinely novel, genuinely hard; only worth building if
  C2 sees real use.

## Phasing

| Phase | Delivers |
|---|---|
| **R0** | Track A storage completeness + A1 (delta-rs e2e) |
| **R1** | A2 (real PySpark writes Delta via ABFS; cross-engine read with delta-rs) |
| **R2** | B1+B2 (Livy passthrough; real RunNotebook mode) |
| **R3** | C1 (DuckDB SQL over the lakehouse); C2/C3 by demand |

## Non-goals

Performance parity, autoscaling/capacity behavior, high-concurrency Livy
sessions (initially), Spark version matrixing, and emulating engine
*internals* in any form. Where a real engine can't be attached, the surface
returns 501 — it never pretends.
