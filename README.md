# fabric-emulator

[![CI](https://github.com/calvinchengx/fabric-emulator/actions/workflows/ci.yml/badge.svg)](https://github.com/calvinchengx/fabric-emulator/actions/workflows/ci.yml)
[![Docs](https://github.com/calvinchengx/fabric-emulator/actions/workflows/docs-site.yml/badge.svg)](https://calvinchengx.github.io/fabric-emulator/)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A clean-room, local emulator of **Microsoft Fabric**, built to compose with
[entra-emulator](https://github.com/calvinchengx/entra-emulator) — the control
plane (workspaces, items, RBAC, git, LROs) plus a real **OneLake** ADLS/Blob
data plane, a **T-SQL warehouse** over TDS, native **Livy** sessions on a real
Spark engine, **Data Factory** pipelines, and **KQL** eventhouses.

![fabric-emulator demo: a real Entra token, a workspace, a lakehouse, and a file written to and read back from OneLake — against two local binaries](docs/demo/demo.gif)

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

The bare binary runs none of the engines (clock-derived, milliseconds) — but
`docker compose up` auto-loads the override that attaches them, so the
documented path is engine-backed by default. Heavier engines (KQL, OpenMetadata)
stay behind opt-in profiles. Coverage floor is 90% (currently ~90%).

Docs: <https://calvinchengx.github.io/fabric-emulator/> — start with
[architecture](docs/03-architecture.md), the
[control-plane API](docs/07-control-plane-api.md), [OneLake](docs/08-onelake.md),
the [roadmap](docs/13-roadmap.md), [real compute](docs/14-real-compute.md), the
[warehouse over TDS](docs/16-warehouse-tds.md), [running modes](docs/27-running-modes.md), and the
[parity map](docs/parity.md).

## Parity at a glance

| | Rows | Meaning |
|---|---|---|
| 🟢 **Real** | 89 | Genuine work — real signed JWTs, real bytes on disk, a real engine or client computes |
| 🟡 **Emulated** | 17 | Faithful API contract and persisted state, but no engine behind it |
| 🟠 **Non-default engine** | 14 | Real on the JVM Spark overlay or an opt-in profile — *not* "bring your own": `docker compose up` already starts Sail and the SQL Server sidecar |
| 🔴 **Not implemented** | 19 | Deliberately out of scope — the parity map argues where the boundary sits and why |

Every 🟢 row names the witness that proves it, and a CI job fails the build if a
claim loses its witness. Full detail: [parity map](docs/parity.md).

## Relationship to entra-emulator

The two projects are decoupled — fabric-emulator depends on entra-emulator
**only over HTTP** (JWKS + issuer, plus a token-mint call for workspace
identities). A [`docker-compose.yml`](docker-compose.yml) brings up both with
fabric pre-wired to entra's issuer. It could equally point at a real Entra
tenant.

`docker compose up` attaches **real engines by default** — a Spark agent and a
SQL Server sidecar, via the auto-loaded
[`docker-compose.override.yml`](docker-compose.override.yml) — so Livy
sessions, notebook cells, and the T-SQL/TDS warehouse surface run for real out
of the box. `docker compose -f docker-compose.yml up` opts out to the lite,
contract-only pair.

Optional profiles add heavier engines only when asked for — nothing is pulled
otherwise. `--profile rti` attaches Microsoft's own KQL engine behind the
Eventhouse / KQL Database surface
([docs/25-rti-kusto.md](docs/25-rti-kusto.md)); `--profile governance` adds
OpenMetadata ([docs/22-openmetadata.md](docs/22-openmetadata.md)).

Overlay files *swap* an engine rather than add one. Layering
[`docker-compose.spark-jvm.yml`](docker-compose.spark-jvm.yml) runs the Spark
surface on the **JVM** instead of the default Sail, buying the capabilities
Spark Connect cannot express — the RDD API (`sc`), structured streaming,
`OPTIMIZE`/`VACUUM`, and Java/Scala UDFs — at the cost of image size and
startup ([docs/20-lakesail-engine.md](docs/20-lakesail-engine.md)).

## Getting started on Linux, macOS or Windows

The workflow is the same on all three — only the prerequisites differ:

```bash
make doctor   # toolchain, docker context, memory, ports — run this first
make up
make status   # "stack OK" is the real verdict; `make up` only means containers exist
```

Everything else, once it is running:

```bash
make help     # every target with a one-line description
make up-lite  # contract-only pair — no compute sidecars, honest 501s
make up-jvm   # swap the default Sail engine for JVM Spark
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
instead of letting it surface later as a broken recipe, an unreachable socket,
or a `?` in a status column. Per-platform detail — the `docker` group on Linux,
VM memory and Apple-silicon sidecar constraints on macOS, Rancher Desktop
context selection on Windows:
[docs/26-platform-setup.md](docs/26-platform-setup.md).

## Python tooling

Python packages, development dependencies, and E2E clients are managed by the
root uv workspace in [`pyproject.toml`](pyproject.toml) and [`uv.lock`](uv.lock).
Use frozen named groups so local, CI, and container runs resolve identically:

```bash
uv sync --frozen --group test
uv run --frozen --group test pytest
uv run --frozen --group governance python e2e/governance/run.py
```

The Docker Python runtimes are also built from these locked groups. Update a
dependency with `uv add --group <group> <package>`, then commit both project
files.

## License

Apache-2.0. Clean-room: built only from public documentation
([`MicrosoftDocs/fabric-docs`](https://github.com/MicrosoftDocs/fabric-docs)) and
public REST references — no Microsoft source.
