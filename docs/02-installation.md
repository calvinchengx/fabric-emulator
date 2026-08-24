# 02 — Installation

**Run this with Docker.** One command brings up the emulated stack wired
together — the Fabric control plane, a real OneLake data plane, the Entra
issuer it trusts, a real Spark engine, and the T-SQL warehouse — with the
issuer alignment already done. That is the supported path and the one every
example in these docs assumes.

The standalone binary is a real install (see [below](#the-binary-on-its-own)),
but it is the *emulator process alone*: no engine, no warehouse sidecar, and
you supply an issuer yourself. Reach for it when you want the CLI or you are
embedding the control plane in something else — not when you want Fabric.

## Docker Compose — the whole stack

```bash
git clone https://github.com/calvinchengx/fabric-emulator
cd fabric-emulator
docker compose up
```

The clone is the point: the compose files *are* the wiring, so this is not a
step you can skip by pulling an image.

**Sail is already the default engine.** `docker-compose.override.yml` is
auto-loaded by Compose whenever you do not name files with `-f`, and it attaches
[LakeSail's Sail](20-lakesail-engine.md) — a Rust Spark-Connect server, no JVM
anywhere — behind the Livy statement agent, plus a SQL Server sidecar for the
T-SQL/TDS surface. No extra flag buys you real compute; you already have it
([14-real-compute.md](14-real-compute.md) is the full account).

Seven services, and what each one publishes:

| Service | Port | What it is |
|---|---|---|
| `fabric-emulator` | `9443`, `1433` | control plane, OneLake, and the warehouse TDS surface |
| `entra-emulator` | `8443` | the issuer whose tokens everything above trusts |
| `keyvault-emulator` | `8444` | secrets, for pipelines that resolve them |
| `arm-emulator` | `8445` | the ARM surface behind workspace provisioning |
| `sail` | `50051` | Spark Connect — PySpark clients can attach directly at `sc://localhost:50051` |
| `spark-agent` | — | the statement executor the emulator drives for Livy sessions and notebook cells |
| `sqlserver` | — | the warehouse engine; reached through `fabric-emulator:1433`, not directly |

Then open <https://localhost:9443> and go on to the
[quickstart](01-quickstart.md).

### Running less, or more

```bash
# Contract-only: four services, no engines, honest 501s on the Spark and SQL
# surfaces. Naming the base file explicitly is what makes Compose skip the
# override -- that is the opt-out mechanism, not a flag.
docker compose -f docker-compose.yml up
```

Profiles pull nothing unless asked for. `--profile governance` adds
OpenMetadata over the same state your pipelines write ([22](22-openmetadata.md));
`--profile airflow` adds a real Airflow scheduler; `--profile rti` attaches
Microsoft's own KQL engine ([25](25-rti-kusto.md)); `--profile eventstream`
plus `-f docker-compose.eventstream.yml` attaches Apache Kafka
([51](51-eventstream-kafka.md)). `-f docker-compose.spark-jvm.yml` **swaps**
Sail for JVM Spark, buying the RDD API, structured streaming and JVM UDFs at
the cost of image size ([20](20-lakesail-engine.md)).

The `make` targets are a convenience wrapper over exactly these commands, not a
second way to run things — `make up` is `docker compose` with `governance` and
`airflow` already asked for, which is 15 services rather than 7. Make is
optional; Docker is not.

```bash
make help     # every target with a one-line description
make doctor   # toolchain, docker context, memory, ports
make status   # "stack OK" is the real verdict -- `up` only means containers exist
```

### What it costs to run

Give the container runtime **8 GB** for the default seven, **13 GB** with
governance and Airflow, **2 GB** for the contract-only four, **17 GB** for
everything at once. Four cores is ample; six to eight if you drive PySpark or
warehouse queries.

Idle is nowhere near those numbers — a freshly booted contract-only stack is
65 MB and the default set about 1 GB. The spread is the *work*, not the
container count: Sail costs 36 MB to start and ~1.9 GB to push PySpark through,
so an engine you do not drive is nearly free.
[Per-service measurements](27-running-modes.md#what-it-costs-to-run).

### Prerequisites

A container runtime with Compose v2. That is the whole list for
`docker compose up`; the `make` targets add GNU Make, and Python 3 is optional
(`make spark`, `make seed`).

```bash
# Linux
curl -fsSL https://get.docker.com | sh && sudo usermod -aG docker "$USER" && newgrp docker
```

```bash
# macOS -- Docker Desktop and OrbStack work too
brew install colima docker docker-compose && colima start --memory 8
```

```powershell
# Windows -- Git supplies sh.exe and friends; ezwinports supplies make
winget install Git.Git; winget install ezwinports.make
```

Details that bite per platform — the `docker` group on Linux, the VM memory cap
and Apple-silicon sidecar constraints on macOS, `sh.exe` and GNU Make on
Windows — are in [26-platform-setup.md](26-platform-setup.md). `make doctor`
checks the result on any of them and names what is missing rather than letting
it surface later as a broken recipe.

## One container, without the stack

```bash
docker run --rm -p 9443:9443 \
  -e FABRIC_ENTRA_ISSUER="https://host.docker.internal:8443/6f89cf12-978b-4d23-ac18-9ef0c127cf87/v2.0" \
  -e FABRIC_ENTRA_TLS_INSECURE=true \
  ghcr.io/calvinchengx/fabric-emulator:latest
```

Distroless, multi-arch (amd64/arm64), with a built-in `HEALTHCHECK` — the
binary probes its own `/health`, since there is no shell in the image. State
lives in `/data`; mount it to persist.

This is the emulator **on its own**: it still needs an issuer to trust, which
is what the `FABRIC_ENTRA_ISSUER` above points at, and it has no Spark engine
and no warehouse sidecar. The Spark and SQL surfaces answer `501`.

## The binary on its own

For the CLI, or to embed the control plane. Same caveats as the single
container: no engines, and you supply an issuer.

```bash
# macOS / Linux
brew install calvinchengx/tap/fabric-emulator
```

```bash
# Anywhere Go runs -- needs Go >= 1.25
go install github.com/calvinchengx/fabric-emulator/cmd/fabric-emulator@latest
```

Pure Go all the way down (`modernc.org/sqlite`) — no CGO and no runtime
dependencies, so it cross-compiles anywhere and is CI-tested on Linux, macOS
and Windows.

Tagged releases also carry `tar.gz`/`zip` archives per OS/arch plus
`checksums.txt`:
<https://github.com/calvinchengx/fabric-emulator/releases>. Or build it:

```bash
git clone https://github.com/calvinchengx/fabric-emulator
cd fabric-emulator
go build ./cmd/fabric-emulator
```

> **winget is not available yet.** Manifests have been
> [submitted](https://github.com/microsoft/winget-pkgs/pulls?q=calvinchengx.fabric-emulator)
> but none has landed, so `winget install calvinchengx.fabric-emulator` does not
> work today — the moderation queue moves in days and tags here land several
> times a day. On Windows, use Docker, `go install`, or the archives.

### Verify

```bash
fabric-emulator version      # stamped by the release pipeline; "dev" from source
fabric-emulator healthcheck  # exit 0 when a local instance is healthy
```

The server needs exactly one thing to start: an issuer to trust
(`FABRIC_ENTRA_ISSUER` or `-entra-issuer`) — see
[configuration](04-configuration.md). Compose sets it for you, which is most of
why it is the recommended path.
