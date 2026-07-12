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

## Track D — the notebook developer loop

A Fabric "Notebook" is four layers; three already have designs. This track
adds the fourth and composes them into a complete local environment for a
Fabric data engineer:

| Layer | Status |
|---|---|
| **The item + source format** (`.platform` + `notebook-content.py`, definitions, git/fabric-cicd round-trip) | ✅ shipped (P1) |
| **Execution** (Livy on the documented endpoint → real Spark sidecar) | 📐 Track B |
| **Storage** (Delta in OneLake via real engines) | 📐 Track A |
| **The runtime library — NotebookUtils** | ⬅ this track |

**D1 — a functional `notebookutils` shim** (pip-installable Python package,
`python/notebookutils/` in this repo). `notebook-utilities.md` documents ten
modules; every one maps onto surfaces the family already serves:

| Module | Backed by |
|---|---|
| `notebookutils.fs` (+ mount) | the OneLake DFS plane |
| `notebookutils.credentials.getToken` | entra-emulator (MSI endpoint / workspace identity) |
| `notebookutils.credentials.getSecret` | **azure-keyvault-emulator** — the `Code.AccessAzureKeyvault.All` path, for real |
| `notebookutils.notebook` (run/runMultiple/exit + management) | jobs + Livy (B2) and `/v1` item CRUD |
| `notebookutils.lakehouse` | `/v1/workspaces/{id}/lakehouses` |
| `notebookutils.runtime` (context) | session/workspace context from the emulator |
| `notebookutils.session`, `udf`, `variableLibrary` | session mgmt; 501-honest until their items exist |

Naming note: Microsoft publishes a non-functional `notebookutils` typing stub
on PyPI for IDE support; ours is a *functional* implementation of the same
documented API — distribution name TBD to avoid collision, module name
compatible so notebook code runs unchanged.

**D2 — default-lakehouse session semantics**: the Spark sidecar's sessions
are provisioned with the notebook's attached lakehouse mounted — relative
`Files/`/`Tables/` paths and `spark.sql` over lakehouse tables resolve to our
OneLake plane, matching the attached-lakehouse experience. Spark/Delta
versions pinned to the documented Fabric Runtime (1.3 = Spark 3.5/Delta 3.x).

**D3 — authoring surfaces**, in order of fidelity:

1. **VS Code + the Microsoft Fabric extension** — the real local authoring
   tool. For Runtime 1.3+ the docs say there is *no local conda env*: the
   extension runs notebooks on **remote Spark compute** via a Jupyter kernel
   — which is precisely the shape of our Livy+Spark sidecar. Investigation
   item: whether the extension's service endpoints can be redirected
   (fabric-cicd could via env; the DNS-pin + cert trick is the fallback).
   If yes, a data engineer authors in VS Code against the emulator
   end-to-end.
2. **Plain Jupyter/VS Code `.ipynb`** + git / fabric-cicd sync — works
   **today**: author anywhere, `fabric-cicd` publishes into the emulator,
   `updateFromGit` pulls edits back.
3. The Svelte portal gets a read-only notebook view, not an editor —
   authoring belongs to real tools.

The complete loop this yields: *author in VS Code → sync via git/fabric-cicd
→ run interactively on the Spark sidecar with the default lakehouse mounted →
`notebookutils` resolves files, tokens, and Key Vault secrets through the
emulator family → Delta lands in OneLake → query it via the DuckDB SQL
endpoint → schedule via the jobs API → CI drives the identical REST surfaces.*

## Track E — pipelines: real orchestration where it exists, real work everywhere

Pipelines are the sharpest test of the never-fake principle, because the
engines split three ways:

**E1 — Apache Airflow: the fully real tier.** Fabric's own code-first
orchestrator is **genuine Apache Airflow**
(`apache-airflow-jobs-concepts.md`: "Python-based DAGs", the next generation
of ADF's Workflow Orchestration Manager) — and Airflow is OSS. So the
highest-fidelity local pipeline story attaches a **real Airflow sidecar** as
the runtime behind `ApacheAirflowJob` items:

- DAG files stored as item definitions (the P1 machinery), synced into the
  sidecar's DAG folder;
- operators drive our control plane over REST — trigger notebook jobs (real
  Spark via Track B), poll LROs, move OneLake data;
- Airflow's **Azure Key Vault secrets backend** (documented:
  `apache-airflow-jobs-enable-azure-key-vault.md`) pointed at
  azure-keyvault-emulator — connections/variables resolve from the family's
  vault.

Real scheduler, real executor, real DAG semantics — zero orchestration
emulation. For data engineers who *can choose*, this is the recommended
local pipeline path.

**E2 — DataPipeline interpreter: our control flow, real work.** The no-code
pipeline engine (ADF lineage) is proprietary — Polaris-class unobtainable.
For `DataPipeline` items we therefore implement the **documented control-flow
semantics** ourselves — `dependsOn` conditions
(Succeeded/Failed/Completed/Skipped), ForEach (sequential/parallel), If /
Until (+timeout), Invoke Pipeline, and the retry policies
`activity-overview.md` specifies — with every **leaf activity delegating to a
real engine**:

| Activity | Executes via |
|---|---|
| Notebook | Livy → the real Spark sidecar (Track B) |
| Script / Stored procedure / Lookup | the warehouse engine (Track C) |
| Web / Webhook | real HTTP calls |
| Copy | real byte movement over a **scoped connector set** (OneLake↔OneLake, ADLS-shaped, HTTP source, local SQL) |
| Invoke pipeline | recursive interpretation |

Stated plainly: the orchestrator here is *ours* — faithful to documented
semantics, deterministic on the controllable clock (a Until-timeout or retry
backoff is testable in milliseconds) — but it is not Microsoft's engine. The
*work* the pipeline does is real, and a pipeline that runs here exercises
real Spark, real SQL, and real data movement.

**E3 — honestly unobtainable → 501.** Dataflow Gen2 (the Power Query M
compute is proprietary), self-hosted integration runtime / gateway scenarios,
and the long tail of cloud connectors outside the scoped set. Activities we
cannot execute for real fail with a clear 501 — a pipeline "succeeding"
without doing its work is exactly what this design forbids.

CI/CD for pipelines needs none of this and works **today**: `DataPipeline`
definitions round-trip through git and fabric-cicd like any other item.

## Engine weights — what actually runs, and when

The emulator core never gets heavier; engines are opt-in sidecars behind
compose profiles and flags. The default (no flags) runs nothing but the Go
binary, and the clock-derived job behavior remains — CI's unit suite never
waits on a JVM.

| Rung | What runs | Weight |
|---|---|---|
| Default | the Go binary only; jobs are clock-derived | milliseconds |
| A1 delta-rs | Rust library in a Python wheel — no JVM | tens of MB |
| A2/B PySpark | **full JVM Spark** (local mode or container, via Livy) | GBs, seconds to start |
| C1 DuckDB | in-process library — no server, no JVM | a few MB |
| C2 Babelfish | **full PostgreSQL fork** in a container | hundreds of MB |

Honest asterisk on Track C: Spark's JVM *is* what Fabric runs — the weight
buys true fidelity. DuckDB/Babelfish are **real engines standing in for an
unobtainable one** (Fabric's warehouse is Microsoft's proprietary Polaris
engine): the Delta files they query are byte-identical to production, but
T-SQL dialect edges will differ. Same class of documented divergence as the
C2 SQL-auth compromise.

## Phasing

| Phase | Delivers |
|---|---|
| **R0** | Track A storage completeness + A1 (delta-rs e2e) |
| **R1** | A2 (real PySpark writes Delta via ABFS; cross-engine read with delta-rs) |
| **R2** | B1+B2 (Livy passthrough; real RunNotebook mode) |
| **R3** | C1 (DuckDB SQL over the lakehouse); C2/C3 by demand |
| **R4** | D1–D3 (notebookutils shim; default-lakehouse sessions; VS Code extension compatibility) |
| **R5** | E1 (real Airflow sidecar behind ApacheAirflowJob items); E2 (DataPipeline interpreter, real-engine leaf activities) |

## Non-goals

Performance parity, autoscaling/capacity behavior, high-concurrency Livy
sessions (initially), Spark version matrixing, and emulating engine
*internals* in any form. Where a real engine can't be attached, the surface
returns 501 — it never pretends.
