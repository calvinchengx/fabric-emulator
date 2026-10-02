# 65 — The REST surface ledgers: four questions, four gates, and the numbers no page explained

**Status: three evidence files under `docs/` are rewritten by CI on every run,
are read by four gates, and were named by no prose page in this repository —
so every number in them was reviewable only by reading the checker that wrote
it. This document is the missing half. Writing it found two of the three
carrying numbers no gate compares, one of which disagrees with its own
siblings.**

**The headline: `docs/route-coverage.json` records `exercised: 116`,
`registered: 174` and 61 not-yet-exercised routes, and those three numbers
cannot all be true.** Its writer defines `exercised` as
`registered - len(notYetExercised)`, which is 113. The gate reads only the
*set* of 61 route names and never looks at either count, so the file has been
internally inconsistent and green at the same time.

This is the seventh document on this architecture, after
[60-test-flakiness.md](60-test-flakiness.md)'s three language surfaces,
[62-performance-regressions.md](62-performance-regressions.md)'s unbounded
request bodies, [63-security-footguns.md](63-security-footguns.md)'s unread
source and [64-logging-quality.md](64-logging-quality.md)'s unregistered log
sites. It differs from all four in one way worth stating up front: **it lands
no new checker and changes no gate.** The machinery here already existed and
already works. What was missing was the prose, and a measurement nobody can
read is a measurement that decays the way
[docs/10](10-testing.md#the-failure-this-codebase-keeps-producing)'s item
twelve describes — confidently, and with nothing going red.

## Why this exists

Four machine-written ledgers sit under `docs/`. Every other one in that
directory has a chapter that names it and says what its numbers mean:
`docs/test-flakiness.json` has [60](60-test-flakiness.md),
`docs/perf-regressions.json` has [62](62-performance-regressions.md),
`docs/security-footguns.json` has [63](63-security-footguns.md),
`docs/logging-subsystems.json` has [64](64-logging-quality.md),
`docs/witnesses.json` is named by ten pages.

These three had none:

| File | Named by | Written by |
|---|---|---|
| `docs/surface-ledger.json` | one release note | `scripts/check_surface_ledger.py --update` |
| `docs/route-coverage.json` | two release notes | `scripts/check_route_coverage.py --update` |
| `docs/undocumented-routes.json` | **nothing at all** | `scripts/check_undocumented_routes.py --update` |

A release note is not a substitute, and
[`scripts/check_doc_drift.py`](../scripts/check_doc_drift.py) already says why
in its own source: `docs/release-notes/` is skipped outright, because a note
describes the tree *at its tag* and editing it to match today would falsify a
historical record. So a reader who wanted to know what `silent: 563` counts, or
what the denominator of a coverage ratio was, had exactly one route to the
answer — read the checker.

## The four questions

The four gates look like overlapping coverage checks and are not. Each asks a
question the other three structurally cannot, and the distinction is what makes
four of them worth maintaining.

| Question | Gate | Denominator |
|---|---|---|
| Documented, but we answer nothing and nobody noticed? | `check_surface_ledger.py` | what Microsoft's vendored swagger documents |
| We serve it, but no traffic ever proved its shape? | `check_route_coverage.py` | what this emulator registers |
| We serve it, but Fabric does not document it at all? | `check_undocumented_routes.py` | what this emulator registers |
| We serve it and answered the wrong shape? | `check_openapi_conformance.py` | what a recording happened to touch |

The first two point in opposite directions from the same pair of artifacts.
The third is the inverse of the first's denominator. The fourth is the only one
that reads response *bodies*. A surface can be at 100% route coverage and
answer nothing at all for two thirds of the API — and before the ledger
existed, no gate would have said so.

## 1. `docs/surface-ledger.json` — documented, and in one of three states

**Denominator: every operation Microsoft's vendored swagger documents**, under
the two prefixes a recording can ever prove. Not the published reference pages
— the swagger committed in this tree, under
`third_party/fabric-rest-api-specs` and `third_party/powerbi-rest-swagger`.

```
1002 documented  =  422 served  +  17 refused  +  563 silent
```

Three states, and only three:

- **SERVED** — a registered route matches the documented method and path
  shape. What it then *answers* is conformance's question, not this one.
- **REFUSED** — not served, and **measured** to answer a legible 404: a
  recording holds a response for a concrete path under this operation. A
  refusal is a fine answer; an unasserted one is not, which is why this state
  is evidence-backed rather than declared.
- **SILENT** — neither. Nothing serves it and nothing has ever asked. This is
  the state that hurts: an integration that needs the route finds out at
  runtime, and no test in this repository disagrees.

**The gate is on the silent set**, and it ratchets both ways: silent must not
grow, and an operation that *leaves* silent must leave the baseline too, or the
file records a lie about the tree. The `servedOperations` and
`refusedOperations` lists exist so that leaving either state is a diff somebody
reviews.

Shapes are matched with **parameter names erased** — the spec writes
`{workspaceId}` and this emulator writes `{wid}`, and they are the same route.
Go's `{path...}` wildcard folds to the same placeholder, which is coarser than
the router and deliberately coarse in the direction of claiming *less*
coverage.

**It does not say a served operation is correct.** An operation can be SERVED
and wrong. It can also be REFUSED and that be exactly right — this emulator has
no personal workspace and no PBIX importer, and pretending otherwise would be
worse than the 404.

### Its history is a measurement worth keeping

The alias expansion in `served_shapes()` was once missing, and the ledger read
`served: 120` / `silent: 865`. Fixing it moved those to 422 and 563: **302
operations had been recorded as nothing-has-ever-asked while the emulator was
serving them.** Microsoft documents `.../notebooks` and `.../warehouses` as
separate operations and this emulator answers both, so each is its own fact
here — this gate deliberately does *not* collapse the families its sibling
collapses.

## 2. `docs/route-coverage.json` — served, and whether anything ever drove it

**Denominator: the routes this emulator registers** under the in-scope
prefixes, with alias families collapsed back to one row each. Routes are
matched on the expanded names and **counted on the collapsed ones**, because a
baseline carrying hundreds of rows that are one handler is a baseline nobody
reads.

The collapse folds **by provenance, not by spelling**, and that is not
academic: `sqlEndpoints` is itself a typed collection, so a name-matching
version folded the literally-registered
`POST .../sqlEndpoints/{epid}/refreshMetadata` into the family and thereby
claimed all 51 collections answer `refreshMetadata`.

It ratchets **three** ways, and the third sounds perverse:

- a **new** route with no conformance traffic fails, so an endpoint arrives
  with its evidence rather than acquiring it later;
- a route that **stops** being exercised fails;
- a route that **starts** being exercised also fails — the baseline is a record
  of what is *not yet proved*, and an improvement that does not shrink it
  leaves a lie in the file.

**Why not simply require full coverage.** Because the honest denominator is
what this emulator serves, and even those cannot all be reached by the suites
that exist today. A gate nobody can pass is a gate somebody deletes; a gate
that only ratchets is one that keeps paying.

What counts as exercised is deliberately crude: **one recorded response on a
route, from any suite that records.** Not every status, not every parameter.

### The inconsistency this document found

`write_baseline(uncovered, total)` writes `registered` as `total` and
`exercised` as `total - len(uncovered)`. The three committed numbers must
therefore satisfy `exercised + len(notYetExercised) == registered`. They do
not:

```
exercised 116  +  notYetExercised 61  =  177     !=     registered 174
```

Measured against the tree at this commit, the live collapsed registration count
is **177** — which is what `116 + 61` says, and not what `registered` says. So
`registered: 174` is the stale field, and the two that agree with each other
also agree with the tree.

**Nothing catches this, by construction.** `read_baseline()` returns
`set(...["notYetExercised"])` and nothing else: the gate compares route *names*
in both directions and never reads either count. The counts are written by
`--update` and then never looked at again. The `Makefile` comment beside
`check_backward_compat.py` is written as though they were — it notes that
deleting a served route reads in review as `174 -> 173` and never names
itself, which is true, and which also depends on a number no gate maintains.

This chapter does not fix it: the repair is one `--update` against the CI
recordings, and that regenerates a reviewed artifact, which belongs in a change
that can show the diff rather than in a documentation backfill. What this
chapter does is make the number legible enough that the next person can see it
is wrong.

## 3. `docs/undocumented-routes.json` — served, and documented by nobody

**The fourth direction, and the only one nothing measured for a long time.**
The other three all point from a spec, or from traffic, towards evidence. None
of them asks the inverse: *which routes does this emulator register that no
published spec describes?*

**That direction is the dangerous one.** A documented operation we do not serve
fails here and works in Fabric — annoying, and discovered on the first call. A
route we serve that Fabric does not is the opposite: a script is written
against the emulator, passes, ships, and 404s in production. **The emulator
being more permissive than the thing it emulates is the worst direction for a
fidelity bug to point.**

Four states, and a route is exactly one of them:

- **DOCUMENTED** — method plus parameter-name-erased shape matches a documented
  operation.
- **ALIAS** — a typed-collection spelling whose `/items` equivalent is
  documented. `GET .../notebooks/{iid}` *is* the generic
  `GET .../items/{itemId}` with the type forced, so crediting it is not a
  loophole; it is the same operation.
- **NATIVE** — declared by pattern in the script's `NATIVE` table **with a
  written reason**: either emulator-native, or a real Fabric surface whose
  protocol is not REST at all. The MCP endpoints are the clearest case — they
  speak JSON-RPC over MCP Streamable HTTP, so no swagger can describe them and
  none does. A `NATIVE` pattern that matches no registered route is itself a
  failure, so the table cannot rot into a silencer.
- **UNDOCUMENTED** — everything else, ratcheted in **both** directions against
  the baseline. The set growing fails, and a route leaving it fails too.

**Why this one runs in `make check`.** Its two siblings need a recording and
live in the aggregate CI job behind eleven e2e suites. This one reads the Go
source and the vendored swagger and nothing else: offline, deterministic, and
answerable before a push.

### Six routes, each adjudicated

The baseline's `notes` object records what was checked for each. They are not a
backlog of accidents — two are a deliberate stand-in, one is a spelling
disagreement with the swagger, and three are emulator-native graph surfaces:

| Route | Why |
|---|---|
| `GET /v1/admin/labels` | The sensitivity-label taxonomy is the emulator's own; real Fabric gets labels and their order from Purview, which cannot be attached offline. `internal/api/labels.go` calls it "an emulator affordance, not a Fabric API" at the registration. The two label operations Microsoft *does* document are served and classify as documented. |
| `GET /v1/workspaces/{wid}/lineage` | Workspace lineage as a REST collection is emulator-native: Fabric shows lineage in its portal and publishes no operation for it. |
| `POST /v1/workspaces/{wid}/lineage` | The write half of the same graph — an engine that is not a queued notebook run reports what it moved (`internal/api/reportlineage.go`). |
| `POST .../items/{iid}/jobs/instances` | **A spelling disagreement, not an invented surface**, and left here rather than matched away because this is exactly what the gate is for. The operation *is* documented, and the vendored swagger spells it with `jobType` as a path parameter where this emulator does not. |
| `POST .../mirroredDatabases/{iid}/refreshMirror` | Checked against the swagger first: Microsoft documents `startMirroring`, `stopMirroring` and two status reads, and no `refreshMirror`. This is the emulator's explicit on-demand snapshot standing in for continuous replication it cannot run offline (`internal/api/mirror.go`). |
| `POST .../sqlDatabases/{iid}/refreshMirror` | As above, on the `sqlDatabases` spelling. |

### Its counts are a stale snapshot, and the gate is right not to care

The committed file and the live tree disagree on three of five numbers:

| | committed | tree at this commit |
|---|---|---|
| registered in-scope routes | 613 | **1059** |
| documented | 422 | 422 |
| typed alias | 148 | **589** |
| declared emulator-native | 37 | **42** |
| undocumented | 6 | 6 |

Both rows are internally consistent — they each sum correctly — so unlike
`route-coverage.json` this is one honest snapshot that has simply aged, not a
file at war with itself. **And `--strict` exits 0**, correctly: the invariant
this gate exists to hold is the six-route set, and that set has not moved. The
counts are context written at `--update` time and compared by nothing.

That is a defensible design and it has a cost worth naming once: **four numbers
published under `docs/` describe a tree from several hundred route
registrations ago.** Anyone quoting `613 registered` from this file is quoting
history.

## 4. `check_openapi_conformance.py` — served, and the shape it answered

The fourth gate reads no ledger of its own; its output is findings against a
recording. It is listed here because the other three keep deferring to it, and
a reader should know what it does and does not promise.

Its input is produced by the emulator itself: `FABRIC_RECORD_RESPONSES=<file>`
appends one JSON object per line for each documented-surface response — method,
path, status, body, and **no headers, ever** (`internal/server/record.go`). The
e2e suites already generate the traffic; recording is the only new thing, and a
suite that does not set the variable simply contributes nothing.

Three classes are checked, chosen because they are what a typed client
stumbles into: an **undocumented status**, a **missing required property**
(including inside arrays, which is where most of the surface lives), and a
**wrong primitive type** or non-member enum value.

**Unexpected properties are deliberately not checked.** Swagger omits
`additionalProperties` almost everywhere, so "extra field" would fire on nearly
every response — and a checker that cries wolf gets muted, which
[docs/10](10-testing.md) accounts for at length.

**What a pass means: shape, never semantics.** A job reported `Succeeded` that
ran nothing is perfectly conformant. This says the answer was *shaped* right,
not that it was *true*.

## Where each gate runs, and what that means for you

| Gate | `make check` | CI | Needs a recording |
|---|---|---|---|
| `check_undocumented_routes.py` | **yes** | yes | no |
| `check_openapi_conformance.py` | no | aggregate job | **yes** |
| `check_surface_ledger.py` | no | aggregate job | **yes** |
| `check_route_coverage.py` | no | aggregate job | **yes** |

The split is the `Makefile`'s own reasoning: the one gate that needs no
recording runs before a push, and its three siblings cannot, because they read
recordings produced by eleven e2e suites.

**This matters for a practical reason.** Running the recording-bound gates with
no recording does not report "clean" — it reports *wrongly*. With no arguments,
`check_surface_ledger.py` measures 0 refusals and 580 silent operations
(the 17 measured refusals fall back into silent, 563 + 17 = 580), disagrees
with the committed baseline on all 17, and `--strict` **exits 1**.
`check_route_coverage.py` refuses to run at all without a recording argument.
Neither is broken; both are telling you the input is missing. Do not read
either as a verdict on the tree.

### Regenerating a baseline

The aggregate job in `.github/workflows/ci.yml` collects every `recording-*`
artifact into a file of paths and passes the union to all three gates:

```bash
python3 scripts/check_openapi_conformance.py $(cat recordings.txt) --strict
python3 scripts/check_surface_ledger.py      $(cat recordings.txt) --strict
python3 scripts/check_route_coverage.py      $(cat recordings.txt) --strict
```

Swap `--strict` for `--update` to rewrite a baseline, and **let the diff be the
review** — that is the whole mechanism by which these ratchets stay honest.
`check_undocumented_routes.py` takes no recording:

```bash
python3 scripts/check_undocumented_routes.py --update
```

The union matters more than it looks. That job **fails if any of eleven named
suites uploaded no recording** — `fabric-cli`, `az-rest`,
`pipeline-activities`, `terraform-fabric`, `powerbi-ps`, `medallion`,
`semantic-model`, `livy`, `sail`, `eventstream`, `data-science-loop`. A short
union would make the route ratchet blame the routes for a missing suite, which
is [docs/10](10-testing.md)'s item nine exactly: a true measurement answering
an adjacent question.

## Reading these alongside the parity map

[parity.md](parity.md) is a hand-written judgement about whether *real work
happens*. These ledgers are machine-written counts of *surface*. Neither
substitutes for the other, and the two use **different denominators that do not
reconcile by arithmetic** — so a reader comparing the headline numbers should
know why before concluding one of them is wrong.

[parity.md's own scope section](parity.md#what-the-denominator-is) cites
Fabric's REST reference as publishing *on the order of 880 operations across
~57 workload groups*. This ledger counts **1002**. Both are correct, about
different things:

```
1002 documented operations in docs/surface-ledger.json
  =  715 Fabric  (/v1,        third_party/fabric-rest-api-specs)
  +  287 Power BI (/v1.0/myorg, third_party/powerbi-rest-swagger)
```

Two independent reasons the figures differ:

- **The ledger counts a second product.** 287 of its operations are Power BI's
  `/v1.0/myorg` surface. The parity map's ~880 is a Fabric figure and does not
  count them at all.
- **The ledger's Fabric half is smaller, not larger.** 715 is below ~880,
  because the denominator is the swagger **vendored in this tree** rather than
  everything the published reference describes. A spec not committed here
  cannot be counted, and committing more would make the silent count go *up* —
  which is the correct direction for an honest gate and an uncomfortable one
  for a dashboard.

So: the parity map for whether a surface does real work, the ledger for whether
this emulator answers at all, and neither ratio read as a coverage figure
against Fabric. The parity map says this plainly about itself and it is worth
repeating here — **a green count says the chosen surface is complete and
proven, never that Fabric is covered.**

## What none of this says

Stated plainly, because a document listing only what it covers reads as
complete:

- **Nothing here is about behaviour.** All four gates are shape and presence.
  The parity map, `docs/witnesses.json` and the client witnesses in
  [24](24-parity-completion.md) are where "does it really work" lives.
- **The silent set is not a backlog.** 563 operations are unimplemented and
  unasked-about, and most of them are correctly so — the surface this emulator
  implements is self-selected, as the parity map explains.
- **Two ledgers publish counts no gate holds.** Both cases are above. The sets
  are gate-held; the counts are prose written by a script, and they age.
- **A recording proves one response.** Route coverage credits a route on one
  recorded response, not on every status or parameter combination.
- **There is no ratchet on the denominators themselves.** Vendoring a new
  swagger tree changes 1002, and nothing fails to point that out.
