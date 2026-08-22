# 07 — Control-plane API

The surface is grounded in an endpoint-frequency scan of `fabric-docs`: the
handful of routes below are what SDKs, `fabric-cicd`, git integration, and
deployment-pipeline automation actually call. Typed item collections
(`/notebooks`, `/lakehouses`, `/warehouses`, `/dataPipelines`, …) are thin
aliases over the **generic item** shape, so one implementation covers dozens of
item types. Eventstream destination bind, Reflex triggers, and Fabric Core
MCP (`POST /v1/mcp/core`) are on this plane too. The OneLake data plane has
its own page: [08-onelake.md](08-onelake.md).

All routes are under `https://api.fabric.microsoft.com/v1` unless noted.
`application/json`. Bearer required. Mutations are async (see **LRO** below)
unless marked *sync*.

## Core — workspaces

| Method + path | Notes |
|---|---|
| `GET /workspaces` | list; opt-in `?maxPageSize=N` paginates with a `continuationToken` + `continuationUri` (omit it for the full set); `?roles=` filter is REST-reference-only, not shown in fabric-docs *sync* |
| `POST /workspaces` | create → 201 `{ id, displayName, capacityId }` |
| `GET /workspaces/{id}` | get *sync* — WorkspaceInfo: `capacityAssignmentProgress` (always `Completed`; assignment is not a poll here), `capacityRegion` when assigned, production-shaped `oneLakeEndpoints` |
| `PATCH /workspaces/{id}` | rename / describe |
| `DELETE /workspaces/{id}` | delete (cascades items + role assignments) |
| `POST /workspaces/{id}/assignToCapacity` | `{ capacityId }` → 202; see the capacity model below |
| `POST /workspaces/{id}/unassignFromCapacity` | detach → 202 |

## Capacities (the model behind assignToCapacity)

Wire shapes are REST-reference-only (`/rest/api/fabric/core/capacities`;
fabric-docs covers capacity portal-side). The emulator does not model SKUs or
billing. It does model **concurrent-job admission** ([36-capacity-job-queueing.md](36-capacity-job-queueing.md)):
each capacity has a ceiling (default 999, overridable so a test can set it to 1).
Manual submits against a full capacity are `430 CapacityNotAvailable` with
`Retry-After`; scheduled and event-triggered jobs enter `Queued` and are
admitted FIFO when a slot frees. Same-item jobs are not serialised. A capacity
is otherwise an **assignable object** — it exists because real tooling checks
it: fabric-cicd refuses to publish into a workspace whose `capacityId` is empty.

| Method + path | Notes |
|---|---|
| `GET /v1/capacities` | list capacities the caller can see *sync* |

- **Seed:** every instance boots with one deterministic capacity —
  `{ id: <fixed GUID>, displayName: "Emulator Capacity", sku: "F64", region: "West Europe", state: "Active" }`.
- **ARM consume (on by default in this repo's compose; opt-out with an
  explicitly empty value):** `FABRIC_ARM_URL` names an arm-emulator origin that
  serves `GET /_family/capacities`. Capacities created over
  `Microsoft.Fabric/capacities` then appear on this list under the Fabric REST
  GUID ARM assigned at create (ARM's public resource document does not carry
  that GUID; the family feed does). ARM rows come and go with the feed; the
  seeded default is never deleted. Empty `FABRIC_ARM_URL` is the standalone
  default — do not point compose at ARM until a released arm-emulator image
  carries this provider.
- **Default assignment:** `POST /workspaces` with no `capacityId` auto-assigns
  the seeded capacity (mirrors a tenant whose workspaces land on a trial/default
  capacity, and keeps fabric-cicd working out of the box). Pass an explicit
  `capacityId` to override; an unknown id is a 404 `CapacityNotFound`.
- `assignToCapacity` / `unassignFromCapacity` are Admin-only 202 LROs (no
  result), setting/clearing `workspace.capacityId`.

## Display-name uniqueness (409)

Real Fabric rejects duplicate names, and so does the emulator — every
name-addressed contract here depends on it (OneLake `name.Type` paths, git
logical ids, the [`FABRIC_TARGET` toggle](21-real-fabric-toggle.md)'s
name-based workspace resolution, catalog ingest).

| Scope | Rule | On conflict |
|---|---|---|
| Workspace `displayName` | unique **tenant-wide** | `409` `WorkspaceNameAlreadyExists` |
| Item `displayName` | unique **per (workspace, type)** — reusable across types | `409` `ItemDisplayNameAlreadyInUse` |

Both comparisons are case-insensitive, and both apply to renames as well as
creates (renaming an entity to its own name is a no-op, not a conflict).
Item names being reusable across types is deliberate and documented:
"you can reuse item names across multiple item types" — which is exactly why
OneLake addresses an item as `name.Type` (`onelake-access-api.md`).

Every error response also carries the code in an **`x-ms-public-api-error-code`
header** alongside the body's `errorCode`, because documented Fabric client
code branches on that header.

## Core — RBAC (the decision Entra does not make)

| Method + path | Notes |
|---|---|
| `GET /workspaces/{id}/roleAssignments` | list *sync* |
| `GET /workspaces/{id}/roleAssignments/{raId}` | get one *sync* |
| `POST /workspaces/{id}/roleAssignments` | `{ principal:{id,type}, role }` |
| `PATCH /workspaces/{id}/roleAssignments/{raId}` | change role |
| `DELETE /workspaces/{id}/roleAssignments/{raId}` | revoke |

Roles: `Admin` \| `Member` \| `Contributor` \| `Viewer`. Enforcement maps the
caller's token `oid`/`appid` → role → allowed operations. A missing/insufficient
role yields Fabric-shaped `401`/`403`.

**RBAC fidelity map.** Fabric's permission model has four layers
(`security/permission-model.md`); the emulator covers them as follows:

| Layer | Emulated? |
|---|---|
| **Workspace roles** | ✅ Per the `roles-workspaces.md` matrix: workspace delete/rename + role management = Admin (Member may grant ≤ Member); item CRUD, definitions, git sync, job start/cancel = Contributor+; item/metadata reads + job status = Viewer+; **git connect and workspace-identity provisioning = Admin only** (both explicit matrix rows). |
| **OneLake API access (ReadAll)** | ✅ Contributor+ only — Viewers are denied on the data plane, as in the matrix. (Viewers read via the SQL endpoint's `ReadData`, which is compute and not modeled.) |
| **Item permissions** (per-item sharing: Read/ReadAll/ReadWrite/Reshare) | ❌ Not yet — grants exist only at workspace scope. Emulable later as an `itemAccess` store + checks that OR with workspace roles. |
| **OneLake security / data access roles** (`dataAccessRoles`, `DefaultReader`, folder-scoped) | ⚠️ **Authoring + DFS read enforcement.** `GET`/`PUT …/items/{id}/dataAccessRoles` round-trip roles verbatim, PUT replaces the whole set as the reference specifies, and only Admin/Member may write — witnessed by Microsoft's own `fab` (`ci:fabric-cli`). The rules are evaluated by `pkg/onelakesec` (deny-by-default, both membership kinds, row and column narrowing), and the DFS surface enforces them **for Viewers**: a Viewer has no ReadAll and is refused by default, and a role grants specific paths — the product's one documented effect here, since Admin/Member/Contributor "override any OneLake security Read permissions" and are never narrowed. Read only; a granted Viewer still cannot write. Listing is **filtered**, not refused: an engine enumerates a table before reading it, and an ungranted table's name is withheld rather than shown. Witnessed by Microsoft's Azure Blob SDK (`ci:adls-sdk`) and by real delta-rs (`ci:delta-rs`), each asserting the refusal as well as the grant. **No engine applies row or column filters yet**, so RLS/CLS is defined and unenforced. Stages 4-5 of [54-onelake-security](54-onelake-security.md). |
| **Compute permissions** (T-SQL GRANT/OLS/RLS, semantic-model DAX) | 🚫 Non-goal: requires real SQL/DAX engines (see [03-architecture.md](03-architecture.md) non-goals). |

## Core — items (generic; typed aliases reuse this)

| Method + path | Notes |
|---|---|
| `GET /workspaces/{id}/items` | list; `?type=` filter *sync* |
| `POST /workspaces/{id}/items` | create `{ displayName, type, definition? }` |
| `GET /workspaces/{id}/items/{itemId}` | get *sync* |
| `PATCH /workspaces/{id}/items/{itemId}` | rename / describe |
| `DELETE /workspaces/{id}/items/{itemId}` | delete |
| `POST /workspaces/{id}/items/{itemId}/move` | `{ targetFolderId }` — empty is the workspace root *sync* |
| `POST /workspaces/{id}/items/bulkMove` | `{ items[], targetFolderId? }` — at most 50; all-or-nothing *sync* |
| `POST /workspaces/{id}/items/{itemId}/getDefinition` | returns `{ definition:{ parts:[…] } }` |
| `POST /workspaces/{id}/items/{itemId}/updateDefinition` | replaces parts |

**Item definition** (the CI/CD source format):

```json
{
  "definition": {
    "parts": [
      { "path": "notebook-content.py", "payload": "<base64>", "payloadType": "InlineBase64" },
      { "path": ".platform",          "payload": "<base64>", "payloadType": "InlineBase64" }
    ]
  }
}
```

Stored verbatim so `getDefinition` round-trips exactly what `updateDefinition` /
git wrote. This is what makes `fabric-cicd` and deployment pipelines testable.

## Jobs (trigger, state, and real execution)

| Method + path | Notes |
|---|---|
| `POST /workspaces/{id}/items/{itemId}/jobs/instances?jobType=…` | schedule → operation |
| `GET  /workspaces/{id}/items/{itemId}/jobs/instances` | **List Item Job Instances** — paged, newest first *sync* |
| `GET  /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}` | status *sync* |
| `POST /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/cancel` | cancel |
| `POST /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/queryactivityruns` | DataPipeline: the recorded activity runs *sync* |
| `GET  /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/notebookRun` | RunNotebook: parsed cells + run detail *sync* |
| `POST /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/notebookRunResult` | engine → service callback: report per-cell results, finalise status |
| `GET  /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/sparkJobRun` | SparkJobDefinition: source, arguments, binding, and Environment run contract |
| `POST /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/sparkJobRunResult` | engine callback: finalise Spark job output/status |
| `GET  /workspaces/{id}/lineage` | emulator extension: exact activity source/sink edges for governance ingestion |
| `POST /workspaces/{id}/lineage` | emulator extension: an engine reports its own read/write set for work with no job to hang it on — an interactive Spark session or a plain script. Body is a `step` name plus `moves`, each a real reads→writes group (a flat reads × writes cross product would invent derivations that never happened). Recorded as `producer: Reported` — a claim by the caller, distinct from what the emulator observed itself ([31-flow-observability.md](31-flow-observability.md)) |

Jobs transition `NotStarted → InProgress → Completed/Failed` on the controllable
clock (`Queued` while waiting for a capacity slot), and — for the executing job
types — actually do work at trigger. A Manual POST against a saturated capacity
is `430 CapacityNotAvailable` rather than a job instance.

- **DataPipeline** jobs run the pipeline interpreter now: the definition's
  control flow executes and the **activity runs are recorded** (queryable via
  `queryactivityruns`). A pipeline failure sets the job's terminal status,
  overriding fault injection.
- **RunNotebook** jobs parse the notebook into cells with the real Go parser and
  resolve default-lakehouse/Environment metadata into a `Pending` run. A Spark runner executes the cells and
  posts back to `notebookRunResult`, which merges per-cell results and finalises
  the job's status from the real outcome.
- **SparkJobDefinition** jobs parse V1 source/arguments/libraries, resolve the
  same compute binding, and use an independent Pending→Completed/Failed callback.

`Cancelled` is implemented (the `cancel` path sets it); `Deduped` (from the REST
reference) is the only state not yet emulated.

## Job scheduler (Fabric's own, per item)

| Method + path | Notes |
|---|---|
| `POST   /workspaces/{id}/items/{itemId}/jobs/{jobType}/schedules` | create → the `ItemSchedule` *sync* |
| `GET    /workspaces/{id}/items/{itemId}/jobs/{jobType}/schedules` | list, paged *sync* |
| `GET    /workspaces/{id}/items/{itemId}/jobs/{jobType}/schedules/{id}` | read one *sync* |
| `PATCH  /workspaces/{id}/items/{itemId}/jobs/{jobType}/schedules/{id}` | replace `enabled` + `configuration` *sync* |
| `DELETE /workspaces/{id}/items/{itemId}/jobs/{jobType}/schedules/{id}` | remove |

This is Fabric's **native** scheduler, distinct from the `ApacheAirflowJob`
item, which hands scheduling to a real Airflow sidecar (docs/14).

`configuration` is the documented `ScheduleConfig` union, discriminated on
`type`. All four members carry `startDateTime`, `endDateTime` and
`localTimeZoneId`:

| `type` | Fields |
|---|---|
| `Cron` | `interval` minutes, 1–5,270,400 |
| `Daily` | `times[]` as `hh:mm`, at most 100 |
| `Weekly` | `times[]` + `weekdays[]` (`Monday`…`Sunday`) |
| `Monthly` | `occurrence` (`DayOfMonth` with `dayOfMonth`, or `OrdinalWeekday` with `weekIndex` `First`…`Fifth` + `weekday`), `recurrence` 1–12 months, `times[]` |

An item accepts at most **20 schedules per job type**; the 21st is
`ScheduleExceedsLimit`. An invalid configuration is refused at write time
rather than stored to silently never fire.

`localTimeZoneId` takes a **Windows** zone id (`Pacific Standard Time`) as real
Fabric does, or an IANA name (`America/Los_Angeles`). Times are real local wall
times: a daily `09:00` stays 09:00 across a daylight-saving change rather than
drifting an hour. Ids outside the mapped set are rejected — a schedule that
fires at the wrong hour is worse than one that refuses to be created.

### How a schedule fires without a background worker

The emulator's defining property is a controllable clock; a goroutine ticking
on wall time would make a job's outcome depend on how long a test took. So
schedules are evaluated **on demand**, at every moment a caller could observe
the result:

- `POST /_emulator/clock` — the deterministic lever. The response reports
  `scheduledJobsStarted` and `queuedJobsAdmitted`, and `{"advance": 0}` is a
  plain "tick now".
- listing an item's job instances, or its schedules;
- creating or updating a schedule — which is what makes the documented *"if the
  start time is in the past, it will trigger a job instantly"* true, with no
  special case: the first window simply opens at `startDateTime`.

Each evaluation materialises every occurrence in the half-open window
(last fired, now], so nothing fires twice and nothing in between is missed.
Firing goes through the same path a manual run takes — **a scheduled
DataPipeline really executes the interpreter** — and only
`invokeType: "Scheduled"` distinguishes it from an on-demand run.

**One boundary, stated:** catch-up is capped at 100 occurrences per evaluation,
keeping the newest. Real Fabric never needs the cap because its clock advances
one second per second; here a caller can advance a year against a one-minute
Cron, and half a million job instances is not a useful answer.

## Eventstream destinations

| Method + path | Notes |
|---|---|
| `POST   /workspaces/{id}/eventstreams/{id}/destinations` | bind a destination *sync* |
| `GET    /workspaces/{id}/eventstreams/{id}/destinations` | list bindings *sync* |

**This binding surface is emulator-native.** Fabric's Eventstream topology
(sources → operators → destinations) is assembled in the portal and has no
public REST — the same situation as Reflex triggers. Bindings are persisted
on the Eventstream (`item_properties`) and also appear on
`properties.destinations` so a GET eventstream is not silent.

```json
{
  "type": "Lakehouse",
  "itemId": "<lakehouse-id>",
  "table": "clicks",
  "workspaceId": "<optional; defaults to the Eventstream workspace>"
}
```

`type` is `Lakehouse`, `Reflex`, or `Eventhouse`. For Lakehouse, `table`
is a single `Tables/<name>` segment (no slashes). For Eventhouse, `table`
is a Kusto identifier (`[A-Za-z_][A-Za-z0-9_]*`); optional `database` is
the child KQL Database display name (default: the eventhouse's own child).
After a successful Custom HTTP produce, operators run on the batch, then
a Lakehouse dest appends the (possibly filtered/aggregated) values as a
real Delta table; a Reflex dest fires triggers on that Reflex whose
`eventType` is `Microsoft.Fabric.Eventstream.EventReceived` and whose
`source.itemId` is the Eventstream — each event starts the action job with
`@pipeline()?.TriggerEvent?.Key` / `.Value`; an Eventhouse dest ingests
via `.create-merge` + `.ingest inline` (direct ingest, not Fabric
streaming ingest). Produce reports `produced` (Kafka count) and `drained`
(post-operator count).

```json
{
  "type": "Eventhouse",
  "itemId": "<eventhouse-id>",
  "table": "clicks",
  "database": "<optional KQL Database display name>",
  "workspaceId": "<optional; defaults to the Eventstream workspace>"
}
```

## Eventstream operators

| Method + path | Notes |
|---|---|
| `POST   /workspaces/{id}/eventstreams/{id}/operators` | bind an operator *sync* |
| `GET    /workspaces/{id}/eventstreams/{id}/operators` | list operators *sync* |

Same emulator-native surface as destinations — Fabric's topology has no
public REST. Bindings persist on the Eventstream and appear on
`properties.operators`.

```json
{"type": "Filter", "condition": {"field": "n", "op": "gte", "value": 3}}
```

```json
{"type": "GroupBy", "keys": ["src"], "aggregates": [{"fn": "count", "as": "n"}]}
```

```json
{"type": "Window", "kind": "tumbling", "duration": "1h", "on": "ts"}
```

Filter ops: `eq`, `ne`, `gt`, `gte`, `lt`, `lte`, `contains`, `exists`.
GroupBy aggregates: `count`, `sum`, `min`, `max`, `avg`. Window is
tumbling on this produce batch only (stamps `_window_start`); hopping and
sliding are refused. Join, Union, and Expand are refused — they need more
than one stream. Kafka / DefaultStream stays the raw source; destinations
see the operator output.

## Event triggers (Reflex / Data Activator)

| Method + path | Notes |
|---|---|
| `POST   /workspaces/{id}/reflexes/{reflexId}/triggers` | bind a trigger *sync* |
| `GET    /workspaces/{id}/reflexes/{reflexId}/triggers` | list, paged *sync* |
| `GET    /workspaces/{id}/reflexes/{reflexId}/triggers/{tid}` | read one *sync* |
| `PATCH  /workspaces/{id}/reflexes/{reflexId}/triggers/{tid}` | update (absent fields keep their value) |
| `DELETE /workspaces/{id}/reflexes/{reflexId}/triggers/{tid}` | unbind |

**This binding surface is emulator-native.** In Fabric, adding a Trigger to a
pipeline creates a Reflex fed by an Eventstream, assembled in the portal — there
is no public REST for it, so there is no contract here to be faithful to. What
*is* faithful is everything downstream of the binding: the filter, the
invocation, the `TriggerEvent` fields, and a real pipeline really running.

```json
{
  "displayName": "on-landing",
  "eventType": "Microsoft.Fabric.OneLake.FileCreated",
  "source": { "itemId": "<lakehouse-id>", "pathPrefix": "Files/landing" },
  "action": { "itemId": "<pipeline-id>", "jobType": "Pipeline" }
}
```

`eventType` is one of `Microsoft.Fabric.OneLake.FileCreated`, `…FileDeleted`,
`…FileRenamed`. `source` is the `subject` filter: which item's OneLake storage
to watch, and optionally which folder within it (empty watches the whole item).
`action` names the item job to start; its `workspaceId` defaults to the Reflex's
own. A trigger whose action does not resolve is refused at bind time rather
than discovered to be inert later.

### No broker, because none is needed

Every byte written to OneLake passes through this emulator's own storage layer,
so a file event is observable **at the source**, whoever wrote it — an ADLS
client, `azcopy`, delta-rs, a Copy activity, the mirror writer. That is what
makes the trigger a genuine emulation of Data Activator's OneLake source rather
than a stub that only notices writes made through one API.

A match starts a job with `invokeType: "EventTriggered"`, and the event is bound
into the run:

```
@pipeline()?.TriggerEvent?.FileName      orders.csv
@pipeline()?.TriggerEvent?.FolderPath    Files/landing
@pipeline()?.TriggerEvent?.Subject       Files/landing/orders.csv
@pipeline()?.TriggerEvent?.EventType     Microsoft.Fabric.OneLake.FileCreated
@pipeline()?.TriggerEvent?.WorkspaceId / .ItemId / .Source
```

Reading those is what the expression language's **safe navigation** (`?.`) is
for, and why Fabric's own samples are written that way: the *same* definition
must also run when started by hand, where there is no trigger event at all.
With `?.` the chain yields null; with a plain `.` it would fail. Plain `.` still
fails loudly on a missing member, so safe navigation is opt-in and a typo in an
ordinary expression is still an error.

**Chains and cycles.** Dispatch is synchronous and reentrant: a triggered
pipeline writes files, which emit events, which may fire further triggers —
that is how a bronze→silver→gold chain behaves, and it works. A *cycle* would
recurse forever, so a trigger already on the dispatch stack does not fire again;
any cycle is cut at its first repeat while genuine chains still run.

## Git integration (unlocks CI/CD testing)

| Method + path | Notes |
|---|---|
| `POST /workspaces/{id}/git/connect` | attach a git provider (body below) |
| `POST /workspaces/{id}/git/initializeConnection` | first-sync direction |
| `GET  /workspaces/{id}/git/status` | ahead/behind + per-item change list *sync* |
| `POST /workspaces/{id}/git/commitToGit` | push workspace → git (writes item definitions) |
| `POST /workspaces/{id}/git/updateFromGit` | pull git → workspace |
| `POST /workspaces/{id}/git/disconnect` | detach |
| `GET  /workspaces/{id}/git/myGitCredentials` | credential config *sync* |

**Connect body** (per `git-automation.md` — note it is *not* a flat org/repo
object, and the SP path requires a **connection**):

```json
{
  "gitProviderDetails": {
    "gitProviderType": "AzureDevOps",
    "organizationName": "…", "projectName": "…",
    "repositoryName": "…", "branchName": "…", "directoryName": "…"
  },
  "myGitCredentials": { "source": "ConfiguredConnection", "connectionId": "…" }
}
```

`myGitCredentials.source` is `Automatic` (SSO) or `ConfiguredConnection`;
service principals must use `ConfiguredConnection`, whose `connectionId` comes
from the shipped `GET/POST /v1/connections` (see Connections below).

The emulator's "git remote" is a local store of item definitions per branch — no
real GitHub/AzDO needed for the happy path (a real provider can be wired later).

## Folders (workspace item organization)

| Method + path | Notes |
|---|---|
| `GET  /workspaces/{id}/folders` | list *sync* |
| `POST /workspaces/{id}/folders` | create `{ displayName, parentFolderId? }` → 201 *sync* |
| `GET  /workspaces/{id}/folders/{folderId}` | get *sync* |
| `PATCH /workspaces/{id}/folders/{folderId}` | `{ displayName }` *sync* |
| `DELETE /workspaces/{id}/folders/{folderId}` | empty folders only; otherwise `FolderNotEmpty` *sync* |
| `POST /workspaces/{id}/folders/{folderId}/move` | `{ targetFolderId }` — empty is the workspace root; a cycle is `InfiniteFolderHierarchyLoop` *sync* |

Folders organize items within a workspace (nesting via `parentFolderId`); the
folder tree is a plain metadata store.

## Catalog Search

| Method + path | Notes |
|---|---|
| `POST /catalog/search` | `{ search, filter?, pageSize?, continuationToken? }` — items in workspaces the caller can see; Dashboard and Dataflow excluded *sync* |

`search` matches item display name, description, and workspace display name.
`filter` is `Type eq`/`ne` with `or`/`and` and parentheses, as the REST
reference documents. Results are `ItemCatalogEntry` objects
(`catalogEntryType: FabricItem`). This is metadata discovery only — it does
not grant data-plane access.

## Fabric Core MCP Server

| Method + path | Notes |
|---|---|
| `POST /mcp/core` | Streamable HTTP JSON-RPC (initialize, ping, tools/list, tools/call). Bearer is the same Fabric control-plane token as the REST surface. Tools wrap the handlers above, so RBAC and LRO are not a second implementation. `Mcp-Session-Id` is issued on initialize |
| `GET  /mcp/core` | 405 — no SSE stream |
| `DELETE /mcp/core` | end session → 204 |

Does not execute notebooks or write lakehouse tables (Microsoft's published
limitation). Distinct from the local `Fabric.Mcp.Server` VS Code package and
from pbix-mcp.

## Livy / Spark data plane

Fabric exposes Spark through the Apache Livy REST API at a **lakehouse-scoped**
endpoint. The emulator validates the bearer token and workspace RBAC (like every
`/v1` route — session/job submission needs Contributor, status reads Viewer),
then serves the Livy contract:

| Method + path | Notes |
|---|---|
| `{GET,POST,DELETE} /workspaces/{id}/lakehouses/{lid}/livyapi/versions/{ver}/{sessions\|batches}/…` | classic Livy sessions + batches |
| `POST /workspaces/{id}/lakehouses/{lid}/livyapi/versions/{ver}/highConcurrencySessions` | Fabric high-concurrency session (acquire) |
| `{GET,DELETE} …/highConcurrencySessions/{hcid}` | get / release an HC session |
| `{POST,GET} …/highConcurrencySessions/{sid}/repls/{replid}/statements[/{stid}]` | submit / poll HC statements |

Execution mode depends on how the server is launched:

- **`--spark-agent-url` set:** native Livy termination — the emulator implements
  the Livy session/statement contract itself and drives a Spark
  statement-executor agent. The default agent computes through Sail over Spark
  Connect; a JVM-backed agent can be supplied separately (unmodified
  `pylivy`/`sparkmagic` clients work at the protocol layer).
- **`--spark-livy-url` set:** the routes reverse-proxy to a real external Apache
  Livy backend.
- **Neither set:** the routes `501` honestly — no faked sessions.

## Long-running operations

| Method + path | Notes |
|---|---|
| `GET /operations/{id}` | `{ status: NotStarted\|Running\|Succeeded\|Failed, … }` *sync* |
| `GET /operations/{id}/result` | terminal payload when `Succeeded` (REST-reference-only; fabric-docs scripts poll `/operations/{id}` and read `Location` for the result) |

Async mutations respond `202` with **both** an `x-ms-operation-id` header (what
the documented automation scripts actually read) and `Location:
/v1/operations/{id}`, plus `Retry-After`. Clients poll while status ∈
{`NotStarted`, `Running`}.

## Connections (shipped) and admin (later)

| Method + path | Notes |
|---|---|
| `GET  /v1/connections` | list *sync* |
| `POST /v1/connections` | create |

`GET/POST /v1/connections` is shipped — git `connect` with a service principal
requires a `connectionId` (see git section above). `/admin/*` (tenant settings,
workspace listing) is added as demand warrants.

### Connection credentials

`connectionDetails` on **create** is `{type, creationMethod, parameters[]}`, and
all three are required — `path` belongs to the *read* shape and is composed from
the parameters. Creation methods and their parameter names come from
`GET /v1/connections/supportedConnectionTypes` (321 types on a measured tenant;
`WebForPipeline.Contents` takes `baseUrl`, `AzureKeyVault.Actions` takes
`accountName`, `Sql` takes `server` and `database`).

Connections carry `credentialDetails` with a `credentialType`, validated per
type. The enum is Fabric's own: `Anonymous`, `Basic`, `Key`, `KeyPair`,
`OAuth2`, `ServicePrincipal`, `SharedAccessSignature`, `Windows`,
`WindowsWithoutImpersonation`, `WorkspaceIdentity`.

- **Write-only secrets.** Credential material (`password`, `secret`, keys) is
  accepted on create/update and **never echoed back** — reads return
  `credentialType` and non-secret fields only, as real Fabric does.
- **`ServicePrincipal`**: `{ tenantId, servicePrincipalClientId, secret }`,
  probed against entra-emulator via a client-credentials validation at create
  (Fabric's "test connection"), so a wrong secret fails connection creation the
  way it does in production.
- **`WorkspaceIdentity`**: no credential material at all
  (`workspace-identity-authenticate.md` — "no need to manage keys, secrets,
  and certificates"); valid only when the owning workspace has a provisioned
  identity. Deprovisioning breaks the connection, as documented.
- **Vault-backed credentials** are the reference twin of an inline field, never a
  credentialType of their own: `keyReference` (Key), `passwordReference` (Basic),
  `tokenReference` (SharedAccessSignature) and
  `servicePrincipalSecretReference` (ServicePrincipal). Each is a
  `KeyVaultSecretReference` — `{connectionId, secretName, version}` — and
  `connectionId` names a **connection of type `AzureKeyVault`**, whose own
  credentials reach the vault. A field and its reference are alternatives;
  sending both is refused. The emulator resolves at create (Fabric's "test
  connection") with a vault-audience token, and stores only the pointer.

  > **This replaced an invented shape in v0.22.0.** The emulator used to accept
  > `credentialType: "AzureKeyVaultReference"` carrying a `vaultUri`. Measured
  > against a real tenant on 2026-08-11, no such credentialType exists — and
  > because a `vaultUri` requires nothing to exist, the old form looked valid
  > while addressing nothing. It is now rejected by name, with a message naming
  > the replacement.
- Identity material itself (app registrations, SP secrets) stays in
  entra-emulator — connections *reference* principals, never own them.

## Scope note

fabric-docs samples overwhelmingly acquire tokens with scope
`https://analysis.windows.net/powerbi/api/.default` (the legacy Power BI
first-party resource), not `https://api.fabric.microsoft.com/.default`. The
emulator accepts **both** audiences — matching what entra-emulator already
mints for either resource form.
