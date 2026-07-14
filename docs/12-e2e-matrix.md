# 12 — E2E matrix

What CI proves on every push, and what's queued. The bar, inherited from
entra-emulator's SDK matrix: **real clients, unmodified**, against the
emulator — because driving Microsoft's actual tools catches fidelity gaps
spec-reading cannot (see [what fabric-cicd caught](11-testing-with-fabric-cicd.md#what-driving-the-real-tool-caught)).

## Verified on every push

Go integration tests start a real entra-emulator in-process and drive the
full HTTP surface; the remaining suites drive real third-party clients and
engines against the running emulator.

| Suite | Client | Proves | Where |
|---|---|---|---|
| Token handshake | in-process **entra-emulator** | real client-credentials token (Fabric aud) → JWKS validation → full workspace/RBAC/item/LRO flow over HTTP | Go integration tests (CI `test`, Linux + macOS + Windows) |
| Git round-trip | Go HTTP | two-workspace commit→update, definitions intact, logical ids preserved | Go integration tests |
| Identity handshake | in-process entra-emulator | provision → entra mints for the identity → token passes fabric RBAC → deprovision revokes → delete cascades | Go integration tests |
| OneLake | Go HTTP + real entra Storage tokens | create/append/flush/read via GUID + name addressing, listings, RBAC walls, managed-folder rejections | Go integration tests |
| **fabric-cicd** | Microsoft's real Python tool (v1.2.x) | `publish_all_items` publishes a notebook; parts round-trip byte-for-byte | `e2e/fabric-cicd/run.py` (CI `fabric-cicd`, 3-OS) |
| **Delta write/read (A1)** | real `deltalake` (delta-rs) | a real engine writes/reads a Delta table through the OneLake Blob surface with an entra Storage token — Range reads + the `_delta_log` put-if-absent commit primitive | `e2e/delta-rs/run.py` (CI `delta-rs`, 3-OS) |
| **ADLS SDK** | Microsoft's real `azure-storage-blob` | Parquet upload → byte-identical download (exercising `x-ms-range`), `list_blobs`, and the DFS surface sees the same file | `e2e/adls-sdk/run.py` (CI `adls-sdk`, 3-OS) |
| **azcopy** | Microsoft's real `azcopy` binary | multi-block upload (Put Block + Put Block List) → byte-identical download, and the DFS surface sees the same object | `e2e/azcopy/run.py` (CI `azcopy`, Linux) |
| **Spark via ABFS (A2)** | real PySpark + delta-spark | writes/reads a Delta table over `abfss://…@onelake.dfs…` with OAuth against entra | `e2e/spark/run.py` (CI `spark-a2`, Linux, containerized) |
| **Native Livy** | real Livy REST client + real Spark | emulator terminates the Livy protocol itself and drives a Spark agent — session + PySpark statements computed by real Spark, no Apache Livy server | `e2e/livy/run.py` (CI `livy-native`, Linux) |
| **dbt (fabric-spark)** | Microsoft's real `dbt-fabricspark` adapter | a dbt project (debug → seed → run → test) over the Fabric REST + Livy HC surface, models computed by real Spark | `e2e/dbt-fabricspark/run.py` (CI `dbt-fabricspark`, Linux) |
| **dbt (fabric) via ODBC** | Microsoft's real `dbt-fabric` adapter + Microsoft ODBC Driver 18 | a dbt project (debug → seed → run → test) over the TDS warehouse surface through pyodbc + FedAuth (byte-spliced to a real SQL Server) — the **second** independent TDS driver family | `e2e/dbt-fabric/run.py` (CI `dbt-fabric`, Linux) |
| **DuckDB SQL** | real DuckDB | SQL (aggregation, join, filter) over Delta tables in the OneLake plane — the lakehouse SQL-analytics-endpoint semantics | `e2e/duckdb/run.py` (CI `duckdb`, 3-OS) |
| **notebookutils** | real Fabric notebook | the functional `notebookutils` shim: fs over OneLake, credential tokens, Key Vault secret brokering, lakehouse control plane, `notebook.run` | `e2e/notebookutils/run.py` (CI `notebookutils`, 3-OS) |
| **Notebook execution** | real Spark | emulator parses a Fabric notebook into cells; real Spark executes them against OneLake (a Delta table lands) and the run reports back | `e2e/notebook-run/run.py` (CI `notebook-run`, Linux) |
| **Warehouse TDS** | real `go-mssqldb` + real SQL Server 2022 | entra-token connect, then DDL + DML + a GROUP BY relayed through the TDS endpoint — **one of two** independent TDS driver witnesses (the other: Microsoft ODBC Driver 18 via `dbt-fabric` above); plus the SQL Database → OneLake Delta mirror, the pipeline Script/SqlServerStoredProcedure activities over real HTTP + jobs, and an external-source MirroredDatabase mirror (seeded on a database reached independently of the emulator's own per-item routing) | CI `warehouse-tds` (Linux) |

Plus: coverage floor 90% (cross-package; currently ~95%), `go vet`, a
distroless container smoke (`docker-smoke`), the portal build + headless
render (`portal`), and the
[docs site](https://calvinchengx.github.io/fabric-emulator/) build on every
docs push.

## Queued (designed, not yet wired)

Nothing queued — every designed real-client suite is wired above. New
milestones (e.g. non-OneLake external storage for shortcuts) will land here as
they're scoped.

## Running locally

```bash
go test ./...              # everything in-process, no network
python3 e2e/fabric-cicd/run.py   # a real-tool e2e (needs Python 3 + go); see e2e/ for the rest
```

Both are deterministic: virtual clock, in-memory stores, seeded credentials.
