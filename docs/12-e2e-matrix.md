# 12 — E2E matrix

What CI proves on every push, and what's queued. The bar, inherited from
entra-emulator's SDK matrix: **real clients, unmodified**, against the
emulator — because driving Microsoft's actual tools catches fidelity gaps
spec-reading cannot (see [what fabric-cicd caught](11-testing-with-fabric-cicd.md#what-driving-the-real-tool-caught)).

## Verified on every push

Go integration tests start a real entra-emulator in-process and drive the
full HTTP surface; the remaining suites drive real third-party clients and
engines against the running emulator.

> **Default compute tier:** every Spark row below (`spark-a2`, `livy-native`,
> `dbt-fabricspark`, `notebook-run`) runs on **LakeSail's Sail** (Rust Spark
> Connect, no JVM). This proves the Spark Connect/DataFrame subset, not full
> Microsoft Fabric Runtime parity — see
> [20-lakesail-engine.md](20-lakesail-engine.md).

| Suite | Client | Proves | Where |
|---|---|---|---|
| Token handshake | in-process **entra-emulator** | real client-credentials token (Fabric aud) → JWKS validation → full workspace/RBAC/item/LRO flow over HTTP | Go integration tests (CI `test`, Linux + macOS + Windows) |
| Git round-trip | Go HTTP | two-workspace commit→update, definitions intact, logical ids preserved | Go integration tests |
| Identity handshake | in-process entra-emulator | provision → entra mints for the identity → token passes fabric RBAC → deprovision revokes → delete cascades | Go integration tests |
| OneLake | Go HTTP + real entra Storage tokens | create/append/flush/read via GUID + name addressing, listings, RBAC walls, managed-folder rejections | Go integration tests |
| **fabric-cicd** | Microsoft's real Python tool (v1.2.x) | `publish_all_items` publishes a notebook; parts round-trip byte-for-byte | `e2e/fabric-cicd/run.py` (CI `fabric-cicd`, 3-OS) |
| **Delta write/read (A1)** | real `deltalake` (delta-rs) | a real engine writes/reads a Delta table through the OneLake Blob surface with an entra Storage token — Range reads + the `_delta_log` put-if-absent commit primitive | `e2e/delta-rs/run.py` (CI `delta-rs`, 3-OS) |
| **Sail / Spark Connect (S0)** | LakeSail's real `sail` server + `pyspark-client` | a Rust Spark-Connect engine (no JVM) writes/reads Delta and runs SQL through the OneLake Blob surface — same object_store contract as delta-rs, entra-authenticated via the launcher mint | `e2e/sail/run.py` (CI `sail`, Linux) |
| **fabric_target toggle (T0)** | the `fabric-target` package | one `FABRIC_TARGET` switch resolves endpoints + credentials: seeded TokenCredential mints per-scope, session drives workspace-by-name → item → LRO, real-mode guards (workspace scope, az-login-or-SP, destructive gate) enforce; plus the T1 conformance suite (same 7 tests run against real Fabric via the secret-gated `real-fabric` workflow) | `e2e/fabric-target/run.py` (CI `fabric-target`, 3-OS) |
| **Real-Time Intelligence (KQL)** | Microsoft's own `kustainer` KQL engine + Microsoft's `azure-kusto-data` SDK + raw Kusto REST | the eventhouse's published `queryServiceUri` really executes KQL: create a table, ingest rows two documented ways, query the values back (`summarize`, datetime filter), and prove per-KQL-database isolation; Kusto-audience bearer + RBAC enforced, engine database naming never leaks | `e2e/rti/run.py` (CI `rti`, Linux **amd64 only** — the engine needs AVX2 and Microsoft documents ARM as unsupported) |
| **Governance (OpenMetadata)** | real OpenMetadata 1.13.2 (Postgres) + delta-rs | catalogs Delta schemas, shortcut lineage, and an executed pipeline Copy edge (`lake.orders → curated.orders_copy`) returned by OM's graph API; idempotent on re-ingest | `e2e/governance/run.py` (CI `governance`, Linux) |
| **Governance SSO** | real OpenMetadata + entra-emulator | OM's authenticator is entra: a forged **user** token is accepted by OM's API and a broken-signature token is refused — the catalog inside the family trust chain | `e2e/governance/sso.py` (CI `governance-sso`, Linux) |
| **ADLS SDK** | Microsoft's real `azure-storage-blob` | Parquet upload → byte-identical download (exercising `x-ms-range`), `list_blobs`, and the DFS surface sees the same file | `e2e/adls-sdk/run.py` (CI `adls-sdk`, 3-OS) |
| **azcopy** | Microsoft's real `azcopy` binary | multi-block upload (Put Block + Put Block List) → byte-identical download, and the DFS surface sees the same object | `e2e/azcopy/run.py` (CI `azcopy`, Linux) |
| **Spark API on Sail (A2)** | real PySpark (Spark Connect client) + LakeSail's `sail` | writes/reads Delta over production-shaped `abfs://…@onelake.dfs…` URLs — engine is Rust, no JVM | `e2e/spark/run.py` (CI `spark-a2`, Linux, containerized) |
| **Native Livy** | real Livy REST client + Sail | emulator terminates the Livy protocol itself and drives a statement agent — session + PySpark statements computed by a real engine (Sail, no JVM), no Apache Livy server. Also the **Delta maintenance witness**: `OPTIMIZE`/`VACUUM`/Change Data Feed run through delta-rs against a real `abfss://…onelake…` table, with a negative control (an invalid Storage bearer must be refused, so a pass cannot mean OneLake skipped the check) | `e2e/livy/run.py` (CI `livy-native`, Linux) |
| **Fabric VS Code extension contract** | Microsoft Fabric Data Engineering extension 1.18.1 route fixture | Power BI discovery/auth, workspace/artifact authoring, notebook content/resources with ETags, and host redirection through `api.powerbi.com` | `e2e/vscode-extension/run.py` (CI `vscode-extension`, Linux) |
| **Apache Airflow Job** | real Apache Airflow 2.10.5/Python 3.12 | uploaded DAG discovery, scheduler/executor task run, REST polling, and the resulting Fabric job terminal state | `e2e/airflow/run.py` (CI `airflow`, Linux) |
| **Data science loop** | PySpark/Sail + Direct Lake DAX + MLflow 3 + dbt-duckdb Delta plugin | one Spark-written OneLake Delta table is queried by DAX, tracked as typed MLflow experiment/model items with mirrored artifacts, then built and tested by dbt | `e2e/data-science-loop/run.py` (CI `data-science-loop`, Linux) |
| **dbt (fabric-spark)** | Microsoft's real `dbt-fabricspark` adapter | a dbt project (debug → seed → run → test) over the Fabric REST + Livy HC surface, models computed by Sail (no JVM) | `e2e/dbt-fabricspark/run.py` (CI `dbt-fabricspark`, Linux) |
| **dbt (fabric) via ODBC** | Microsoft's real `dbt-fabric` adapter + Microsoft ODBC Driver 18 | a dbt project (debug → seed → run → test) over the TDS warehouse surface through pyodbc + FedAuth (byte-spliced to a real SQL Server) — the **second** independent TDS driver family | `e2e/dbt-fabric/run.py` (CI `dbt-fabric`, Linux) |
| **Medallion tutorial, end to end** | delta-rs + Microsoft ODBC Driver 18 + real `dbt-fabric` | the whole analytics loop in one run: a Key Vault secret resolved through an `AzureKeyVaultReference` connection (and never returned on read), an extract landed in OneLake, bronze → silver in Delta, silver → gold with dbt over TDS, and a DAX query answered through `executeQueries` — plus the inverse assertion that the gold DQ tests **fail** on poisoned silver. Runs [`examples/medallion`](https://github.com/calvinchengx/fabric-emulator/tree/main/examples/medallion) unmodified — the witness for [28-tutorial-end-to-end.md](28-tutorial-end-to-end.md) | `e2e/medallion/run.py` (CI `medallion`, Linux) |
| **DuckDB SQL** | real DuckDB | SQL (aggregation, join, filter) over Delta tables in the OneLake plane — the lakehouse SQL-analytics-endpoint semantics | `e2e/duckdb/run.py` (CI `duckdb`, 3-OS) |
| **notebookutils** (+ T2 target unit tests) | real Fabric notebook | the functional `notebookutils` shim: fs over OneLake, credential tokens, Key Vault secret brokering, lakehouse control plane, `notebook.run`; plus `python/tests` asserting the shim's emulator-vs-real resolution (endpoints, TLS, DefaultAzureCredential, no seed leakage) | `e2e/notebookutils/run.py` (CI `notebookutils`, 3-OS) |
| **Notebook + SJD + Environment** | Sail and Spark 3.5/Delta 3.2 JVM | attached lakehouse metadata binds unqualified table APIs to OneLake; Environment requirements/config apply; SJD source+args execute and report; Sail rejects JAR requirements while JVM exposes its dependency surface | `e2e/notebook-run/{run.py,run-jvm.py}` (CI `notebook-run`, scheduled JVM oracle) |
| **External shortcuts** | containerized HTTP object stores | ADLS Gen2 and Amazon S3 shortcut definitions read real remote bytes through authenticated OneLake requests | `e2e/external-shortcuts/run.py` |
| **Warehouse TDS** | real `go-mssqldb` + real SQL Server 2022 | entra-token connect, then DDL + DML + a GROUP BY relayed through the TDS endpoint — **one of two** independent TDS driver witnesses (the other: Microsoft ODBC Driver 18 via `dbt-fabric` above); plus the SQL Database → OneLake Delta mirror, the pipeline Script/SqlServerStoredProcedure activities over real HTTP + jobs, and an external-source MirroredDatabase mirror (seeded on a database reached independently of the emulator's own per-item routing) | CI `warehouse-tds` (Linux) |

Plus: coverage floor 90% (cross-package; currently ~90%), `go vet`, a
distroless container smoke (`docker-smoke`), the portal build + headless
render (`portal`), and the
[docs site](https://calvinchengx.github.io/fabric-emulator/) build on every
docs push.

## Slower compatibility oracles

| Cadence | Suite | Proves |
|---|---|---|
| Weekly/manual | `e2e/spark-jvm` | Apache Spark 3.5.3 + Delta 3.2 batch Delta, Hadoop ABFS, RDD/SparkContext, JVM/JAR bridge, Structured Streaming, VACUUM and CDF |
| Weekly/manual | `e2e/notebook-run/run-jvm.py` | the same representative notebook used by Sail runs on the Fabric Runtime 1.3-aligned JVM baseline |
| Weekly + release | `e2e/notebook-run/real_fabric.py` | the representative DataFrame/SQL notebook publishes and completes in real Microsoft Fabric; secret-gated |

The Sail suite also asserts the negative boundary: RDD/SparkContext, Py4J,
`spark.jars`, streaming, `OPTIMIZE`, `VACUUM`, and ignored CDF options; it also
asserts one-winner/one-rejected concurrent overwrite behavior. A capability
change fails CI until this matrix is deliberately reclassified.

## Queued (designed, not yet wired)

Nothing queued — every designed real-client suite is wired above. New
milestones (e.g. non-OneLake external storage for shortcuts) will land here as
they're scoped.

## Running locally

```bash
go test ./...  # everything in-process, no network
uv run --frozen --group fabric-cicd python e2e/fabric-cicd/run.py
uv run --frozen --no-sync python e2e/vscode-extension/run.py
uv run --frozen --no-sync python e2e/airflow/run.py
python3 e2e/data-science-loop/run.py
```

Python dependencies are defined in the root `pyproject.toml` and locked by
`uv.lock`; dependency-bearing E2Es use their named dependency group, while
stdlib-only Docker launchers use `--no-sync`. The commands are
deterministic: virtual clock, in-memory stores, seeded credentials.
