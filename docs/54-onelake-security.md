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

## What the DuckDB witness is, and is not

It performs the documented authorized-engine flow end to end, and it is not
"DuckDB supports OneLake security". Three differences, stated so the witness is
not read as more than it is.

**DuckDB has no OneLake integration.** The suite is a harness that ACTS as an
authorized engine, with DuckDB as its compute layer. Nothing shipped by DuckDB
Labs calls `principalAccess`. The docs address "third-party engine developers",
and this is what one of them would write, not what their users get for free.

**The predicate dialect is the integrator's problem.** Real Fabric returns
T-SQL — `SELECT * FROM [dbo].[Customers] WHERE [customerId] = '123'` — and
bracketed identifiers are not DuckDB syntax. The witness authors its predicate
in the engine's own dialect, which keeps the test about the CONTRACT rather than
about a translator. A real integration needs that translator, and this emulator
does not provide one.

**The engine identity's own restrictions are not modelled.** The guide requires
the engine to have unrestricted Read, and says API calls return errors if RLS or
CLS applies to the engine identity itself. We do not enforce that: an engine
identity narrowed by a role would get an answer here where the product would
fail. A boundary, not a claim.

What the witness DOES establish is the part that matters for an emulator: the
sequence works against us unmodified — privileged read, fetch effective access
for a named end user, filter in the engine's own layer, return only permitted
rows — and the ungranted table never appears in the policy at all.

## Preview risk

`securityPolicy/principalAccess` and the external-engine integration are both
marked preview in the pinned docs (`ms.date: 01/12/2026`), and the response
carries `identityETag` and `metadataETag` fields that look likely to move.

Per this repo's rule about derived surfaces, the request and response shapes
should be **derived from vendored Microsoft sources and gated**, not transcribed
from a doc page. A preview contract transcribed by hand is a claim that ages
without anyone noticing.

## Boundaries, stated rather than discovered

- **`members.fabricItemMembers`** — **decided in stage 1: both member kinds are
  modelled, and this one is not optional.** It is the *virtual* membership the
  default roles rely on — "all users that have the necessary permissions to view
  data in the item (the ReadAll permission, for example) are included as members
  of this default role". An evaluator with only explicit Entra members cannot
  express `DefaultReader`, so a newly created item would be unreadable by
  everyone: not a simplification of the product, a different one. `Members`
  therefore carries `Entra` and `ItemAccess`, and membership is the union.
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

## Stage 5's enforcement, corrected by measurement

Stage 5 secured a session by reshaping its catalog: a narrowed table becomes a
temp view holding the filter, a denied one is removed. Two assumptions under
that were never measured, and `e2e/onelake-security-bypass` measured them.

**A temp view shadows the unqualified name only, and that was a live bypass.**
With the filter installed, `SELECT count(*) FROM sales` returned 2 rows of 3 and
one column of two, while `SELECT count(*) FROM default.sales` returned all 3
rows and both columns. The second spelling is not exotic: `catalog.register()`
deliberately registers every table into a schema *and* into `default` so
unqualified names resolve the way they do in a lakehouse-attached notebook, so
the convenience registration was the door. Enforcement now sweeps every
qualified registration of a secured table out of the session, leaving the view
as the only way to name it, and the livy e2e asserts both spellings are blocked.

This was a defect in a row already marked supported, not the documented
path-read gap. It is the difference between "the query language is filtered" and
"the data is filtered", and only measurement separated them.

**Which is sound only where the catalog is private.** Removing a registration
changes whatever catalog the session has. Measured: Sail gives each
`builder.create()` session its own, and the owner's session was untouched
throughout — same table, same 3 rows, still listed. `newSession()` on the JVM
overlay shares the catalog by contract, where the same sweep would take the
table away from everyone. `catalog.CATALOG_IS_PRIVATE` records which route
gives which, and `onelake_security.apply()` **refuses** rather than reshaping a
shared catalog. The JVM entry is the conservative reading and is not measured
here: being wrong about it costs a refusal, never a leak.

**The shared-metastore worry was unfounded.** The suspicion that a viewer's
`DROP TABLE` unregisters a table for the owner was measured false on Sail: owner
count unchanged, table still listed, Delta files intact. It remains the reason
the refusal above exists for engines that do share.

**Re-application has to start from the table, not from last statement's view.**
`apply()` runs per statement, and the sweep removes what the view was built
from, so the second statement rebuilds the filter over an already-filtered
relation — which fails outright once CLS has removed a column the row filter
names. The livy e2e caught exactly that: statement one filtered, statement two
returned "Table not found". `restore()` re-registers from the recorded location
before re-securing, **unqualified**, so it lands in the current database that
the filter's own SQL resolves against. Re-registering into `default` is the
plausible-looking version that does not work, because the agent sets the
current database to the lakehouse schema.

### Still open: direct path reads

`spark.read.format("delta").load("abfss://…")` still returns unfiltered rows.
That is not fixable in the catalog, and real Fabric does not try: the platform
**blocks** direct path access to a secured table for non-privileged users, and
lists exactly the patterns it blocks — `spark.read...load`, `DeltaTable.forPath`,
and OneLake REST/SDK reads of a secured `Tables/<table>`. So the fix belongs in
our OneLake surface, refusing the read, rather than in the engine filtering it.

## Direct path access, blocked at the platform

Real Fabric does not filter a raw read, it refuses it: "certain OneLake security
features like row and column level security aren't supported by storage level
operations, [so] not all types of access to row or column level secured data can
be permitted", and "for user access to data in OneLake with RLS or CLS on it,
the query is blocked if the user requesting access isn't permitted to see all
the rows or columns in that table". The Spark article names the three patterns:
`spark.read.format("delta").load("abfss://…")`, `DeltaTable.forPath`, and
OneLake REST/SDK reads of a secured `Tables/<table>` folder.

So this belongs in the OneLake surface, not in the engine. `authorizeViewer`
asks `onelakesec.Narrowing()` after `Allows()` and refuses with the reason
named. One change covers both the DFS and Blob surfaces because both already
route through that function — two spellings of one store, and a refusal only one
of them honours is not a refusal.

**An unrestricted covering grant still reads.** Roles union rather than compete,
so a principal who reaches the table through any grant that narrows nothing may
see all of it, and `Narrowing` scans every covering entry rather than the first.
Intersecting instead would let ADDING a role take access away, which a
Permit-only model cannot express, and would fire the block on principals the
product does not restrict.

**Admin, Member and Contributor are unaffected** — "workspace Admin, Member, and
Contributor roles aren't restricted by RLS or CLS" — and they never reach this
code, because the viewer path is the only caller.

### What this does NOT close, and why

A notebook's `spark.read.format("delta").load("abfss://…")` still returns
unfiltered rows. Not because the rule is missing, but because of WHOSE identity
does the reading: our Spark agent holds one service credential and uses it for
every caller, so the read arrives at OneLake as a Contributor and is correctly
allowed. Real Fabric's user context carries the user's own identity, which is
what makes the platform block reach that call there.

Closing it needs the two-context split — a system context holding the credential
and doing the reading, a user context that never has it. Until then the parity
row says **Partial** and names the gap, because a row claiming the guarantee
would be claiming the half we have as the whole.

## The two-context model

Stage A made OneLake refuse a direct path read from a narrowed principal, and
that refusal does not reach a notebook. Measured (`e2e/two-context/probe.py`),
against a Viewer narrowed to one region and one column:

| question | answer |
|---|---|
| is the SQL path filtered? | yes — 2 of 3 rows |
| can a cell obtain the agent's storage bearer? | **yes** — `__import__('storage').token()` returns it |
| can a cell obtain the credential that MINTS one? | **yes** — `ENTRA_CLIENT_SECRET` is in the process environment |
| can the viewer read the files by path from a cell? | **yes** — all 3 rows, both columns |
| the same viewer's own identity, straight at OneLake? | **403** — stage A, working |

So the gap is the IDENTITY, not the rule. The agent holds one service credential
and uses it for every caller, so a notebook's read arrives at OneLake as a
Contributor and is correctly allowed. Real Fabric's user context carries the
user's own identity, which is what makes the platform block apply there.

The client secret is the sharper half. A token expires and can be scoped; a
secret in the environment lets a cell mint fresh ones indefinitely, for any
audience the app is allowed. No in-process mitigation reaches this: user code
runs through `exec()` in the agent's own process, so `__import__`, `os.environ`
and the module globals are all one namespace away. **The split has to be by
process.** That is what Fabric describes:

> **User context.** Runs the user's notebook … with the user's identity. This
> context plans the query and consumes the filtered output, but it never has
> direct, unfiltered access to secured tables.
>
> **System (security) context.** A privileged, Microsoft-managed context that
> resolves the user's effective access against OneLake, reads the underlying
> Delta files, applies RLS row filtering and CLS projections, and returns only
> the rows and columns the user is allowed to see.

### Stages

**B1 — the user context becomes its own process.** Statements execute in a child
per Livy session that holds neither the storage bearer nor the client secret.
Its Spark session is configured with a token forged for the CALLER, so a path
read arrives at OneLake as that principal and stage A refuses it when the grant
narrows. The child keeps everything a cell can see today — stdout capture, the
`sc` facade, delta_ops interception — or the split is a regression dressed as a
fix.

**B2 — the system context produces the filtered relation.** With B1 the SQL path
would break, because the secured view reads through the user's token and OneLake
now refuses it. So the parent, which still holds the credential, reads the Delta
files, applies the row filter and column projection, and puts the result where
the child can read it without OneLake at all. This is the emulator's version of
"the system context reads and filters"; it materialises where Fabric filters
in-plan, which is a boundary to state, not to hide.

**B1 is wired but dark.** `FABRIC_TWO_CONTEXT=1` turns it on; the default is
off, and deliberately, because B1 ALONE IS A REGRESSION. The child reads as the
caller, and a narrowed table refuses that principal by design, so a secured
session would lose the filtered read it has today until B2 supplies it. Shipping
it dark keeps the code reviewable and the behaviour unchanged. The flag is a
staging device with a removal condition, not a setting: when B2 lands, the
default flips and the flag goes.

**The protocol has a descriptor of its own, on both platforms.** stdout and
stderr stay the child's log; responses travel on a private pipe. The platforms
do not share a mechanism for handing one over, so there are two spawns:

| | POSIX | Windows |
|---|---|---|
| handed over as | a descriptor NUMBER, via `pass_fds` | a kernel HANDLE, via `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` |
| why that works | fork copies the descriptor table, exec keeps what is not close-on-exec | `CreateProcess` builds a fresh process; only named handles are inherited |
| the child does | reads the number from the environment | `msvcrt.open_osfhandle(handle, O_WRONLY \| O_BINARY)` |

`subprocess` refuses `pass_fds` on Windows outright, and it does so with an
`assert` -- so under `python -O` the flag is silently DROPPED rather than
raising, and the child simply has no descriptor. O_BINARY is not decoration
either: the CRT would open the handle in text mode and rewrite every newline,
and newlines are the frame delimiter.

The Windows branch is covered from POSIX by injecting its three platform calls,
because code that first executes on a machine nobody can step through is how the
two earlier portability defects reached CI instead of a test.

**B3 — witnesses and parity.** `e2e/two-context` runs the livy stack with the
split on and asserts what a cell can and cannot reach. It does NOT turn the
direct-path row green, because B2 measured why that is not ours to close; what
it witnesses is the separation.

### Costs, stated before building

- **A process per Livy session** is memory and start-up latency the agent does
  not spend today.
- **B2 materialises.** A filtered snapshot is a copy, and a copy is stale the
  moment the table moves. Per-statement refresh keeps it honest and costs the
  copy each time.
- **The `sc` facade and RDD contract cross a process boundary.** Some of what
  works today may not survive, and what does not must be reported rather than
  quietly dropped.


## B2 landed, and it does not close the path read. Here is why

The system context works. With `FABRIC_TWO_CONTEXT=1` the whole livy e2e passes:
the viewer sees 2 of 3 rows and one column of two, both bypass spellings are
refused, the owner is untouched — and the filtered rows now arrive from a
snapshot the privileged half wrote, not from a view over a table the caller can
still name. Withholding got simpler too: a table the caller may not read is
never registered in the user context, so there is nothing to drop and nothing to
sweep. The catalog sweep and the shared-catalog refusal exist to simulate, inside
one namespace, the separation this path actually has.

Re-running `e2e/two-context/probe.py` with the flag on:

| question | before | after |
|---|---|---|
| `ENTRA_CLIENT_SECRET` in the cell's environment | **yes** | **no** |
| `storage.token()` returns a bearer | the SERVICE one | the CALLER's own |
| SQL path filtered | 2 of 3 | 2 of 3 |
| **`spark.read...load(abfss://…)` from a cell** | **3 rows, both columns** | **3 rows, both columns** |

The escalation is closed. The path read is not, and the reason is not in the
agent at all.

**The engine is a third process, and it holds a credential of its own.**
Measured, in the running container: `docker/sail/launcher.py` mints a bearer
with client credentials at start-up and exports it as `AZURE_STORAGE_TOKEN` into
Sail's environment, and the live Sail process has it. A bare
`spark.read.load("abfss://…")` is executed BY SAIL, so it uses Sail's daemon
identity whatever the calling process holds. Splitting the agent into two
contexts cannot reach that, because the read never happens in either of them.

Real Fabric does not have this problem: its Spark executors run in the user's
context with the user's identity, so the platform block applies to the engine's
own read. Our Sail is one long-lived shared server with one identity.

### The option, and its cost

There is a way to close it: **take the ambient credential away from the
engine**. With no `AZURE_STORAGE_TOKEN` in Sail's environment, every read must
carry its own options — the agent's own paths already pass them explicitly, so
the system context keeps working, and a bare path read from a cell fails for
want of a credential.

It is not a small change. It reaches EVERY session, not just secured ones: any
notebook anywhere that reads `abfss://` without options stops working, which is
a large behavioural change to buy one guarantee. It should be measured against
the e2e suite before anyone commits to it, and it is not in this stage.

Until then the direct-path-access parity row stays **🟡**, and it says the
engine is the reason. A row claiming the guarantee would be claiming a
separation that a third process quietly opts out of.


## B3: what the witness asserts, and the one thing it only watches

`e2e/two-context/` layers `FABRIC_TWO_CONTEXT=1` over the livy stack — a layer
rather than a fifth copy of those four services, so the base cannot drift away
from it — and runs as `ci:two-context`:

```
viewer: 2 of 3 rows, columns region_id  -- supplied by the system context
viewer catalog: ['sales']               -- `secret` is not merely unreadable, it is absent
viewer environment: ['AZURE_STORAGE_TOKEN', 'ENTRA_TOKEN_URL']
the cell's bearer is the caller's own, not the agent's
owner still sees 3 of 3
```

Every claim is paired with one that must still succeed. A stack where nothing
worked would satisfy "the cell cannot reach the secret" and prove nothing, so
the viewer being *filtered rather than broken* is asserted in the same run, and
so is the owner being untouched. The bearer check compares against the service
token the harness itself holds: not merely "a token", and not the agent's.

**The path read is a tripwire, not a guarantee.** It still returns all 3 rows,
and the witness asserts exactly that, with the reason in the failure message:

> the path read returned N, not 3. If the engine now carries per-session
> identity, this is GOOD NEWS: update docs/54 and flip the direct-path-access
> parity row from Partial to Real.

Asserting the current number is the difference between a documented gap and a
forgotten one. A 🟡 row with nothing watching it stays 🟡 long after the reason
expires; this one fails the build the day the reason does.
