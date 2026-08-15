# 27 — Running modes: default, swapped engine, real Fabric

The emulator runs in five shapes. This page is the map: what each one starts,
how to confirm it actually works, and when you would want it. Every mode has a
`make` target, so the commands are identical on Linux, macOS and Windows.

If you only want to get going, [the quickstart](01-quickstart.md) walks mode 1
end to end. Come back here when you need to change something.

### Choose by what you need to do

| I want to work on… | Command | Services | Idle | Give the runtime |
|---|---|---|---|---|
| control plane, OneLake, RBAC, git, CI/CD | `make up-lite` | 3 | **~65 MB** | 2 GB |
| …plus Spark and the T-SQL warehouse | `docker compose up` | 6 | ~1 GB | **8 GB** |
| …plus catalog, lineage, glossary, Airflow | `make up` | 12 | ~3 GB | **13 GB** |
| …plus KQL (Eventhouse) | add `--profile rti` | 13 | +4 GB | +4 GB |
| …plus a shell in the Flow view | add `--profile terminal` | 14 | +5 MB | +5 MB |
| Spark features Sail lacks (RDD, JVM UDFs) | `make up-jvm` | 6 | ~1 GB | 10 GB |
| full DAX against `msmdsrv` (optional oracle) | a machine you own — [52](52-msmdsrv-hosts.md) | +Windows guest | — | **8–16 GB**; not `make up`; not `macos-latest` / `ubuntu-latest` |
| the real Fabric service | `FABRIC_TARGET=real` | 0 | — | — |

**Idle is not the number that matters, and it is nowhere near the other one.**
A freshly booted lite stack is 65 MB; the same three containers writing Delta are
a few hundred. Sail costs 36 MB to start and ~1.9 GB to run PySpark through. So
the column to size against is the last one — and the reason it is so much larger
is the work, not the container count. [Full measurements below.](#what-it-costs-to-run)

| Mode | Spark | T-SQL | Extras |
|---|---|---|---|
| 1. Default — `make up` | Sail | SQL Server | OpenMetadata, Airflow |
| 2. Plain compose — `docker compose up` | Sail | SQL Server | — |
| 3. Lite — `make up-lite` | ❌ 501 | ❌ 501 | — |
| 4. JVM overlay — `make up-jvm` | **JVM Spark** | SQL Server | — |
| 5. Real Fabric — `FABRIC_TARGET=real` | the real service | | |

Whatever you start, `make status` is the verdict. `make up` only means
containers were created; `status` probes the endpoints and reports `stack OK`
or names what is broken — including the failure no health check catches, a
container that is up but attached to no network.

## 1. Default — `make up`

The everyday mode, and the one the parity map grades against.

```bash
make doctor   # toolchain, docker context, memory, ports — run this first
make up
make status
```

You get the family (entra-emulator, fabric-emulator, azure-keyvault-emulator)
**plus real compute**: Sail as the Spark engine behind the Livy surface, and a
SQL Server sidecar behind the T-SQL/TDS warehouse. Livy sessions, notebook
cells, and warehouse queries all do real work — nothing to attach.

`make up` also brings up the **governance profile** (OpenMetadata and its
Postgres/Elasticsearch), because the quickstart advertises it. That is eleven
containers. To skip it:

```bash
make up PROFILE=          # same stack, no OpenMetadata
```

## 2. Plain compose — `docker compose up`

Worth knowing that this is **not** the same as `make up`:

```bash
docker compose up         # 6 services  — no governance profile
make up                   # 11 services — adds OpenMetadata
```

Compose auto-loads [`docker-compose.override.yml`](../docker-compose.override.yml),
which is what attaches Sail and SQL Server. That is why engines are **opt-out,
not opt-in** — a distinction the parity map's 🟠 mark now turns on.

## 3. Lite — `make up-lite`

The contract-only pair: control plane, OneLake, identity. No compute sidecars,
so the Spark and T-SQL surfaces answer an honest `501` instead of pretending.

```bash
make up-lite
```

Reach for it when you are testing the control plane, CI/CD, git integration or
RBAC and do not want to wait for engines you will not use. Naming `-f` explicitly
is what makes Compose skip the auto-override:

```bash
docker compose -f docker-compose.yml up -d    # what the target runs
```

## 4. JVM overlay — `make up-jvm`

Swaps the default Sail engine for **JVM Spark 3.5.5** (Java 11, Delta 3.2) — the same engine real
Fabric Runtime 1.3 uses, so it is the higher-fidelity option.

```bash
make up-jvm
```

Note this *swaps* rather than adds: the `sail` service is gone and the statement
agent becomes a classic in-process Spark session.

**Reach for it when your test touches** the RDD/`SparkContext` API, a
**checkpointed** streaming query (`foreachBatch` on an engine stream, or
resume from `checkpointLocation`), Java/Scala UDFs, `spark.jars`, or a
CDF-enabled table you need Spark itself to author. Durable streaming *sinks*
(delta/parquet/memory), OSS `format("kafka")` on Sail, and the Fabric
Eventstream notebook API land one announced micro-batch on the default Livy
path. The ❌ rows that stay red
are in [the engine matrix](engine-matrix.md), which measures both engines
with the same probes rather than asserting.

**What it costs**, measured on the same 19 probes:

| | Sail (default) | JVM overlay |
|---|---|---|
| Image size | 943 MB | 2.1 GB |
| Run output | 125 log lines | 78,040 log lines |
| Startup | seconds | minutes |

It is not the default because most tests never touch those capabilities, and
the common notebook path — Delta write/append, both time-travel forms,
`createDataFrame`, SQL, `readStream` — passes on **both**.

## Profiles — extra services, only when asked

A profile *adds* services rather than swapping them, and nothing is pulled
unless you name it. `--profile` is repeatable, so combine them freely.
`governance` and `airflow` are already in `make up`'s default `PROFILE`;
overriding `PROFILE` replaces that default rather than adding to it, so name
them again if you still want them.

| Profile | Adds | Gives you | Costs |
|---|---|---|---|
| `governance` | OpenMetadata + Postgres + OpenSearch | catalog, glossary, lineage over the state your pipelines wrote ([22](22-openmetadata.md)) | ~2.8 GB |
| `airflow` | `apache/airflow` scheduler + webserver | `ApacheAirflowJob` items run on genuine Airflow ([14](14-real-compute.md#e1)) | ~1.1 GB |
| `rti` | `kustainer` | Microsoft's own KQL engine behind Eventhouse ([25](25-rti-kusto.md)) | 4 GB (its own `mem_limit`) |
| `eventstream` | `kafka` (`apache/kafka` KRaft) | Fabric Eventstream notebook API on Sail (default) and the JVM overlay ([51](51-eventstream-kafka.md)) | ~400 MB |
| `terminal` | `ttyd` | a shell in the Flow view, beside the graph ([31](31-flow-observability.md#the-terminal-pane)) | negligible |

```bash
make up PROFILE="--profile rti"                          # KQL only — drops governance and airflow
make up PROFILE="--profile governance --profile airflow --profile rti"
```

`--profile rti` needs **amd64 with AVX2**. Microsoft documents ARM as
unsupported and Rosetta does not supply AVX2 — on Apple silicon it needs an
x86-64 VM with a QEMU CPU type that provides it.

There is **no `--profile dax`**. Analysis Services is a Windows process.
macOS uses UTM on the **host you own**; Linux uses `make dax-linux`
(Docker Engine + `/dev/kvm` on the metal); Windows runs Desktop on the
machine. Those are not GitHub `macos-latest` / `ubuntu-latest` — those
runners are already VMs, and nested Windows is not a CI job. The CI
oracle is `e2e/pbix-desktop` on `windows-latest`. OrbStack and Rancher
Desktop on a Mac cannot pass KVM into `dockur/windows`. The guest then
runs `pwsh e2e/msmdsrv/start.ps1` and the emulator sets `FABRIC_DAX_URL`.
See [52-msmdsrv-hosts.md](52-msmdsrv-hosts.md).

### The eventstream profile needs two things

The profile starts Kafka; a second file tells the emulator where it is. Both,
or Spark resolution 501s. Works on Sail (the default `docker compose up`
engine) without the JVM overlay:

```bash
docker compose --profile eventstream \
  -f docker-compose.yml -f docker-compose.override.yml \
  -f docker-compose.eventstream.yml \
  up -d
```

Add `-f docker-compose.spark-jvm.yml` when you want the native OSS Kafka
source instead of the Sail LocalRelation wrap.

See [51-eventstream-kafka.md](51-eventstream-kafka.md).

### The terminal profile needs two things

The profile starts `ttyd`; a second file tells the emulator where it is. Both,
or the pane never appears:

```bash
docker compose --profile terminal \
  -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.terminal.yml \
  up -d
```

Then read the token the emulator printed at startup
(`docker compose logs fabric-emulator | grep terminal`), open
<https://localhost:9443/#flow>, click **Terminal** and paste it. The token is
deliberately not served by any endpoint — the portal is unauthenticated, so an
endpoint handing it out would be the same as having none.

> **Naming any `-f` disables the auto-loaded override.** That is why all three
> files are listed above. Leave `docker-compose.override.yml` out and the stack
> still starts — without Sail, the Spark agent or SQL Server, so Livy and the
> warehouse answer `501` while everything looks healthy.
>
> Nothing cheap catches that: `docker compose ps` shows only what you asked for,
> and plain `make status` reports containers and endpoints without asserting which
> services *should* exist, so it prints `stack OK`. `make status-spark` opens a
> real Livy session and is the check that fails.

## Everything at once

Every engine and every profile — what CI's heaviest legs approximate, and the
most a laptop will be asked for:

```bash
docker compose --profile governance --profile rti --profile terminal \
  -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.terminal.yml \
  up -d
```

Thirteen services, and the most a laptop will be asked for.

## What it costs to run

Measured with `docker stats --no-stream` on a 16 GB machine, idle and then
under an active medallion. Two columns because the difference is the whole
point: the emulator is nothing at rest and grows only while it is doing work.

| Container | Idle | Under a running medallion |
|---|---|---|
| `fabric-emulator` | **5 MiB** | **0.9 – 1.3 GiB** |
| `entra-emulator` | 8 MiB | 38 MiB |
| `keyvault-emulator` | 4 MiB | 7 MiB |
| `sail` (Spark Connect) | 50 MiB | 1.8 GiB |
| `spark-agent` | 88 MiB | 95 MiB |
| `sqlserver` (warehouse) | 880 MiB fresh, 380 MiB once trimmed | 1.6 GiB |
| `om-opensearch` | 1.0 GiB | 1.5 GiB |
| `openmetadata` | 940 MiB | 940 MiB |
| `om-postgresql` | 8 MiB | 100 MiB |
| `airflow` (2.10.5) | **1.09 GiB** | 1.1 GiB+ |
| `ttyd` | a few MiB | a few MiB |
| `kustainer` (RTI) | not measured — amd64/AVX2 only | — |

Which gives, per mode:

| Mode | Services | Idle | Under load |
|---|---|---|---|
| `make up-lite` | 3 — control plane, OneLake, portal | **~20 MiB** | ~1 GiB |
| `docker compose up` | 6 — adds Sail, spark-agent, SQL Server | **~530 MiB** | ~4.5 GiB |
| **`make up`** (the default) | 10 — adds the catalog and Airflow | **~3.6 GiB** | **~7.5 GiB** |

`make up` is the heavy one on purpose: `PROFILE ?= --profile governance --profile
airflow` attaches every real runtime, because each backs a first-class Fabric
item type and a capability that answers "not configured" looks broken rather
than optional. Drop what you do not need:

```bash
make up PROFILE="--profile governance"   # no Airflow  (-1.1 GiB)
make up-jupyter                          # + JupyterLab on :8888 (docs/44)
make up PROFILE=                         # no catalog, no Airflow  (-3 GiB)
make up-lite                             # contract only
```

**CPU is not the constraint, and that is worth knowing before you size a VM.**
Peak during a light run — a Delta write via delta-rs plus two Copy activities —
was `sqlserver` 18%, `sail` 12%, `fabric-emulator` 1.2%. Those are fractions of a
*single* core. Four cores is ample for control-plane and pipeline work; six to
eight if you drive PySpark or warehouse queries in anger. One caveat for Apple
silicon: `sqlserver` is the only amd64 container in the stack, so it runs under
Rosetta and costs more CPU there than these figures suggest.

Three things worth taking from that:

- **The emulator itself is not the cost.** At rest it is ~5 MiB against a 22 MB
  image — a single static Go binary with the portal embedded. What costs memory
  is the real engines it attaches: a JVM-free Sail, a real SQL Server, a real
  OpenSearch. That is the trade the project makes on purpose, and
  [14-real-compute.md](14-real-compute.md) is the argument for it.
- **It is not free under load.** 5 MiB idle becomes ~1 GiB while a medallion
  runs, because Delta writes and Copy activities pass through the emulator's own
  storage layer. Size the box for the work, not for the idle screenshot.
- **Sail is why the Spark tier is affordable at all.** 50 MiB idle where a JVM
  Spark would be 1–2 GiB, and a 943 MB image against 2.1 GB (the table above).

[01-quickstart.md](01-quickstart.md) asks for **8 GB** of runtime memory, which
covers everything here with room for the containers under test beside it. A
control-plane-only loop needs a fraction of that.

To re-derive any of this:

```bash
docker stats --no-stream --format '{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}'
```

## 5. Real Fabric — one environment variable

The same Python code runs against the real service with no edits. This is a
client-side switch, not a compose mode, so nothing above applies:

```bash
pip install ./python/fabric-target

export FABRIC_TARGET=emulator             # local — the default, zero config
python my_pipeline.py

az login                                  # real: your own identity…
export FABRIC_TARGET=real
export FABRIC_WORKSPACE=my-workspace-name # …scoped to one workspace, always
python my_pipeline.py                     # same code
```

```python
from fabric_target import target
t = target()
ws = t.workspace("analytics")   # names, not GUIDs — they differ per target
s  = t.session()                # authed, TLS-aware, 429-honouring
s.post(f"/workspaces/{ws.id}/items", json={"displayName": "nb", "type": "Notebook"})
```

Two guardrails are deliberate. Real mode **refuses to start** without a
credential source (`az login` or `AZURE_*`) and never falls back to the seeded
dev values — so a misconfigured run fails instead of silently testing the
emulator. And it is **always scoped to one workspace**, so a destructive test
cannot wander across a tenant.

Non-Python tools get the same switch as environment variables:

```bash
eval "$(python -m fabric_target env real)"
```

Design and phasing: [21-real-fabric-toggle.md](21-real-fabric-toggle.md). The
conformance suite runs the *same* tests against both targets — the emulator leg
on every push, the real leg behind a secret-gated workflow.

## Switching between modes

Compose reuses containers by project name, so switch cleanly rather than
layering one mode on another:

```bash
make down     # stop, keep the data volumes
make clean    # stop AND delete the volumes — full reset
make up-jvm   # then start the mode you want
```

If something looks wrong after a switch, `make status` first: a stale container
from a previous mode is the usual cause, and it names that rather than leaving
you to infer it from a health column.
