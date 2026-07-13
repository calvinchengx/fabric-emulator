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
| `GET /workspaces` | list (full set — no continuation-token pagination yet; `?roles=` filter is REST-reference-only, not shown in fabric-docs) *sync* |
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

## Jobs (trigger, state, and real execution)

| Method + path | Notes |
|---|---|
| `POST /workspaces/{id}/items/{itemId}/jobs/instances?jobType=…` | schedule → operation |
| `GET  /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}` | status *sync* |
| `POST /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/cancel` | cancel |
| `POST /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/queryactivityruns` | DataPipeline: the recorded activity runs *sync* |
| `GET  /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/notebookRun` | RunNotebook: parsed cells + run detail *sync* |
| `POST /workspaces/{id}/items/{itemId}/jobs/instances/{jobId}/notebookRunResult` | engine → service callback: report per-cell results, finalise status |

Jobs transition `NotStarted → InProgress → Completed/Failed` on the controllable
clock, and — for the two executing job types — actually do work at trigger:

- **DataPipeline** jobs run the pipeline interpreter now: the definition's
  control flow executes and the **activity runs are recorded** (queryable via
  `queryactivityruns`). A pipeline failure sets the job's terminal status,
  overriding fault injection.
- **RunNotebook** jobs parse the notebook into cells with the real Go parser and
  record a `Pending` run (`notebookRun`). A Spark runner executes the cells and
  posts back to `notebookRunResult`, which merges per-cell results and finalises
  the job's status from the real outcome.

`Cancelled` is implemented (the `cancel` path sets it); `Deduped` (from the REST
reference) is the only state not yet emulated.

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

Folders organize items within a workspace (nesting via `parentFolderId`); the
folder tree is a plain metadata store.

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
  statement-executor agent, so real Spark computes the results (unmodified
  `pylivy`/`sparkmagic` clients work).
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

Connections carry `credentialDetails` with a `credentialType`
(`copy-job-rest-api-capabilities.md` shows the wire shape), validated per type:
`Basic`, `ServicePrincipal`, `WorkspaceIdentity`, `AzureKeyVaultReference`,
`Key`, `SharedAccessSignature`, `Anonymous`.

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
- **`AzureKeyVaultReference`**: credential-by-reference — the connection stores a
  pointer to an Azure Key Vault secret, resolved at use with a vault-audience
  workspace-identity token (Fabric's Azure Key Vault references feature,
  offline).
- Identity material itself (app registrations, SP secrets) stays in
  entra-emulator — connections *reference* principals, never own them.

## Scope note

fabric-docs samples overwhelmingly acquire tokens with scope
`https://analysis.windows.net/powerbi/api/.default` (the legacy Power BI
first-party resource), not `https://api.fabric.microsoft.com/.default`. The
emulator accepts **both** audiences — matching what entra-emulator already
mints for either resource form.
