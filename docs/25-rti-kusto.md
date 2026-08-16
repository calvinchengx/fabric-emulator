# 25 — Real-Time Intelligence: a real KQL engine behind Eventhouse

**Status: shipped, opt-in sidecar, graded Real.** Eventhouse / KQL Database
execution is real when Microsoft's own KQL engine container is attached, and
an honest 501 when it is not. `--profile rti` stays opt-in — the engine needs
AVX2, which Rosetta does not provide. This is Tier 2, item 2 of
[24-parity-completion.md](24-parity-completion.md) — the same move the repo
made for T-SQL (terminate the protocol ourselves, byte-relay to a real SQL
Server) and for Spark (drive a real Sail/JVM engine behind the Livy contract).

## The shape of the real thing

Fabric does **not** put KQL on the control plane. An Eventhouse item publishes
a cluster URI, and clients then speak the **Kusto REST protocol** to it — the
sequence Microsoft's own tutorial follows
(`fabric-docs real-time-intelligence/eventhouse-deploy-with-fabric-api.md`,
`.../map/tutorial-create-real-time-map-python.md`):

```text
GET  /v1/workspaces/{ws}/eventhouses/{id}
     → properties.queryServiceUri, properties.ingestionServiceUri,
       properties.databasesItemIds

POST {queryServiceUri}/v1/rest/mgmt    {"db": "<database displayName>", "csl": ".create-merge table T(…)"}
POST {queryServiceUri}/v1/rest/query   {"db": "<database displayName>", "csl": "T | count"}
POST {queryServiceUri}/v2/rest/query   (the frame-stream dialect the SDKs use)
```

Creating an eventhouse also creates a **default child KQL database with the
same name** (`create-eventhouse.md`), and further databases are created with a
`creationPayload` carrying `parentEventhouseItemId`. All of that is implemented
(`internal/api/kql.go`); the KQL itself is not.

## The engine

`mcr.microsoft.com/azuredataexplorer/kustainer-linux:latest` — Microsoft's own
Kusto engine in a container (the Azure Data Explorer "Kusto emulator").

| | |
|---|---|
| Start | `ACCEPT_EULA=Y` is mandatory; the image exits 13 without it |
| Port | 8080, **HTTP only** — the engine supports neither TLS nor Entra |
| Auth | none; it is a trusted sidecar on the compose network |
| Readiness | `POST /v1/rest/mgmt {"csl":".show cluster"}` → 200 (needs no database) |
| Ingestion | **no data-management service**: queued ingestion and the `ingest-` endpoint are unsupported. Direct ingestion commands (`.set-or-append`, `.ingest inline`) run on the engine itself |
| Memory | 4 GB recommended (Microsoft's own examples pass `-m 4G`) |
| **Platform** | **linux/amd64, and it needs AVX2** — the constraint is the instruction set, not the host. The engine's native layer aborts without AVX2 (`libKusto.NativeInfra.so failed … Crash_FailSlow`). Rosetta translates x86-64 but stops at SSE4.2, so `--platform linux/amd64` on Apple silicon crashes on boot; a QEMU x86-64 VM with `--cpu-type max` supplies AVX2 and the engine runs. Both directions verified on this repo's own hardware — see [below](#running-it-on-apple-silicon). |

## What the emulator does (and does not)

The emulator terminates Fabric's half of the contract and relays the KQL:

- **Bearer validation on the Kusto audience** (`https://kusto.fabric.microsoft.com`,
  plus ADX's `https://api.kusto.windows.net`) — a control-plane token is
  rejected, as in real Fabric.
- **Workspace RBAC**: a management command mutates and needs Contributor; a
  query needs Viewer.
- **Isolation**: each Fabric KQL Database gets its own engine-side database,
  named from the item id, created on first use. Two databases under one
  eventhouse cannot see each other's tables even with identical names. The
  internal name is mapped back to the Fabric display name on the way out, so a
  client never sees it.
- **Honest failure**: no engine → 501 in Kusto's own error envelope; an
  unreachable or erroring engine → 502. Never a synthesized result set.

Everything past that — parsing, planning, evaluating, ingesting — is the
engine's. The emulator has no KQL implementation and will not grow one.

### Endpoint addressing

Real Fabric gives every eventhouse a host of its own
(`<guid>.z<n>.kusto.fabric.microsoft.com`). A local emulator has no wildcard
DNS, so the cluster URI is path-prefixed on the emulator's own origin:

```text
{scheme}://{host}/kusto/{workspaceId}/{eventhouseId}
```

This is transparent to real clients: every Kusto SDK builds its endpoints by
appending `/v1/rest/…` or `/v2/rest/query` to the cluster URI. Microsoft's
`azure-kusto-data` drives this URI unmodified in `e2e/rti`. It is the same
accommodation OneLake makes with its account-prefixed path form.

## Running it

```bash
docker compose --profile rti \
  -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.rti.yml up
```

Two opt-ins, doing different jobs: `--profile rti` starts (and only then
pulls) the ~1.2 GB engine, and `-f docker-compose.rti.yml` hands the emulator
its URL (`FABRIC_KQL_URL`, or `--kql-url`). Without the profile a plain
`docker compose up` never touches the image; without the overlay the Kusto
routes 501.

## The witness

`e2e/rti` (CI job **`rti`**) is the oracle, and it asserts real execution, not
plumbing: create a table, ingest rows two documented ways, then query them back
and check the *values* — a `summarize avg(…) by …`, a datetime filter, and a
cross-database isolation check. Two independent client families run over the
same surface: raw REST, and Microsoft's own `azure-kusto-data` SDK (which
parses the v2 frame stream and so exercises a second dialect).

It also settles a question no fake engine can: **which column names KQL
actually refuses**. A schema declaration cannot name a column with a bare KQL
keyword — the engine answers `SYN0002` — and the Eventstream → Eventhouse drain
builds its `.create-merge table T (name:type, …)` from whatever fields the
events carry, so `kind` is an ordinary field name that would emit a command
real Kusto rejects. The emitter quotes every name (`['kind']`, Kusto's
documented remedy), and the witness probes the keyword list in both directions
against kustainer: bare refused, quoted accepted, ingested, and queried back.
A keyword the engine turns out to accept fails the job by name, so the list in
`internal/api/kql_test.go` cannot quietly drift into refusing KQL that runs.

That CI job is the **witness of record**: it runs on amd64 runners, and the
witness depends on `kustainer: service_healthy`, so an engine that never comes
up fails the job rather than passing quietly.

## Running it on Apple silicon

The default Docker setup on an M-series Mac translates amd64 with Rosetta,
which stops at SSE4.2 — so the engine crashes during boot. `e2e/rti/run.py`
detects this and explains it, rather than letting you discover it as a native
crash mid-build. Note it inspects the **Docker daemon's** architecture, not the
host's: with the VM below the two deliberately differ.

QEMU does implement AVX2, so a real x86-64 VM works. `--cpu-type max` is the
part that matters — QEMU's default CPU model omits AVX2 as well:

```bash
brew install colima qemu lima-additional-guestagents
colima start --profile fabric-x86 --arch x86_64 --vm-type qemu \
  --cpu-type max --memory 20 --cpus 6 --disk 60
export DOCKER_CONTEXT=colima-fabric-x86
python3 e2e/rti/run.py
```

`lima-additional-guestagents` is not optional: Lima 2.x ships only the host
architecture's guest agent, and without it the VM fails to start with
`guest agent binary could not be found for Linux-x86_64`.

Size the VM generously: at 8 GiB the engine is OOM-killed (exit 137) once the
rest of the stack is running.

Measured on an M4 Max (36 GB): the VM exposes `avx2`, the engine is ready in
**~40 s**, and the **full suite passes** with the same values CI asserts. The
slow parts are the ~3.4 GB image pull and the image builds — everything is
translated — not the engine; budget ~10 min for a cold run and far less once
cached. CI remains the witness of record.

## Boundaries (deliberate, not backlog)

- **Eventhouse streaming ingest / queued `Kusto.Ingest`** stays 🔴. The
  engine is a query/ingest engine; Fabric's streaming-ingest protocol and
  the `ingest-` endpoint are a different service, and the Kusto emulator
  explicitly has no streaming ingestion. **Eventstream → Eventhouse
  destination** is a separate green row: Custom HTTP produce drains through
  **direct** ingest (`.create-merge` + `.ingest inline`) against the already
  attached engine — the same path this sidecar already supports. That is
  produce-triggered, not a streaming pipeline.
  [51-eventstream-kafka.md](51-eventstream-kafka.md).
- **Queued ingestion / the `ingest-` endpoint and `Kusto.Ingest` SDKs**:
  unsupported by the engine. `ingestionServiceUri` therefore points at the
  engine, and direct ingestion commands are the supported path.
- **OneLake availability** (a KQL database mirroring itself into OneLake
  Delta) is not wired: the engine writes to its own storage, not ours.
