# 14 — Real compute: PySpark, Delta, and the warehouse

**Status: largely shipped (R0–R5).** This document laid out the track that turns
fabric-emulator from a *contract emulator* into a *local Fabric runtime* — by
attaching **real engines**, never by faking results. Most of it now exists, each
milestone gated on a real client in CI; the honest exceptions still in flight
(a real Airflow sidecar, a handful of pipeline leaf activities like Web/Script)
are marked inline. Tense below is kept as originally written where it still
reads as intent; shipped items carry a ✅ and their `e2e/` witness.

## The principle (a refinement, not a reversal)

[03-architecture.md](03-architecture.md) declares "no compute engines" a
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
3. compose-level sidecar attachments (Spark, DuckDB, SQL Server).

This held even for the piece once expected to become a separate service: the
**TDS-FedAuth termination** (Track C) shipped **in this repo** as `internal/tds`
(a pure-Go TDS front that validates the login's Entra token and then byte-splices
the session to the SQL Server sidecar) — not a sibling repo after all.

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
  semantics only — needs Range + conditional writes, not rename. Closes the
  SDK half of the "azcopy / ADLS SDK e2e" roadmap item; the azcopy half is now
  also wired (`e2e/azcopy`).
- **A2 — real PySpark**: `pyspark` + `delta-spark`, ABFS driver
  (`abfss://{workspace}@onelake.dfs.fabric.microsoft.com/{item}/…`, the
  documented URI), OAuth client-credentials token provider pointed at
  entra-emulator. Write a Delta table from Spark; read it back with delta-rs
  (A1) — cross-engine interop on our storage.

  > **A2 is delivered inside Track B, not standalone (decided).** A1 (delta-rs)
  > and the ADLS-SDK e2e both redirect cleanly because their clients expose an
  > endpoint knob (`azure_endpoint`, `account_url`). The Hadoop **JVM ABFS
  > driver does not** — it derives its endpoint from the URI authority and
  > takes no host/port override, so pointing it at a localhost emulator has no
  > in-process ("ABFS-in-a-venv") answer. The structural fix is a **container
  > network** where `onelake.dfs.fabric.microsoft.com` resolves to the
  > emulator — which is exactly the Track B sidecar. So A2 rides on B's
  > containerized Spark rather than being fought first as a bare-venv
  > milestone. Bonus: the JVM lives in a Linux container, making this the
  > only cross-OS-honest path for Windows developers (thin client, no
  > `winutils`).

## Track B — Spark execution (jobs become real), with A2 riding on it

Fabric's documented Spark surface is the **Livy API**
(`data-engineering/get-started-api-livy.md`), and its endpoint lives on the
control-plane origin we already serve:

```
https://api.fabric.microsoft.com/v1/workspaces/{ws}/lakehouses/{lh}/livyapi/versions/2023-12-01/{sessions|batches}
```

- **B1 — Livy sessions on real Spark. ✅** Those routes are implemented
  (bearer-validated, RBAC-checked like everything else). Apache Livy itself is
  retired (Attic), so the shipped path is **native**: `--spark-agent-url` points
  the emulator at a Spark **statement-executor agent** (`e2e/livy/agent.py`) that
  runs each session/statement/batch on real Spark, with the session configured so
  `abfss://` resolves to our OneLake plane — Fabric-shaped URL outside, real Spark
  inside (`internal/api/livy_native.go`, `e2e/livy`). A `--spark-livy-url` mode
  still reverse-proxies an external Livy server if you have one. Fabric's
  **high-concurrency** (5-REPL) session packing is implemented for real on top
  (`internal/api/livy_hc.go`). Without either flag, statements 501 honestly.
- **B2 — jobs integration. ✅** `RunNotebook` job execution parses the notebook
  into cells and runs them on the Spark agent, finalising the job's terminal
  status and exit value from the *actual* run (`e2e/notebook-run`); without an
  agent, the deterministic clock behavior remains the default — CI stays fast.
- **Token passthrough**: the Livy docs define `Code.Access*` scopes for Spark
  code acquiring downstream tokens — including `Code.AccessAzureKeyvault.All`.
  Spark code calling `mssparkutils`-style credential helpers can be pointed
  at entra-emulator's MSI endpoint, and secrets resolve from
  **azure-keyvault-emulator** — the three-emulator chain, from inside a real
  Spark job.

## Track C — the warehouse (two fidelity targets, priced separately)

- **C1 — SQL analytics endpoint *semantics*. ✅** **DuckDB** with its delta
  extension queries the *same Delta files* Spark wrote in Track A — completely
  real SQL over completely real Delta, the actual lakehouse↔warehouse interop
  story, verified in `e2e/duckdb` (aggregation / join / filter over lakehouse
  Delta, agreeing with the TDS engine).
- **C2 — real T-SQL engine. ✅** Sidecar **SQL Server on Linux** (real TDS +
  T-SQL; Babelfish considered and rejected for fidelity — see
  [16-warehouse-tds.md](16-warehouse-tds.md)), each Warehouse/Lakehouse item
  routed to its **own SQL Server database**, with a Lakehouse's OneLake Delta
  reflected into the engine on connect (read-only) and a Warehouse relayed
  read-write. RBAC → SQL permissions, `information_schema` parity, native
  per-column type fidelity, connect-by-name.
- **C3 — Entra FedAuth over TDS. ✅ (in-repo, not a proxy repo).** The original
  "local engines only do SQL auth" compromise was dissolved: `internal/tds`
  terminates the FedAuth login itself (validates the login's Entra token against
  entra-emulator's JWKS, `database.windows.net` audience) and then **byte-splices**
  the client's post-login session straight to the SQL Server sidecar over a SQL
  login — so the engine emits every token natively (transactions, RPCs, prepared
  statements). This restores the production auth story for real clients:
  unmodified `go-mssqldb` **and** the Microsoft ODBC Driver 18 (pyodbc) connect,
  and Microsoft's real **dbt-fabric** adapter passes end-to-end (`e2e/dbt-fabric`).
- **C4 — Fabric SQL Database (OLTP + OneLake mirror). ✅** The `SQLDatabase` item
  reuses the read-write TDS path (its own SQL Server database) and adds
  **mirroring**, the reverse of the lakehouse reflection: `warehouse.Mirror`
  reads the item's SQL tables and writes each as a **Delta** table into OneLake
  `Tables/` (real Parquet + `_delta_log`), triggered by
  `POST …/sqlDatabases/{id}/refreshMirror`. So an operational database becomes
  queryable as Delta by Spark/DuckDB/delta-rs — the OLTP↔analytics story. Proven
  by a gated e2e (write over TDS → mirror → the Delta reads back). *Deferred:*
  continuous/CDC mirroring (the emulator snapshots on the refresh trigger).

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

**E1 — Apache Airflow: the fully real tier. 📐 planned (not yet shipped).**
Fabric's own code-first orchestrator is **genuine Apache Airflow**
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
emulation. And not merely "compatible": Fabric's hosted offering *is*
upstream Apache Airflow (the docs pin exact releases — currently Airflow
**2.10.5** on Python **3.12** — and point at airflow.apache.org's own guides
for custom operators/hooks/plugins). The sidecar pins the same documented
versions, so a DAG that runs locally runs identically hosted. For data
engineers who *can choose*, this is the recommended local pipeline path.

(To be precise about what Airflow is and isn't in Fabric: the default
no-code **Data pipeline** does *not* run on Airflow — that's the proprietary
ADF-lineage engine E2 addresses. Airflow is Fabric's separate code-first
orchestrator, offered side by side.)

**E2 — DataPipeline interpreter: our control flow, real work. ✅ (core).** The
no-code pipeline engine (ADF lineage) is proprietary — Polaris-class
unobtainable. For `DataPipeline` items we implement the **documented control-flow
semantics** ourselves — `dependsOn` conditions
(Succeeded/Failed/Completed/Skipped), ForEach, If / Until (+timeout), Switch,
Filter, Fail, the ADF expression language, and per-activity retry/timeout
policies — with leaf activities delegating to real work where an engine exists:

| Activity | Executes via | Status |
|---|---|---|
| Invoke pipeline | recursive interpretation of the referenced `DataPipeline` | ✅ real |
| Lookup | reads rows from a CSV/JSON/Parquet file, or a lakehouse Delta table, **directly in OneLake** (pure-Go, real bytes) | ✅ real |
| GetMetadata | stats a OneLake path (exists/size/children) | ✅ real |
| Copy | real byte movement, **OneLake→OneLake** (a file or a directory subtree) | ✅ real (in-family) |
| Notebook | Livy → the real Spark agent (Track B) | ✅ real (with `--spark-agent-url`) |
| Script / SqlServerStoredProcedure | real T-SQL against a Warehouse/SQLDatabase item's own SQL Server database (Track C's backend) — the emulator's own scoped `{workspaceId?, itemId}` target reference, not Fabric's linkedService wire shape | ✅ real (with `--warehouse-sql-url`) |
| Web / Webhook | real HTTP calls to arbitrary URLs would break the offline/deterministic guarantee | 📐 not yet wired |

Control-flow fidelity is deterministic on the controllable clock: **ForEach**
honors `isSequential` + `batchCount` and reports the right wall-clock (sequential
iterations add, a parallel batch costs its slowest); a per-activity **timeout**
fails an over-running attempt; and **retry with `retryIntervalInSeconds`** folds
the backoff into the activity's duration — all exercised in milliseconds, no real
sleep. Stated plainly: the orchestrator is *ours* — faithful to documented
control-flow semantics — but it is not Microsoft's engine. The *work* a pipeline
does is real: it exercises real Spark, real OneLake bytes, and real data movement.

**E3 — honestly unobtainable → 501.** Dataflow Gen2 (the Power Query M
compute is proprietary), self-hosted integration runtime / gateway scenarios,
and the long tail of cloud connectors outside the scoped set. Activities we
cannot execute for real fail with a clear 501 — a pipeline "succeeding"
without doing its work is exactly what this design forbids.

CI/CD for pipelines needs none of this and works **today**: `DataPipeline`
definitions round-trip through git and fabric-cicd like any other item.

## Engine weights — what actually runs, and when

The **binary itself** never gets heavier: engines attach via flags
(`--spark-agent-url`, `--warehouse-sql-url`, `--sql-tds-addr`), and with none of
them set it runs nothing but the Go process — clock-derived jobs, milliseconds,
no JVM. That's what CI's fast unit-test job runs, and it's what a bare
`go run`/`fabric-emulator` binary gives you.

The **bundled `docker compose up`** experience is different on purpose: since
[T5](16-warehouse-tds.md) and native Livy shipped, `docker-compose.override.yml`
(auto-loaded alongside `docker-compose.yml`, no flag needed) wires those same
flags to a Spark agent + SQL Server sidecar it also brings up — so the default
containerized experience has real engines attached, not just the contract. Opt
out to the lite pair with `docker compose -f docker-compose.yml up` (the
override is skipped when you name the base file explicitly). CI's heavy e2e jobs
(`livy-native`, `warehouse-tds`, the dbt suites, …) use their own per-suite
compose files under `e2e/`, independent of this one.

| Rung | What runs | Weight |
|---|---|---|
| Bare binary, no flags | the Go binary only; jobs are clock-derived | milliseconds |
| `docker compose -f docker-compose.yml up` (opt-out) | entra + fabric-emulator only, same as the bare binary | milliseconds |
| `docker compose up` (default) | + a Spark agent + a SQL Server sidecar | ~1.5 GB+, seconds to start |
| A1 delta-rs | Rust library in a Python wheel — no JVM | tens of MB |
| A2/B PySpark | **full JVM Spark** (local mode or container, via Livy) | GBs, seconds to start |
| C1 DuckDB | in-process library — no server, no JVM | a few MB |
| C2 SQL Server | **full SQL Server engine** in a container | ~1.5 GB, x86 (emulated on ARM) |

Honest asterisk on Track C: Spark's JVM *is* what Fabric runs — the weight
buys true fidelity. DuckDB/SQL Server are **real engines standing in for an
unobtainable one** (Fabric's warehouse is Microsoft's proprietary Polaris
engine): the Delta files they query are byte-identical to production, but
T-SQL dialect edges will differ. Same class of documented divergence as the
C2 SQL-auth compromise.

## Phasing

| Phase | Delivers | Status |
|---|---|---|
| **R0** | Track A storage completeness + A1 (delta-rs e2e) + the ADLS-SDK e2e (real `azure-storage-blob`) + azcopy; concurrent-commit race test | ✅ shipped |
| **R1+R2** (merged) | **containerized Spark** — A2 (real PySpark writes Delta via ABFS, cross-engine read with delta-rs) **and** B1+B2 (native Livy sessions/HC on a real Spark agent; real RunNotebook mode). JVM Spark image + Docker network so ABFS resolves to the emulator. | ✅ shipped (`e2e/spark`, `e2e/livy`, `e2e/notebook-run`) |
| **R3** | C1 (DuckDB SQL over the lakehouse) **and** C2/C3 (SQL Server sidecar + in-repo FedAuth-over-TDS splice; two driver witnesses) | ✅ shipped (`e2e/duckdb`, `e2e/dbt-fabric`, gated TDS tests) |
| **R4** | D1 (notebookutils shim) shipped (`e2e/notebookutils`); D2–D3 (default-lakehouse sessions; VS Code extension compatibility) partial/by-demand | ~ mostly |
| **R5** | E2 (DataPipeline interpreter, real-engine leaf activities) shipped; E1 (real Airflow sidecar behind ApacheAirflowJob items) **not yet built** | ~ E2 ✅ / E1 📐 |

## Correctness: how we prove it

Five oracle layers, cheapest first. The theme throughout: **we don't write
the oracles — we borrow them** from the clients and engines that production
Fabric already has to satisfy.

1. **Protocol conformance — unmodified real clients.** The established
   method (fabric-cicd found four real bugs; azsecrets/azkeys proved the
   vault): if delta-rs, the ADLS SDK, the ABFS driver, and azcopy succeed
   *unmodified*, the wire is right. Every A/B/C milestone is gated on a real
   client, never on our own HTTP calls agreeing with ourselves.
2. **Cross-engine round-trips — the data oracle.** Write with engine A, read
   with engine B: Spark writes Delta → delta-rs reads it → row counts and
   content checksums match → DuckDB queries the same table → same results.
   Three independent implementations agreeing on bytes they didn't write is
   the strongest correctness signal available without a formal spec.
3. **Concurrency and atomicity — the Delta commit protocol.** Two writers
   racing to commit `_delta_log/N.json`: exactly one must win the
   put-if-absent (`If-None-Match: *`), the loser must observe the conflict
   and retry as version N+1, and no committed row may vanish. This is the
   corruption class the R0 ETag work exists to prevent, and it gets explicit
   adversarial tests (concurrent writers + fault injection), not just happy
   paths.
4. **Borrowed reference suites.** The ecosystems ship their own conformance
   tests, written by the people who defined the semantics: Hadoop's **ABFS
   filesystem contract tests** (hadoop-azure), delta-rs's object-store
   integration suite, and dbt's **dbt-tests-adapter** acceptance suite.
   Pointing them at the emulator turns thousands of third-party assertions
   into our regression net. These run as opt-in CI jobs (they're heavy),
   like the fabric-cicd job.
5. **Version pinning as a correctness contract.** Engines and libraries pin
   to the documented Fabric Runtime (Spark 3.5.x / Delta 3.x for Runtime
   1.3; Airflow 2.10.5 / Python 3.12) — so "works on the emulator" and
   "works hosted" refer to the same binaries, and drift is a deliberate
   bump, not an accident.

Where a layer can't apply, the surface 501s rather than passing vacuously —
an untestable claim is treated as a missing feature, not a passing test.

## The compatibility matrix: what must run

**Python on PySpark — Tier 1 (touches our surfaces; each gets an e2e):**

| Library | Why it must work | Surface it exercises |
|---|---|---|
| `pyspark` 3.5.x + `delta-spark` 3.x | the engine itself (Runtime 1.3 pin) | ABFS → OneLake DFS (Track A) |
| `notebookutils` (the D1 shim) | every Fabric notebook imports it | fs → DFS; credentials → entra + vault; lakehouse/jobs → `/v1` |
| `deltalake` (delta-rs) | Spark-free Delta access; the A1 milestone | object-store semantics on DFS |
| `pandas` + `pyarrow` | `toPandas()`/`createDataFrame`, `pyspark.pandas`, parquet bridging | in-engine, plus storage when reading `abfss://` |
| `fsspec` + `adlfs` + `azure-storage-file-datalake` + `azure-identity` | what `pandas.read_parquet("abfss://…")` actually uses | the Python storage path incl. the credential chain |
| `mlflow` | Fabric experiments/models *are* MLflow | later: attach a **real** mlflow tracking sidecar (same never-fake pattern) or 501 |
| `semantic-link` (`sempy`) | Fabric-native analytics | partial: list/REST paths work; semantic-model *query* needs the unobtainable engine → 501 |

**Tier 2 (compute-local, storage-agnostic — work automatically once Spark
runs; verified by one smoke notebook, not per-library e2e):** numpy,
scikit-learn, matplotlib/seaborn, and the rest of the Runtime's preinstalled
scientific stack.

**dbt — three adapters, three engine targets:**

| Adapter | Speaks | Works against | Status |
|---|---|---|---|
| **`dbt-fabricspark`** (Microsoft) | the **Fabric Livy API** | our native Livy → real Spark agent; models materialize as Delta in OneLake | ✅ shipped (`e2e/dbt-fabricspark`) |
| **`dbt-fabric`** (Microsoft; documented in `tutorial-setup-dbt.md`) | TDS/pyodbc + Microsoft ODBC Driver 18 to the warehouse SQL endpoint | the C2 SQL Server sidecar, FedAuth terminated in-repo and byte-spliced (T-SQL dialect edges per the Polaris asterisk) | ✅ shipped (`e2e/dbt-fabric`, debug→seed→run→test) |
| **`dbt-duckdb`** (+ delta plugin) | DuckDB in-process | the C1 engine over the same OneLake Delta files | 📐 not yet wired |

Acceptance for each: the official **dbt-tests-adapter** suite plus a
jaffle-shop-style project building end-to-end. The documented
Airflow-orchestrated dbt pattern (`apache-airflow-jobs-dbt-fabric.md`)
composes with E1: a real Airflow DAG running real dbt against the emulator.

## Non-goals

Performance parity, autoscaling/capacity behavior, Spark version matrixing, and
emulating engine *internals* in any form. (High-concurrency Livy session packing,
once listed here as out of scope, shipped — `internal/api/livy_hc.go`.) Where a
real engine can't be attached, the surface returns 501 — it never pretends.
