# 57 — Item permissions: sharing one item without the workspace

**Decision: model item permissions as a store the control plane writes and four
data surfaces read — through Power BI's documented dataset-users API for
semantic models, and an authenticated emulator-native surface for every other
item, because Fabric grants those only through the portal.**

Grounded against Microsoft Learn as read on 2026-09-13: *Share items in
Microsoft Fabric*, *Permission model*, *Lakehouse sharing and permission
management*, *Roles in workspaces in Microsoft Fabric*, *Integrate Direct Lake
security*, and the REST references for *Datasets — Get / Post / Put Dataset
User*, *Items — List Item Access Details (Admin)* and *Users — List Access
Entities (Admin)*.

## What Fabric does

Item permissions are "confined to a specific item and don't apply to other
items". They exist for two cases: collaborating with someone "who doesn't have a
role in the workspace", and granting "additional item level-permissions for
colleagues who already have a role".

| Permission | Wire name | Grants |
|---|---|---|
| Read | `Read` | open the item; connect to its Warehouse or SQL analytics endpoint |
| Read all with SQL analytics endpoint | `ReadData` (additional) | read data through T-SQL "without SQL policy" |
| Read all with Apache Spark | `ReadAll` (additional) | read through OneLake APIs and Spark |
| Share | `Reshare` | grant "up to the permissions that they have" |
| Build | `Explore` | build content on a semantic model |
| Execute | `Execute` | run or cancel the item |
| Edit | `Write` | edit the item |

Four rules the implementation must keep:

- **Read is always granted** when sharing.
- **Item permissions union with workspace roles.** Removing an item grant "isn't
  enough" to take away what a workspace role gives.
- **Who may share**: workspace Admin and Member by default; Contributor and
  Viewer "only if they're granted Share (reshare) permission", and then only up to
  what they hold.
- **Sharing grants no write.** "Lakehouse sharing does not provide write
  permissions", and the dataset API "can't be used to add or remove write
  permission".

### What a workspace role implies

Fabric items, from the *Roles in workspaces* table:

| | Read | ReadData | ReadAll | Write | Execute | Reshare |
|---|---|---|---|---|---|---|
| Viewer | ✅ | ✅ | | | | |
| Contributor | ✅ | ✅ | ✅ | ✅ | ✅ | |
| Member | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Admin | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |

Semantic models, from *Put Dataset User*'s limitations: Admin and Member inherit
`ReadWriteReshareExplore`, Contributor `ReadWriteExplore`, Viewer `Read`.

## How Fabric lets you grant, and what we build

**The Fabric Core REST API has no item-permissions operation.** The Items group
is create, get, update, delete, definitions, move, connections and relations.
For lakehouses, warehouses and every other Fabric item, sharing is the portal's
*Share* and *Manage permissions* dialogs, which call internal endpoints Microsoft
does not document.

**Semantic models are the exception.** Power BI documents
`GET`, `POST` and `PUT /v1.0/myorg/datasets/{datasetId}/users`, with a nine-value
`datasetUserAccessRight` enum from `None` to `ReadWriteReshareExplore`.

So there are three surfaces, and one we deliberately do not build:

| Surface | For | Contract |
|---|---|---|
| `GET/POST/PUT /v1.0/myorg/datasets/{id}/users` | semantic models | **Power BI's, documented** — its rules included |
| `GET/PUT/DELETE /v1/workspaces/{wid}/items/{iid}/_emulator/access[/{principalId}]` | every other item | **emulator-native**, authenticated |
| `GET /v1/admin/workspaces/{wid}/items/{iid}/users` | auditing any item | **Fabric's admin API, documented** |
| the portal's internal `metadata/access` calls | — | **not built**: undocumented and unstable |

**Why the native surface is under `/v1/workspaces` and not `/_emulator/`.**
`/_emulator/*` routes are unauthenticated control surfaces: the clock, fault
injection, the portal. A sharing endpoint there would let anyone grant themselves
`ReadAll`. The `_emulator` *segment* inside an authenticated path marks it as not
a Fabric contract and cannot collide with an API Microsoft adds later.

## The admin list is effective access, and that is an inference

Neither admin sample shows a role-inherited row. Three things point that way
anyway, so the list returns inherited access unioned with direct grants:

- *List Item Access Details* is described as returning users "and their workspace
  roles";
- its sibling *List Access Entities* returns the items a user "can access";
- *Put Dataset User* treats inherited access as a permission on the item, and
  *Get Dataset Users*' own sample lists an `App` at `ReadWriteReshareExplore` —
  the shape an owner's inherited access takes.

The parity row grades it as inferred, and it joins the `real-fabric` workflow's
list to measure.

## Staging, and what each stage may claim

| Stage | Build | May claim |
|---|---|---|
| 1 | `item_access` store; effective access = role-implied ∪ direct; the three surfaces | grants are stored, validated and reported; **nothing is enforced** |
| 2 | OneLake DFS and Blob honour `ReadAll`; `fabricItemMembers` matches on real item access and its `sourcePath` | a shared lakehouse is readable through OneLake; `DefaultReader` works |
| 3 | Direct Lake requires Read + ReadAll when an item has no OneLake roles; a grant reaches a source in another workspace; `executeQueries` requires Read + Build | the Direct Lake boundary docs/parity names is closed |
| 4 | TDS: `Read` connects, `ReadData` reads; database memberships are **synced**, not only added | a shared endpoint is queryable, and a revoked grant stops working |

### Stage 2 in detail

When an item **has no OneLake security roles**, OneLake access is `ReadAll`: a
Viewer or a principal with no workspace role who holds it reads the item, and
nobody else below Contributor does. When an item **has roles**, the roles govern:
"When OneLake security is on, Direct Lake on OneLake uses the current user … to
figure out OneLake security roles" — and `ReadAll` then matters through
`fabricItemMembers`, which is how `DefaultReader` admits its holders.

A principal with no workspace role reaches only the item it holds a grant on.
Workspace-level listing stays refused: a grant "confers nothing above" the item.

`fabricItemMembers.sourcePath` names the item whose permissions confer
membership, as `<workspaceId>/<itemId>`. The store ignored it, so a role naming
*another* item's `ReadAll` matched anyone holding `ReadAll` *here*. Membership now
counts only when `sourcePath` names the role's own item; a foreign path confers
nothing — fail closed, since resolving access on an arbitrary other item is a
separate question from this one.

### Stage 3 in detail

`executeQueries` states its own requirement: "The user must have dataset read
and build permissions." Build is `Explore`, and a workspace Viewer inherits only
`Read` on a semantic model — so a bare Viewer is refused, where the emulator used
to admit anyone with a workspace role. Contributor and above inherit `Explore`;
anyone else needs it granted through the dataset-users API.

Direct Lake reads its source through `store.OneLakeReadAccess`, the decision the
storage surface asks, so it cannot admit what DFS would refuse. That replaces the
looser rule the Direct Lake gate kept for an item with no roles — any workspace
role would do — with Fabric's: Read and ReadAll.

A reader with no role in the source workspace is told only that they cannot read
the source, never whether an item of that name exists there.

**Boundary: the XMLA endpoint keeps its workspace-role gate.** This increment
applies Build to the REST query path, whose reference states the requirement.
What XMLA read access requires was not checked here, so it is left as it was
rather than changed on an assumption.

### Stage 4 in detail

`EnsurePrincipal` only ever **adds** database role memberships. A Contributor
demoted to Viewer keeps `db_datawriter` today, and a revoked `ReadData` grant
would keep `db_datareader` the same way. Stage 4 syncs memberships to the rung
the caller holds now, which closes that pre-existing over-grant as well as the
new one. A principal holding only `Read` gets a **connect** rung: a database user
with no role, so SQL Server refuses a `SELECT` unless a T-SQL `GRANT` allows it —
exactly "connect … without" data.

## Witnesses

The shape doc 54 set: **two callers, one request, different answers**, with the
unrestricted caller asserted in the same run — plus, for every grant, the revoke
that takes it away again. A grant witnessed without its revoke proves storage,
not permission.

## Boundaries

- **UPNs are refused by name.** Power BI identifies a `User` by UPN; the emulator
  keys principals by Entra object id and has no directory to resolve a UPN
  against. An object id is accepted for every principal type.
- **Groups are stored and reported, never matched.** Group membership is not
  modelled, so a grant to a group admits nobody — fail closed, and stated.
- **Service principals cannot be granted through the dataset API**, as Power BI
  documents; the native surface accepts them, since Fabric items can be shared
  with service accounts through *Direct access*.
- **`Write` is never granted through either surface.** Sharing an item for editing
  exists in the portal for some item types; it is not modelled.
- **Propagation is immediate.** Fabric documents up to two hours for a revoke to
  take effect for a signed-in user; modelling a delay would make every test
  nondeterministic for no fidelity a caller can act on.
- **Item metadata** (`GET …/items/{id}`) for a principal with a grant but no
  workspace role is not opened up here: the control-plane item surface stays
  workspace-scoped.
