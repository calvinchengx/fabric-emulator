# 54 — OneLake security

**Decision: build OneLake security into the emulator binary as an internal
package, not as a sidecar.** Policy is authored on the control plane, enforced
on the data plane, and served to engines through a documented API. All three of
those surfaces are already this binary. The engines that apply row and column
filters are already sidecars, and they will fetch policy over HTTP — which is
not a concession to our layout, it is the product's own architecture.

Grounded against Microsoft's docs pinned at `fabric-docs@0d63906a` (2026-07-10).
Two of the three endpoints below are marked **preview** there, which is a real
constraint on how much we should claim: see [Preview risk](#preview-risk).

## What OneLake security is

Not a rename of workspace RBAC. Workspace roles remain "the first security
boundary"; OneLake security is a second, finer one **inside** an item.

- **Deny by default.** "All users start with no access to data unless
  explicitly granted by a OneLake security role."
- **Scope is a path**: tables, folders, or schemas within one item.
- **Row and column filters live in the role**, not in the engine.
- **One definition, every engine.** "Any security set applies to access from
  all engines in Fabric."
- **Default roles**, most notably `DefaultReader`, whose membership is
  *virtualized*: computed from who holds `ReadAll`, not stored as a member list.

The last point is the one a naive implementation gets wrong. `DefaultReader` is
why a new item is readable at all, and it is a computation, not a row.

## Why this lives in this repository

Three options were open: an internal package here, a separate repository
consumed as a Go module, or a sidecar image. The first.

### Not a separate repository

Every repo in the family — `entra-emulator`, `azure-keyvault-emulator`,
`arm-emulator`, `azure-apim-emulator` — emulates **a distinct Azure service with
its own endpoints and its own release cadence**. That is the line, and OneLake
security is on the other side of it: it has no hostname of its own. Its two
endpoints live on `api.fabric.microsoft.com` and
`onelake.dfs.fabric.microsoft.com`, both of which are this binary.

It also has one consumer today, and no obvious second one:
`databricks-emulator` governs data through Unity Catalog and
`snowflake-emulator` through its own grants; neither can use a Fabric data
access role. A separate repo would cost the family's release ordering — release
the library, sweep the consumer, bump the BOM — for a dependency that never
leaves this tree.

That is a judgement about *today*, so the reuse option is kept open cheaply
rather than argued away: the evaluator lives in `pkg/`, not `internal/`, so a
future consumer imports a package instead of forcing a repository extraction.
See [the evaluator](#2-pkgonelakesec--a-pure-evaluator).

### Not a sidecar

The family runs sidecars for Sail, the Spark agent, SQL Server, Kustainer,
Airflow, Kafka, OpenMetadata and the sibling emulators. Every one of them is a
**real third-party engine or service we could not honestly reimplement in Go**.
That is the rule the compose files already follow.

OneLake security is not an engine. It is policy evaluation over stored rules —
squarely inside this repo's design bet that "contracts + storage + identity +
orchestration" are done for real, in process. Three further reasons:

1. **The endpoints are already ours.** `dataAccessRoles` is on
   `api.fabric.microsoft.com`; `securityPolicy/principalAccess` is on
   `onelake.dfs.fabric.microsoft.com`. Both hosts are this binary. A sidecar
   would have to be reached through a URL that does not exist in the product,
   or sit in front of us as a proxy.
2. **Enforcement is in the request path.** A `403` for an unauthorized read has
   to come from the DFS surface the client actually called. Delegating that
   per-request to another process buys nothing and adds a failure mode where
   the honest answer is "deny" but the observed answer is "connection refused".
3. **One static binary is a promise this repo makes.** A sidecar for a few
   hundred lines of rule evaluation spends that promise on nothing.

**Where the process boundary genuinely falls** is engine-side enforcement. Row
and column filters are applied by whoever runs the query, so the Spark agent —
already its own image — fetches effective access over HTTP and filters in its
own execution layer. That is exactly the **authorized engine model** Microsoft
documents for third parties, so the split follows the product rather than our
convenience.

## Shape

Three pieces. Only the middle one knows the rules.

### 1. `internal/store` — policy as data

    OneLakeRole{ ItemID, Name, Effect, DecisionRules[], Members, ETag }
    DecisionRule{ Permission[] }          // attributeName Path | Action
                                          // attributeValueIncludedIn[]

Keyed by item and versioned by ETag, because both endpoints trade in `If-Match`
and `If-None-Match`. Rows and columns hang off the rule.

### 2. `pkg/onelakesec` — a pure evaluator

    func Effective(roles []store.OneLakeRole, principal string,
                   memberships []string, input string) []AccessEntry

No HTTP, no disk, no store handle. Deny-by-default, consolidating across roles
into the "effective access" view the API is specified to return. `DefaultReader`
virtualization lives here, because it is a rule about how membership is
computed and belongs beside the other rules.

Being pure is what makes it testable without a stack, and what makes the two
consumers below provably consistent: they call one function.

**`pkg/`, not `internal/`, deliberately.** Go's `internal/` rule makes a package
unimportable by any other module, so putting the evaluator there would mean the
only way to ever reuse it is to extract a repository. It costs nothing to keep
the door open, and the rest of the layer — the store rows, the DFS surface —
stays internal where it belongs.

### 3. Two consumers, neither owning the rules

**The data plane.** [`internal/onelake/onelake.go`](../internal/onelake/onelake.go)
currently does one coarse check inside path resolution:

```go
role, err := s.Store.RoleOf(sc.TargetWorkspace, principalID)
...
return nil, &dfsError{"AuthorizationFailure", http.StatusForbidden, ...}
```

That is Fabric's *old* model, and its return type — resolved path, or 403 —
cannot express "this table minus these rows". It becomes a call into the
evaluator, still coarse: the DFS surface grants or refuses a path and never
filters content.

**The engine API.** A new `GET …/artifacts/{item}/securityPolicy/principalAccess`
serves the same evaluation to engines, filters included:

```json
{ "path": "Tables/dbo/Customers",
  "access": ["Read"],
  "rows": "SELECT * FROM [dbo].[Customers] WHERE [customerId] = '123'",
  "effect": "Permit" }
```

Note what `rows` is: **SQL text, not rows**. OneLake decides, the engine
applies. That single fact is why the layer can be decoupled at all, and it is
the contract the design has to preserve.

## Building directly on OneLake

A service that uses OneLake as its lake and OneLake security as its access
control, without Fabric's engines, is a **supported pattern in the product** and
must stay supported here.

> OneLake provides open access to all of your Fabric items through existing ADLS
> and Blob APIs and SDKs. You can access your data in OneLake through any API,
> SDK, or tool compatible with ADLS or Azure Blob Storage just by using a OneLake
> URI instead. — `onelake/onelake-access-api.md`

The authorized engine model extends the same freedom to compute: a third-party
engine reads the files itself and applies the filters this layer hands it. No
Fabric engine is in the path.

**What that pattern is not, in the product, is a separate deployment.** The same
page is equally clear:

> As OneLake is software as a service (SaaS), some operations, such as managing
> permissions or updating items, must be done through Fabric experiences, and
> can't be done via ADLS APIs.

and OneLake "exists across your entire Fabric tenant". A workspace is a Fabric
construct, an item is a Fabric construct, and a data access role is defined **on
a Fabric item** through the Fabric API. Reading is ADLS-compatible; authoring is
control-plane, always.

So the division of labour is:

| Concern | Where, in the product | Where, here |
|---|---|---|
| Read and write bytes | ADLS Gen2 / Blob APIs | the DFS + Blob surfaces |
| Fetch effective access | `securityPolicy/principalAccess` | same endpoint |
| Author roles | Fabric REST | `dataAccessRoles` on the control plane |

**We do not ship a standalone OneLake binary**, because that would emulate a
topology the product does not have. A consumer built against standalone-OneLake
would discover in a real tenant that role management needs the Fabric control
plane after all — the emulator leniency this family treats as worse than a gap,
because it destroys the signal rather than merely missing it.

The honest lever for weight is the topology that already exists:

    make up-lite     # contract-only: no Sail, no agent, no SQL Server

One image, serving the control plane, OneLake, and this layer. A service that
speaks only ADLS APIs plus `principalAccess` needs nothing else running, and
`ci:duckdb` (below) is the witness that it genuinely needs nothing else.

## Staging, and what each stage may claim

Each stage ends at an honest parity row. Stopping after any of them leaves a
true statement rather than a half-claim.

| Stage | Build | May claim |
|---|---|---|
| 1 | store + evaluator + Go tests | nothing; no surface changes |
| 2 | `dataAccessRoles` CRUD | authoring only, and the row says so |
| 3 | DFS enforcement | path-scoped read control |
| 4 | `principalAccess` | the authorized-engine contract |
| 5 | RLS/CLS in the Spark agent | engine-side filtering |

## Witnesses

Our rule is that a 🟢 needs a real-client witness in CI (doc 24). The tiers
differ sharply in what they can prove here, so they are named per stage.

| Stage | Witness | What it establishes |
|---|---|---|
| 2 | `ci:fabric-cli` — `fab api` PUT then GET | Microsoft's own CLI round-trips the documented payload |
| 3 | `ci:adls-sdk` — granted path 200, ungranted 403 | enforcement at the storage surface, unmodified Microsoft SDK |
| 3 | `ci:delta-rs` — permitted table reads, denied fails | table and folder scope against a real Delta reader |
| 4 | `ci:duckdb` — fetch policy, apply filters, read Parquet | the authorized engine model, end to end |
| 5 | `ci:sail` — Spark sees filtered rows and columns | engine-side enforcement on the default engine |
| all | `go:` tests | consolidation, deny-by-default, `DefaultReader`, precedence |

**The DuckDB witness is the one that matters most.** Every other witness tests
our own enforcement. That one tests whether a genuinely third-party engine can
perform the documented sequence — privileged read, fetch effective policy for a
user, filter in its own layer — against our emulator, unmodified. If it can,
the seam is real rather than asserted.

**Every witness needs a negative control.** A suite showing "the permitted user
reads the table" passes identically against an emulator with no security at all.
The load-bearing assertions are the refusals: the ungranted principal gets 403,
the RLS user sees *fewer* rows than the unrestricted one, the CLS user's
dataframe is *missing* the column. This is the discipline that made
`e2e/task-parameters` worth having — it asserted the leak was gone, not that the
happy path worked.

## Preview risk

`securityPolicy/principalAccess` and the external-engine integration are both
marked preview in the pinned docs (`ms.date: 01/12/2026`), and the response
carries `identityETag` and `metadataETag` fields that look likely to move.

Per this repo's rule about derived surfaces, the request and response shapes
should be **derived from vendored Microsoft sources and gated**, not transcribed
from a doc page. A preview contract transcribed by hand is a claim that ages
without anyone noticing.

## Boundaries, stated rather than discovered

- **`members.fabricItemMembers`** — roles can grant access to another *item*,
  not only to principals. Business events exclude it; lakehouses do not. Decide
  before stage 1: retrofitting a second member kind into the evaluator later
  invalidates the witnesses built on the first.
- **`ReadWrite`** — the docs define read and write permissions. Stages 2-4 cover
  `Read` only; write scoping is a later increment and the parity row must not
  imply otherwise.
- **DENY rules** — the model defines a `Type` of GRANT or DENY, and then says
  "only GRANT type roles are supported". We implement what the product does,
  and refuse DENY rather than accepting one we would silently ignore.
- **Metadata security** — `Read` is documented as equivalent to both
  `VIEW_DEFINITION` and `SELECT`. Hiding a table's *existence* from a user with
  no role on it is part of the contract, not a nicety, and needs its own
  assertion.
