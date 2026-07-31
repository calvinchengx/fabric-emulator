# 25 — Real-Time Intelligence: a real KQL engine behind Eventhouse

**Status: shipped, BYO-engine (🟠).** Eventhouse / KQL Database execution is
real when Microsoft's own KQL engine container is attached, and an honest 501
when it is not. This is Tier 2, item 2 of
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
| **Platform** | **linux/amd64 only. Microsoft documents ARM as unsupported** — the engine's native layer needs AVX2, which Apple-silicon emulation does not expose, so the container crashes during boot on an M-series Mac (`libKusto.NativeInfra.so failed … Crash_FailSlow`). Verified on this repo's own hardware. CI's amd64 runners are therefore the only witness. |

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

Because the engine cannot run on arm64, that CI job is the only place this is
provable. Locally on Apple silicon the surface still answers — with a 501.

## Boundaries (deliberate, not backlog)

- **Eventstream** stays 🔴. The engine is a query/ingest engine; a streaming
  ingestion pipeline is a different service, and the Kusto emulator explicitly
  has no streaming ingestion.
- **Queued ingestion / the `ingest-` endpoint and `Kusto.Ingest` SDKs**:
  unsupported by the engine. `ingestionServiceUri` therefore points at the
  engine, and direct ingestion commands are the supported path.
- **OneLake availability** (a KQL database mirroring itself into OneLake
  Delta) is not wired: the engine writes to its own storage, not ours.
