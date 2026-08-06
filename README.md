# fabric-emulator

[![CI](https://github.com/calvinchengx/fabric-emulator/actions/workflows/ci.yml/badge.svg)](https://github.com/calvinchengx/fabric-emulator/actions/workflows/ci.yml)
[![Docs](https://github.com/calvinchengx/fabric-emulator/actions/workflows/docs-site.yml/badge.svg)](https://calvinchengx.github.io/fabric-emulator/)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

[![go coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fcalvinchengx.github.io%2Ffabric-emulator%2Fcoverage-go.json)](https://calvinchengx.github.io/fabric-emulator/10-testing/)
[![python coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fcalvinchengx.github.io%2Ffabric-emulator%2Fcoverage-python.json)](https://calvinchengx.github.io/fabric-emulator/10-testing/)
[![portal coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fcalvinchengx.github.io%2Ffabric-emulator%2Fcoverage-portal.json)](https://calvinchengx.github.io/fabric-emulator/10-testing/)
[![parity claims witnessed](https://img.shields.io/endpoint?url=https%3A%2F%2Fcalvinchengx.github.io%2Ffabric-emulator%2Fwitnesses.json)](https://calvinchengx.github.io/fabric-emulator/parity/)
[![e2e suites](https://img.shields.io/endpoint?url=https%3A%2F%2Fcalvinchengx.github.io%2Ffabric-emulator%2Fe2e-suites.json)](https://calvinchengx.github.io/fabric-emulator/12-e2e-matrix/)

> Coverage measures the **unit** suites. What catches consumer-facing defects is
> the e2e fleet, which no percentage scores — hence *parity claims witnessed*
> beside it: every claim of support names a test that exists and ran.

A clean-room, local emulator of **Microsoft Fabric**, built to compose with
[entra-emulator](https://github.com/calvinchengx/entra-emulator) — the control
plane (workspaces, items, RBAC, git, LROs) plus a real **OneLake** ADLS/Blob
data plane, a **T-SQL warehouse** over TDS, native **Livy** sessions on a real
Spark engine, **Data Factory** pipelines, **Apache Airflow** jobs on a real
Airflow scheduler, and **KQL** eventhouses.

![the Data flow view drawing a medallion as it is built: bronze_orders to silver_orders to a warehouse gold table, with the event log filling in beside the graph](docs/demo/flow.gif)

Nothing there is mocked — a real Delta write, two Copy activities run by the
emulator's own executor, and the lineage it recorded. Two containers and one
script [regenerate it](docs/demo/README.md#regenerate-flowgif), so it cannot go
stale unnoticed.

The same view scales to a real pipeline, with no Azure subscription: three
source systems land, get conformed, resolve into one customer identity, and
reach a Warehouse star that Power BI queries. The run then publishes itself to
**OpenMetadata** — domain, glossary, metrics, contracts and lineage, all derived
from what it already knows rather than typed in twice. Every lineage edge
records **how** it is known, so you can tell what the emulator watched from what
a step merely claimed.

Run it yourself:

```bash
make up
cd examples/medallion-advanced-pyspark && uv sync --frozen && uv run python pipeline.py
```

Then open <https://localhost:9443/#flow> for the live flow, and
<http://localhost:8585> for the catalog. Twenty-three steps, about five
minutes, and the run asserts its own results — it is a test, not a demo
script. ([the example](examples/medallion-advanced-pyspark/) ·
[flow observability](docs/31-flow-observability.md) ·
[governance](docs/22-openmetadata.md))

## How it fits together

Real Fabric layers two independent systems: **Entra ID** issues the bearer
tokens, and the **Fabric control plane** (`https://api.fabric.microsoft.com/v1/…`)
serves workspaces, item CRUD, RBAC, git integration and long-running operations.
`entra-emulator` already emulates the first. `fabric-emulator` emulates the
second — and validates every incoming token against entra-emulator's JWKS,
**exactly as real Fabric validates against Entra**.

```
 client / SDK ──Bearer(aud=api.fabric.microsoft.com)──▶ fabric-emulator
                                                          │  validates token
                                                          ▼
                                                     entra-emulator  (JWKS, issuer)
```

![a real Entra token, a workspace, a lakehouse, and a file written to and read back from OneLake — against two local binaries](docs/demo/demo.gif)

## Why

- **Test Fabric CI/CD with no capacity.** `fabric-cicd`, git integration, and
  deployment pipelines drive item `getDefinition`/`updateDefinition` and
  `git/commitToGit`/`updateFromGit`. Point them at `localhost` instead of a paid
  tenant.
- **Test service-principal automation.** SP → Fabric client-credentials against a
  real (emulated) issuer, deterministic and offline.
- **Deterministic long-running operations.** Every Fabric mutation is async
  (`202` → poll `/v1/operations/{id}`). The emulator's clock control makes an LRO
  complete instantly or pins it in `Running` — impossible against real Fabric.

## Status

**Working** — the contract spine (P0–P3) *and* the real-compute track (R0–R5)
are shipped and CI-verified on Linux, macOS, and Windows.

- **Contract plane:** workspaces, items, RBAC, deterministic LROs; the CI/CD
  surface (definitions, typed aliases, connections + credential model, git
  integration, jobs — the real `fabric-cicd` tool publishes unmodified); the
  workspace-identity handshake with entra-emulator; and the OneLake ADLS-Gen2 +
  Blob data plane (managed folders, Delta put-if-absent commits, shortcuts).
- **Real compute (attached by default):** real **Spark** over a native Livy agent
  (interactive + high-concurrency sessions, notebook cell execution, Delta via
  ABFS); real **T-SQL over TDS** with Entra **FedAuth** terminated and the
  session byte-spliced to a **SQL Server** sidecar — driven by both `go-mssqldb`
  and Microsoft **ODBC Driver 18** (Microsoft's real `dbt-fabric` adapter passes
  end-to-end); **DuckDB** SQL over lakehouse Delta; and a pure-Go **pipeline**
  interpreter with real leaf activities. Real clients (delta-rs, the Azure Blob
  SDK, azcopy, PySpark, dbt) drive it in CI as borrowed oracles.
- **Real orchestration (opt-in sidecar):** `ApacheAirflowJob` items run on
  **genuine Apache Airflow** — Fabric's own code-first orchestrator *is* upstream
  Airflow, so the sidecar pins the versions Microsoft documents (2.10.5 on
  Python 3.12). DAG sources are stored as item definitions in OneLake, synced
  into the scheduler's DAG folder, and driven through Airflow's REST API for
  discovery, unpause, trigger and terminal-state polling. Real scheduler, real
  executor, real DAG semantics — no orchestration emulation at all. Attach it
  with `FABRIC_AIRFLOW_URL`; without it the routes answer
  `AirflowNotConfigured` rather than pretending. See
  [14-real-compute.md](docs/14-real-compute.md#e1) and `e2e/airflow`.

The bare binary runs none of the engines (clock-derived, milliseconds) — but
`docker compose up` auto-loads the override that attaches them, so the
documented path is engine-backed by default. Heavier services (KQL,
OpenMetadata, the terminal) sit behind profiles; `make up` enables `governance`
for you and the others are opt-in. Coverage floor is 90% (currently ~90%).

Docs: <https://calvinchengx.github.io/fabric-emulator/>

Start with the [end-to-end tutorial](docs/28-tutorial-end-to-end.md): Entra →
Key Vault → landing → bronze/silver → gold with dbt → semantic model, walking
through [`examples/medallion-pyspark`](examples/medallion-pyspark/) and executed
in CI. The [four medallion examples](examples/) scale it to three source systems
and both gold engines. Reference:
[architecture](docs/03-architecture.md), the
[control-plane API](docs/07-control-plane-api.md), [OneLake](docs/08-onelake.md),
[real compute](docs/14-real-compute.md), the
[warehouse over TDS](docs/16-warehouse-tds.md),
[flow observability](docs/31-flow-observability.md),
[running modes](docs/27-running-modes.md), the [roadmap](docs/13-roadmap.md), and
the [parity map](docs/parity.md).

## Parity at a glance

| | Rows | Meaning |
|---|---|---|
| 🟢 **Real** | 89 | Genuine work — real signed JWTs, real bytes on disk, a real engine or client computes |
| 🟡 **Emulated** | 17 | Faithful API contract and persisted state, but no engine behind it |
| 🟠 **Non-default engine** | 14 | Real on the JVM Spark overlay or an opt-in profile — *not* "bring your own": `docker compose up` already starts Sail and the SQL Server sidecar |
| 🔴 **Not implemented** | 19 | Deliberately out of scope — the parity map argues where the boundary sits and why |

Every 🟢 row names the witness that proves it, and a CI job fails the build if a
claim loses its witness. Full detail: [parity map](docs/parity.md).

## What `docker compose up` gives you

The two projects are decoupled — fabric-emulator depends on entra-emulator
**only over HTTP** (JWKS + issuer, plus a token-mint call for workspace
identities), so it could equally point at a real Entra tenant.

| Command | You get |
|---|---|
| `docker compose up` | both emulators **plus real engines** — a Spark agent and a SQL Server sidecar, via the auto-loaded [override](docker-compose.override.yml). Livy sessions, notebook cells and the T-SQL/TDS warehouse run for real |
| `docker compose -f docker-compose.yml up` | the lite, contract-only pair — honest `501`s on the engine surfaces |
| `--profile rti` | Microsoft's own KQL engine behind Eventhouse / KQL Database ([docs/25](docs/25-rti-kusto.md)) |
| `--profile governance` | OpenMetadata over the same state your pipelines write ([docs/22](docs/22-openmetadata.md)) |
| `--profile terminal` | a shell in the Flow view beside the graph — needs `-f docker-compose.terminal.yml` too ([docs/31](docs/31-flow-observability.md#the-terminal-pane)) |
| `-f docker-compose.spark-jvm.yml` | **swaps** Sail for JVM Spark, buying the RDD API, structured streaming, `OPTIMIZE`/`VACUUM` and Java/Scala UDFs at the cost of image size ([docs/20](docs/20-lakesail-engine.md)) |

Profiles pull nothing unless asked for — but **`make up` asks for `governance`
on your behalf**, so it starts 11 services rather than 6. `make up PROFILE=`
gives the lean stack. Memory: 2 GB for lite, 8 GB for the default six, 12 GB with
governance, 16 GB for everything at once —
[running modes](docs/27-running-modes.md) has the per-service measurements.

## Getting started on Linux, macOS or Windows

The workflow is the same on all three — only the prerequisites differ:

```bash
make doctor   # toolchain, docker context, memory, ports — run this first
make up       # 11 services incl. OpenMetadata; `make up PROFILE=` for the lean 6
make status   # "stack OK" is the real verdict; `make up` only means containers exist
```

Everything else, once it is running:

```bash
make help     # every target with a one-line description
make up-lite  # contract-only pair — no compute sidecars, honest 501s
make up-jvm   # swap the default Sail engine for JVM Spark
make status-spark  # status, plus a real Livy session executing Spark — the
              # only target that proves the engines are attached and computing
make seed     # catalog the emulator into OpenMetadata (governance profile)
make ps       # container states for this project
make logs     # tail logs (SVC=<service> to narrow to one)
make down     # stop and remove containers — volumes SURVIVE
make clean    # stop and remove containers AND delete the data volumes
make restart  # clean, then up
make test     # go build, vet and unit tests
```

Install the prerequisites once (a container runtime with Compose v2, plus GNU
Make; Python 3 is optional and only used by `make spark` / `make seed`):

```bash
# Linux
curl -fsSL https://get.docker.com | sh && sudo usermod -aG docker "$USER" && newgrp docker
```

```bash
# macOS  (Make and Python ship with the Xcode CLT; Docker Desktop / OrbStack work too)
xcode-select --install && brew install colima docker docker-compose && colima start --memory 8
```

```powershell
# Windows  (Git supplies sh.exe + grep/awk/curl; ezwinports supplies make)
winget install Git.Git; winget install ezwinports.make
```

`make doctor` is the entry point on every platform: it names what is missing
rather than letting it surface later as a broken recipe or a `?` in a status
column. Per-platform detail (the `docker` group, VM memory, Apple-silicon
sidecars, Rancher Desktop contexts):
[docs/26-platform-setup.md](docs/26-platform-setup.md).

## Python tooling

Packages, dev dependencies and e2e clients live in the root uv workspace
([`pyproject.toml`](pyproject.toml), [`uv.lock`](uv.lock)). Use frozen named
groups so local, CI and container runs resolve identically:

```bash
uv sync --frozen --group test
uv run --frozen --group test pytest
uv run --frozen --group governance python e2e/governance/run.py
```

The Docker Python runtimes build from the same locked groups. Add a dependency
with `uv add --group <group> <package>` and commit both files.

## License

Apache-2.0. Clean-room: built only from public documentation
([`MicrosoftDocs/fabric-docs`](https://github.com/MicrosoftDocs/fabric-docs)) and
public REST references — no Microsoft source.
