# 02 — Emulated API surface

The surface is grounded in an endpoint-frequency scan of `fabric-docs`: the
handful of routes below are what SDKs, `fabric-cicd`, git integration, and
deployment-pipeline automation actually call. Typed item collections
(`/notebooks`, `/lakehouses`, `/warehouses`, `/dataPipelines`, …) are thin
aliases over the **generic item** shape, so one implementation covers dozens of
item types.

All routes are under `https://api.fabric.microsoft.com/v1` unless noted.
`application/json`. Bearer required. Mutations are async (see **LRO** below)
unless marked *sync*.

## Core — workspaces

| Method + path | Notes |
|---|---|
| `GET /workspaces` | list; continuation-token pagination (`?roles=` filter is REST-reference-only, not shown in fabric-docs) *sync* |
| `POST /workspaces` | create → 201 `{ id, displayName, capacityId }` |
| `GET /workspaces/{id}` | get *sync* |
| `PATCH /workspaces/{id}` | rename / describe |
| `DELETE /workspaces/{id}` | delete (cascades items + role assignments) |
| `POST /workspaces/{id}/assignToCapacity` | `{ capacityId }` → 202; see the capacity model below |
| `POST /workspaces/{id}/unassignFromCapacity` | detach → 202 |

## Capacities (the model behind assignToCapacity)

Wire shapes are REST-reference-only (`/rest/api/fabric/core/capacities`;
fabric-docs covers capacity portal-side). The emulator does not model SKUs,
billing, or throttling — a capacity is an **assignable object**, nothing more.
It exists because real tooling checks it: fabric-cicd refuses to publish into
a workspace whose `capacityId` is empty.

| Method + path | Notes |
|---|---|
| `GET /v1/capacities` | list capacities the caller can see *sync* |

- **Seed:** every instance boots with one deterministic capacity —
  `{ id: <fixed GUID>, displayName: "Emulator Capacity", sku: "F64", region: "local", state: "Active" }`.
- **Default assignment:** `POST /workspaces` with no `capacityId` auto-assigns
  the seeded capacity (mirrors a tenant whose workspaces land on a trial/default
  capacity, and keeps fabric-cicd working out of the box). Pass an explicit
  `capacityId` to override; an unknown id is a 404 `CapacityNotFound`.
- `assignToCapacity` / `unassignFromCapacity` are Admin-only 202 LROs (no
  result), setting/clearing `workspace.capacityId`.

## Core — RBAC (the decision Entra does not make)

| Method + path | Notes |
|---|---|
| `GET /workspaces/{id}/roleAssignments` | list *sync* |
| `POST /workspaces/{id}/roleAssignments` | `{ principal:{id,type}, role }` |
| `PATCH /workspaces/{id}/roleAssignments/{raId}` | change role |
| `DELETE /workspaces/{id}/roleAssignments/{raId}` | revoke |

Roles: `Admin` \| `Member` \| `Contributor` \| `Viewer`. Enforcement maps the
caller's token `oid`/`appid` → role → allowed operations. A missing/insufficient
role yields Fabric-shaped `401`/`403`.

## Core — items (generic; typed aliases reuse this)

| Method + path | Notes |
|---|---|
| `GET /workspaces/{id}/items` | list; `?type=` filter *sync* |
| `POST /workspaces/{id}/items` | create `{ displayName, type, definition? }` |
| `GET /workspaces/{id}/items/{itemId}` | get *sync* |
| `PATCH /workspaces/{id}/items/{itemId}` | rename / describe |
| `DELETE /workspaces/{id}/items/{itemId}` | delete |
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

## Jobs (trigger + state, no real execution)

| Method + path | Notes |
|---|---|
| `POST /workspaces/{id}/items/{itemId}/jobs/instances?jobType=…` | schedule → operation |
| `GET  /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}` | status *sync* |
| `POST /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/cancel` | cancel |

Jobs transition `NotStarted → InProgress → Completed/Failed` on the controllable
clock (the REST reference also defines `Cancelled` and `Deduped`; add them with
the cancel path). Nothing actually computes.

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
from `GET/POST /v1/connections` — so **connections are on the P1 critical
path**, not a later add.

The emulator's "git remote" is a local store of item definitions per branch — no
real GitHub/AzDO needed for the happy path (a real provider can be wired later).

## Long-running operations

| Method + path | Notes |
|---|---|
| `GET /operations/{id}` | `{ status: NotStarted\|Running\|Succeeded\|Failed, … }` *sync* |
| `GET /operations/{id}/result` | terminal payload when `Succeeded` (REST-reference-only; fabric-docs scripts poll `/operations/{id}` and read `Location` for the result) |

Async mutations respond `202` with **both** an `x-ms-operation-id` header (what
the documented automation scripts actually read) and `Location:
/v1/operations/{id}`, plus `Retry-After`. Clients poll while status ∈
{`NotStarted`, `Running`}.

## OneLake data plane (P3) — `onelake.dfs.fabric.microsoft.com`

An **ADLS-Gen2 / Blob** subset (DFS endpoint), `Storage`-audience token. The
**filesystem is the workspace** (account name is always `onelake`), so listing
happens at the workspace level:

- `PUT  /{workspace}/{item}.{type}/{path}?resource=file|directory` — create
- `PATCH …?action=append` + `?action=flush` — write
- `GET  /{workspace}/{item}.{type}/{path}` — read
- `GET  /{workspace}?resource=filesystem&recursive=false[&directory={item}.{type}/Files]` — list
- `DELETE …`

**Managed-folder rules** (`onelake-api-parity.md` — core fidelity, not
optional): ADLS/Blob APIs can **never create, rename, or delete workspaces or
items** — only `HEAD` is allowed at the workspace (container) and tenant
(account) level. An item's top-level folder (`/MyLakehouse.lakehouse`) and its
first level (`/Files`, `/Tables`) are Fabric-managed: protected from
create/delete/rename; full CRUD only *within* them. Disallowed query parameters
(e.g. `action=setAccessControl`) reject the request; disallowed headers (e.g.
`x-ms-owner`) are ignored and echoed back in `x-ms-rejected-headers`.
Permission response headers are canned: `x-ms-owner`/`x-ms-group` =
`$superuser`, `x-ms-permissions` = `---------`.

Enough for shortcut / trusted-workspace-access smoke tests. GUID and
name-addressing both resolve to the same item.

## OneLake shortcuts (planned)

Wire shapes are REST-reference-only (`/rest/api/fabric/core/onelake-shortcuts`;
fabric-docs covers shortcut creation portal-side). A shortcut is a **symlink in
OneLake**: a named entry inside an item's managed folders whose reads resolve
into another location. Scope: **OneLake-to-OneLake targets only** — external
targets (ADLS Gen2, S3, Dataverse, …) need real cloud credentials, which is
exactly what an offline emulator cannot honor; they 501 with a clear message.

| Method + path | Notes |
|---|---|
| `POST /v1/workspaces/{wid}/items/{iid}/shortcuts` | create → 201 |
| `GET  /v1/workspaces/{wid}/items/{iid}/shortcuts` | list *sync* |
| `GET  /v1/workspaces/{wid}/items/{iid}/shortcuts/{path}/{name}` | get *sync* |
| `DELETE /v1/workspaces/{wid}/items/{iid}/shortcuts/{path}/{name}` | delete (removes the link, never the target) |

**Create body** (the OneLake target):

```json
{
  "path": "Files",
  "name": "linked-data",
  "target": { "oneLake": { "workspaceId": "…", "itemId": "…", "path": "Files/raw" } }
}
```

- **Data-plane resolution:** on the DFS surface, `/{ws}/{item}/Files/linked-data/…`
  resolves reads/lists through to the target item's `Files/raw/…`. Writes
  through shortcuts follow the target's RBAC (the caller needs a role on the
  *target* workspace — this is the trusted-workspace-access smoke path).
- **Listing:** shortcut entries appear in filesystem listings as directories
  with `isShortcut: true` metadata on the shortcut API (plain directories on
  the DFS listing, as in real OneLake).
- **Integrity:** deleting the target item leaves a dangling shortcut whose
  resolution 404s (matching real behavior); deleting the shortcut never
  touches target data. Cycles are rejected at create (`400 InvalidTarget`).
- **Storage:** a `shortcuts` table (`item_id, path, name, target_ws, target_item,
  target_path`) — no data is copied, resolution happens per request.

## Connections (P1) and admin (later)

`GET/POST /v1/connections` lands in **P1** — git `connect` with a service
principal requires a `connectionId` (see git section above). `/admin/*` (tenant
settings, workspace listing) is added as demand warrants.

## Scope note

fabric-docs samples overwhelmingly acquire tokens with scope
`https://analysis.windows.net/powerbi/api/.default` (the legacy Power BI
first-party resource), not `https://api.fabric.microsoft.com/.default`. The
emulator accepts **both** audiences — matching what entra-emulator already
mints for either resource form.
