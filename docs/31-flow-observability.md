# Flow observability: watching data move through the emulator

**Status: built.** All four steps — the event bus, every event kind, the SSE
endpoint, attribution, and the portal **Data flow** view.

Running the medallion example today is a black box. It prints step numbers, and
when something fails you reconstruct what happened afterwards from
`queryactivityruns` — which is why `examples/medallion/common.py` grew
hand-rolled activity-level failure reporting. You cannot watch the data move,
and you cannot see a failure at the moment it happens.

That is a tooling gap, not a missing capability. Every fact needed is already
recorded; none of it is *streamed*, and the pieces are not joined.

## The one thing that makes this cheap

The emulator owns its storage layer, so **every byte that moves through OneLake
passes through one function**. [`internal/store/onelake.go`](../internal/store/onelake.go)
already emits a `FileEvent` after every committed write, rename and delete —
added for event triggers ([`internal/api/triggers.go`](../internal/api/triggers.go)),
but the choke point is general:

```go
type FileEvent struct {
	Type        string // Microsoft.Fabric.OneLake.File{Created,Deleted,Renamed}
	WorkspaceID string
	ItemID      string
	RelPath     string
}
```

No writer can bypass it — not an ADLS client, azcopy, delta-rs, Sail, a Copy
activity, or the mirror writer. A real Fabric would need an Eventstream and a
broker to get this; we get it from owning the storage layer.

What was missing is only that `Store.FileEvents` was a **single** `func` field
with one subscriber. Everything below follows from making it a fan-out.

Two contracts, deliberately kept separate: `FileEvents` stays **synchronous and
exactly-once** (a Reflex must have fired before the write returns), while the
bus is **asynchronous and lossy** (a watching developer must never be able to
slow a writer down).

## Design

### 1. An event bus in the store

```go
// Subscribe returns a channel of events and a cancel func.
func (s *Store) Subscribe() (<-chan Event, func())
```

A channel rather than a callback, in the end: the SSE handler already selects
over the request context and a keepalive ticker, and a callback would have
needed a channel behind it anyway.

`FileEvents` becomes the bus's first publisher rather than the only consumer.

**The critical constraint.** The emit is synchronous inside the write path, and
the store runs `SetMaxOpenConns(1)`. A subscriber that blocks stalls OneLake
writes for every caller. So:

- each subscriber gets a **buffered channel** (256) and its own goroutine;
- a full buffer **drops the event and increments a counter**, it never blocks;
- the drop count is reported on the stream, so a consumer knows it missed
  something rather than silently seeing a gap.

Slow consumers degrade themselves, never the emulator. This is the one place a
mistake here would be expensive, so it is stated first.

### 2. Event kinds

One envelope, four kinds. `seq` is a monotonic per-process counter so a client
can detect gaps; `at` is **emulator** time (the controllable clock), because
every other timestamp in the system is.

```json
{ "seq": 1041, "at": 1754049600, "kind": "…", "…": … }
```

| kind | emitted when | carries |
|---|---|---|
| `file` | a OneLake path is written / renamed / deleted | `eventType`, `workspaceId`, `itemId`, `path`, `attribution` |
| `table` | a Delta commit lands (see below) | `itemId`, `table`, `version`, `rowsAdded`, `filesAdded`, `filesRemoved`, `attribution` |
| `activity` | a pipeline activity starts or finishes | `jobId`, `activityName`, `activityType`, `status`, `error`, `durationInSeconds`, `retryAttempt` |
| `job` | a job instance starts or reaches a terminal state | `workspaceId`, `itemId`, `jobId`, `jobType`, `invokeType`, `status`, `failureReason` |

Existing shapes are reused verbatim where they exist — `activity` is
`pipeline.ActivityRun` plus the job id, `job` is the `jobBody` fields. Nothing
new to learn, and no second source of truth for a status.

**When an activity is announced.** Not when it is recorded. The interpreter's
retry loop *discards* failed attempts and back-patches the survivor's
`retryAttempt`, `durationInSeconds` and — on a timeout — its status, so
announcing at record time would stream outcomes that never appear in the run,
and stream them before they were final. Instead the interpreter flushes at
points where its records are settled: after `runWithPolicy` returns, and after
the skip / unresolvable-dependency records that bypass it. The stream and
`queryactivityruns` therefore always agree, which is the design's rule.

**When a job is announced.** `Started` always. A terminal event only where one
genuinely exists: a DataPipeline (which runs inline), a Dataflow (which fails
loudly), a notebook / Spark job / Airflow DAG at the moment its engine reports
back. A **generic item's status is derived from the clock** and never has such
a moment, so nothing further is claimed for it. The stream says what happened
and stays quiet where nothing did.

### 3. Delta commits are what make the stream legible

Raw file events are a firehose: one table write is dozens of Parquet parts plus
a log entry. Unfiltered, that is noise, not a visualization.

But a write to `Tables/<name>/_delta_log/…0004.json` **is** a table-version
event. The commit's own `add`/`remove` actions (already parsed by
[`internal/warehouse/delta.go`](../internal/warehouse/delta.go)) say what
changed. So the bus watches for `_delta_log` writes and derives a `table` event:

```
bronze_customers → v4  (+1,203 rows, 1 file added, 0 removed)
```

`TableRoot()` in [`internal/onelake/observe.go`](../internal/onelake/observe.go)
already collapses part-file paths to the table root, so the grouping exists.

A consumer can therefore watch `table` events alone and see the medallion move,
or drop to `file` events when debugging what a writer actually did. **Both are
published; the client chooses.** A `?kinds=table,activity,job` filter keeps the
common case quiet.

### 4. Attribution: which activity moved these bytes

A `FileEvent` says *what* moved, not *who* moved it. Two mechanisms already
answer that, and this design unifies them rather than inventing a third:

- **Notebook / Spark writes** — `x-ms-fabric-job-id` / `x-ms-fabric-cell-index`
  headers, or the same values as bearer `extraClaims` for engines built on Rust
  `object_store` (delta-rs, Sail) which cannot set request headers.
  `observe()` in [`internal/onelake/observe.go`](../internal/onelake/observe.go)
  already computes this and throws it away after recording the access.
- **Copy activity** — the executor knows its own `jobID` and the activity's
  `Name` at the moment it writes ([`internal/api/pipelines.go`](../internal/api/pipelines.go)),
  and already records both on the lineage edge.

So:

```go
type Attribution struct {
	JobID        string
	ActivityName string // Copy and other pipeline activities
	CellIndex    *int   // notebook cells
}
```

`CellIndex` ended up a **pointer**, not an int with a sentinel: cell 0 is a
real cell, and a plain int cannot tell "the first cell" from "not a cell at
all". Same reason `Version` is a pointer on a table event.

**How it reaches the write, without plumbing `context` through everything.**
`CreateOneLakePath` is called from the DFS and Blob handlers, git, deployment,
the mirror, and the Delta writer. Threading a context through all of them to
serve two callers would be a large diff for a small gain. Instead:

- `CreateOneLakePath` keeps its signature and emits an **unattributed** event;
- a sibling `CreateOneLakePathAs(attr Attribution, …)` emits an attributed one;
- `warehouse.WriteDeltaTableAs` is the same pattern one level up, so the Delta
  writer's *two* writes — the Parquet part and the `_delta_log` commit — both
  carry it, and the derived `table` event inherits it from the commit;
- the OneLake handlers pass `attributionOf(r)`, which is the value `observe()`
  was already computing and discarding.

Sibling functions rather than a parameter, so the ten existing callers and
their tests stay untouched; only the sites that know something change.

Explicit, no globals, no goroutine-local trickery, and every other caller is
untouched. **Attribution is never inferred** — same rule the lineage design
already holds to: an engine reports it, or the data plane observes it, or the
field is empty.

Note what this does *not* change: `lineage_edges` remains the authoritative
record of a source→target movement. Attribution on a file event is a live
debugging aid; the lineage edge is the durable fact.

### 4a. When a file event fires — a bug this work surfaced

Building the stream exposed a real defect in the *already-shipped* event
triggers. The ADLS protocol writes in three steps — create the path, append the
bytes, flush — and the event was firing on the **create**, while the file was
still empty. A Reflex therefore triggered before its data existed, and a derived
table event would have reported an empty commit.

Azure's own ADLS Gen2 raises `BlobCreated` on `FlushWithClose`, not on the
create, and the DFS handler's own comment already said flush "is the point the
DFS protocol considers the file written". So:

- a zero-length non-directory create raises **nothing** — mid-sequence it is
  indistinguishable from an empty file, and the store cannot tell them apart;
- the flush handler calls `Store.EmitFileWritten`, because only the protocol
  layer knows a staged write is complete.

Exactly one event per file, carrying data that is actually there.

### 5. The stream: `GET /_emulator/events`

Server-Sent Events, not a WebSocket:

- one-way is all this needs;
- `curl -N` tails it with no client at all — which is the single biggest
  quality-of-life win here, and it needs no portal work;
- no new dependency, ~40 lines of Go;
- browsers reconnect automatically via `EventSource`.

```
curl -N https://localhost:9443/_emulator/events?kinds=table,activity,job
```

Real output, captured from the test suite — an ADLS upload followed by a Delta
commit:

```
event: file
data: {"seq":1,"at":1785591720,"kind":"file","workspaceId":"3cfca386…","itemId":"bf775fce…",
       "eventType":"Microsoft.Fabric.OneLake.FileCreated","path":"Files/landing/customers.csv"}

event: file
data: {"seq":2,…,"path":"Tables/bronze_customers/_delta_log/00000000000000000000.json"}

event: table
data: {"seq":3,…,"table":"Tables/bronze_customers","version":0,"rowsAdded":1203,"filesAdded":1}
```

`version` is deliberately a pointer in Go so a table's *first* commit — version
0, the common case for a fresh medallion — is still reported, while file events
carry no meaningless `"version": 0`.

It sits under `/_emulator`, the existing testing-lever namespace (clock, faults,
portal) — deliberately **not** part of the Fabric contract, because real Fabric
has no such endpoint. It is emulator-only, and `docs/parity.md` records it in
the emulator-only table alongside the clock and the fault injector.

**Ring buffer.** The bus keeps the last 1,000 events, and `?since=<seq>` replays
from there. A client that connects after the run started still sees it, and the
portal survives a reload mid-medallion.

### 6. The portal `Flow` view

A new entry under *Data plane* in [`portal/src/App.svelte`](../portal/src/App.svelte),
which already has the section structure:

- **A live log** — the event stream, filterable by kind and workspace. Failures
  in red, with the activity error inline. This alone replaces reading container
  logs.
- **A flow graph** — nodes are items and tables, edges come from
  `lineage_edges` (which already carry `producer`, so a Copy edge and a notebook
  edge are visually distinct). Nodes pulse as `table` events land on them and go
  red when an activity targeting them fails. On the medallion this draws itself:
  landing → bronze → silver → gold.
- **A table inspector** — click a node, see the current Delta version, schema,
  and row count via the existing warehouse reader.

Polling would have worked for the log. It would not have worked for the graph:
the point is watching it happen.

Nodes are laid out in columns by distance from a source — computed by
relaxation rather than a topological sort, so a cyclic graph still renders
instead of hanging — which makes a medallion draw itself:
landing → bronze → silver → gold. Copy edges are solid, notebook edges dashed,
straight from `producer`.

The graph reads `/_emulator/portal/lineage`, a tenant-wide listing added for
this view: the API-facing `/v1/workspaces/{id}/lineage` is workspace-scoped
because it sits behind RBAC, and the portal has no principal.

## What this deliberately does not do

**It does not pack every emulator into one UI.** entra, Key Vault, Sail, SQL
Server, Airflow, MLflow, Kusto and OpenMetadata are separate containers; several
have their own UI and rebuilding them is a large surface that rots on every
sidecar version bump. Health badges and outbound links, not embedded frames.

The fabric-emulator portal is the right hub for a different reason: it is the
one component that **sees everything** — every OneLake byte, every job, every
validated token. Sail's writes already appear here, attributed, because of the
bearer-claims path. That is a genuine cross-emulator view that costs no proxying.

**It does not add a background worker.** The bus is passive: it publishes what
callers already do. Nothing polls, nothing ticks. The emulator's determinism
guarantee is untouched.

**It does not become a second source of truth.** Every event is a projection of
state already persisted (`job_instances`, `pipeline_runs`, `lineage_edges`,
`onelake_paths`). If the stream and the API ever disagree, the API is right.

## Build order

1. ~~**Bus + `file`/`table` events + SSE endpoint.**~~ **Done.** Useful on its
   own: `curl -N` while the medallion runs. Also emitted Delta `stats`
   (`numRecords`) from our own writer, which every real Delta writer already
   does and without which our own commits could not report row counts.
2. ~~**`activity`/`job` events.**~~ **Done.** Failures arrive as they happen
   instead of being reconstructed afterwards — with the activity name, its
   error, and the job id that correlates them.
3. ~~**Attribution** (§4).~~ **Done.** A Copy activity's writes name the
   activity; a notebook cell's writes name the cell — including cell 0, and
   including engines that cannot set headers, via the bearer claims that were
   already there.
4. ~~**Portal `Flow` view.**~~ **Done.**

Each step is independently useful, and each stops at a point where the tree is
green.

## Testing

The bus is pure Go with no clock dependency, so it unit-tests directly:
subscribe, write a file through the store, assert the event. The drop-on-full
path is the one that matters most and is the easiest to get wrong — a
deliberately blocked subscriber must not stall a write, and must report a
non-zero drop count.

The SSE endpoint gets a server-level test that runs a real pipeline and asserts
the expected event sequence arrives on the wire, in order, with `seq` gaps
absent. The medallion e2e gains an assertion that the run produced `table`
events for `bronze_customers` and the gold table — which also makes the example
a witness that the stream reflects real work.

Per the repo rule, the `docs/parity.md` row and its `docs/witnesses.json` entry
land in the **same commit** as the claim.
