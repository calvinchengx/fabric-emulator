# 07 — Control-plane API

The surface is grounded in an endpoint-frequency scan of `fabric-docs`: the
handful of routes below are what SDKs, `fabric-cicd`, git integration, and
deployment-pipeline automation actually call. Typed item collections
(`/notebooks`, `/lakehouses`, `/warehouses`, `/dataPipelines`, …) are thin
aliases over the **generic item** shape, so one implementation covers dozens of
item types. The OneLake data plane has its own page: [08-onelake.md](08-onelake.md).

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

**RBAC fidelity map.** Fabric's permission model has four layers
(`security/permission-model.md`); the emulator covers them as follows:

| Layer | Emulated? |
|---|---|
| **Workspace roles** | ✅ Per the `roles-workspaces.md` matrix: workspace delete/rename + role management = Admin (Member may grant ≤ Member); item CRUD, definitions, git sync, job start/cancel = Contributor+; item/metadata reads + job status = Viewer+; **git connect and workspace-identity provisioning = Admin only** (both explicit matrix rows). |
| **OneLake API access (ReadAll)** | ✅ Contributor+ only — Viewers are denied on the data plane, as in the matrix. (Viewers read via the SQL endpoint's `ReadData`, which is compute and not modeled.) |
| **Item permissions** (per-item sharing: Read/ReadAll/ReadWrite/Reshare) | ❌ Not yet — grants exist only at workspace scope. Emulable later as an `itemAccess` store + checks that OR with workspace roles. |
| **OneLake security / data access roles** (`dataAccessRoles`, `DefaultReader`, folder-scoped) | ❌ Not yet — emulable later as folder-scope filters on the DFS surface. |
| **Compute permissions** (T-SQL GRANT/OLS/RLS, semantic-model DAX) | 🚫 Non-goal: requires real SQL/DAX engines (see [03-architecture.md](03-architecture.md) non-goals). |

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

## Connections (P1) and admin (later)

`GET/POST /v1/connections` lands in **P1** — git `connect` with a service
principal requires a `connectionId` (see git section above). `/admin/*` (tenant
settings, workspace listing) is added as demand warrants.

### Connection credentials (planned)

Real connections carry `credentialDetails` with a `credentialType`
(`copy-job-rest-api-capabilities.md` shows the wire shape): `Basic`,
`ServicePrincipal`, `WorkspaceIdentity`, `Key`, `SharedAccessSignature`,
`Anonymous`. The emulator currently stores connection details verbatim with no
credential model; the planned design:

- **Write-only secrets.** Credential material (`password`, `secret`, keys) is
  accepted on create/update and **never echoed back** — reads return
  `credentialType` and non-secret fields only, as real Fabric does.
- **`ServicePrincipal`**: `{ tenantId, servicePrincipalClientId, secret }` —
  optionally validated against entra-emulator at create (a real client-
  credentials probe = Fabric's "test connection"), so a wrong secret fails
  connection creation the way it does in production.
- **`WorkspaceIdentity`**: no credential material at all
  (`workspace-identity-authenticate.md` — "no need to manage keys, secrets,
  and certificates"); valid only when the owning workspace has a provisioned
  identity (composes with the P2 lifecycle; deprovisioning breaks the
  connection, as documented).
- **AKV references** (see roadmap): credential-by-reference — the connection
  stores a pointer to an azure-keyvault-emulator secret and resolves it at
  use, reproducing Fabric's Azure Key Vault references feature offline.
- Identity material itself (app registrations, SP secrets) stays in
  entra-emulator — connections *reference* principals, never own them.

## Scope note

fabric-docs samples overwhelmingly acquire tokens with scope
`https://analysis.windows.net/powerbi/api/.default` (the legacy Power BI
first-party resource), not `https://api.fabric.microsoft.com/.default`. The
emulator accepts **both** audiences — matching what entra-emulator already
mints for either resource form.
