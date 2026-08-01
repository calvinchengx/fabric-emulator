# 27 — Running modes: default, swapped engine, real Fabric

The emulator runs in five shapes. This page is the map: what each one starts,
how to confirm it actually works, and when you would want it. Every mode has a
`make` target, so the commands are identical on Linux, macOS and Windows.

If you only want to get going, [the quickstart](01-quickstart.md) walks mode 1
end to end. Come back here when you need to change something.

| Mode | Command | Spark | T-SQL | Extras |
|---|---|---|---|---|
| 1. Default | `make up` | Sail | SQL Server | OpenMetadata |
| 2. Plain compose | `docker compose up` | Sail | SQL Server | — |
| 3. Lite | `make up-lite` | ❌ 501 | ❌ 501 | — |
| 4. JVM overlay | `make up-jvm` | **JVM Spark** | SQL Server | — |
| 5. Real Fabric | `FABRIC_TARGET=real` | the real service | | |

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

Swaps the default Sail engine for **JVM Spark 3.5** — the same engine real
Fabric Runtime 1.3 uses, so it is the higher-fidelity option.

```bash
make up-jvm
```

Note this *swaps* rather than adds: the `sail` service is gone and the statement
agent becomes a classic in-process Spark session.

**Reach for it when your test touches** the RDD/`SparkContext` API, a durable
streaming sink (delta/parquet/memory), Java/Scala UDFs, `spark.jars`, or a
CDF-enabled table you need Spark itself to author. Those are exactly the ❌ rows
in [the engine matrix](engine-matrix.md), which measures both engines with the
same probes rather than asserting.

**What it costs**, measured on the same 19 probes:

| | Sail (default) | JVM overlay |
|---|---|---|
| Image size | 943 MB | 2.1 GB |
| Run output | 125 log lines | 78,040 log lines |
| Startup | seconds | minutes |

It is not the default because most tests never touch those capabilities, and
the common notebook path — Delta write/append, both time-travel forms,
`createDataFrame`, SQL, `readStream` — passes on **both**.

## Profiles — heavier engines, only when asked

Profiles *add* an engine rather than swapping one, and nothing is pulled unless
you name it:

```bash
make up PROFILE="--profile rti"          # Microsoft's own KQL engine (kustainer)
make up PROFILE="--profile governance"   # OpenMetadata (already the make default)
```

`--profile rti` needs **amd64 with AVX2**. Microsoft documents ARM as
unsupported, and Rosetta does not supply AVX2 — on Apple silicon it needs an
x86-64 VM with a QEMU CPU type that provides it. See
[25-rti-kusto.md](25-rti-kusto.md).

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
