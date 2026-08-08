# 32 — XMLA: what is measured, and what it would cost

**Status: not implemented, and deferred on COST — not on feasibility.** That
distinction is the whole point of this document. Feasibility was the stated
blocker for months, it was wrong, and [`e2e/xmla`](../e2e/xmla/) disproved it by
measurement. What remains genuinely unknown is size, and there is one cheap
experiment that would settle it.

[33-pbix-tooling.md](33-pbix-tooling.md) is the other half of the same
question — this is how a BI client READS the model; that is what we can hand it
as a file. [18-semantic-model-references.md](18-semantic-model-references.md) holds the
specs; [24-parity-completion.md](24-parity-completion.md) holds the sizing this
plan is trying to replace with a number.

## Why anyone wants it

`docs/24` gates XMLA on "cost and real demand", with the trigger written as
*"XMLA when a real SemPy user appears."*

[semantic-link-labs](https://github.com/microsoft/semantic-link-labs) is that
user. It is Microsoft's own library for semantic-model work — Vertipaq
analysis, best-practice rules, DAX execution, import → Direct Lake migration,
exporting a report as `.pbip` — and the operations that matter need **XMLA
read/write**. It runs inside a Fabric notebook on top of `sempy`, and as of the
emulator driving RunNotebook itself ([notebookdrive.go](../internal/api/notebookdrive.go))
there is somewhere for it to run.

Naming the demand matters because the gate was never technical.

## What is settled

Six facts about Microsoft's own ADOMD.NET client, every one read off the wire by
`e2e/xmla` and re-asserted weekly rather than remembered:

| | Measured |
|---|---|
| Platform | Runs on **Linux**, .NET 8, in a container — `PLATFORM Unix/X64` |
| Endpoint | **Overridable.** `Data Source=powerbi://<host>:<port>/v1.0/myorg/<ws>` sends TLS to whatever host you name |
| TLS | Trusts a **self-signed CA** through the ordinary `update-ca-certificates` route |
| Credential | Takes a **bearer from the connection string** (`Password=<token>`) — nothing interactive, nothing Windows-only |
| First call | **Plain JSON REST, not SOAP:** `GET /powerbi/databases/v201606/workspaces?PreferClientRouting=true` — and it is made **twice**, once with the flag and once without |
| Form | `https://…/xmla` and bare `host:port` **are** Windows-only on .NET Core, so `powerbi://` is mandatory — and is the form the Service documents anyway |

`docs/18` deferred XMLA on "native .NET, **not endpoint-overridable**" and "no
CI oracle." Both were false. A claim that expensive when wrong is now a check
rather than a memory, which is why `e2e/xmla` is a suite and not a note.

## What is NOT settled

The client never reaches XMLA/SOAP in the oracle. It is still in workspace
**routing** when the capture stub refuses it, so nothing here says how much of
`[MS-SSAS-T]` a useful implementation needs.

**Feasibility is measured. Cost is not.** The `L` in `docs/24` stands.

## What already exists to build on

| Asset | Why it matters |
|---|---|
| [`internal/semanticmodel`](../internal/semanticmodel/) | A real, bounded DAX evaluator — `EVALUATE`, `SUMMARIZECOLUMNS`, measures — already answering `executeQueries`. **XMLA needs a transport, not an engine.** |
| [`internal/tds`](../internal/tds/) | Precedent: terminate a Microsoft wire protocol plus Entra FedAuth and splice to a real backend. `docs/24` calls XMLA "the same class of work" |
| [`third_party/bi-shared-docs`](../third_party/bi-shared-docs/PROVENANCE.md) | The paper specs, pinned by SHA — the `rowset` shape an `EVALUATE` returns, and the `MDSCHEMA_*` / `DISCOVER_*` schema rowsets |
| [`e2e/xmla`](../e2e/xmla/) | A live ADOMD.NET client on Linux, already wired into CI |

The evaluator is the load-bearing one. `executeQueries` and XMLA are two
envelopes around the same DAX, so most of what an XMLA `Execute` must answer is
already answered somewhere else in this repository.

## Phase 0 — RUN, and it answered two questions

**Status: DONE as measurement.** `e2e/xmla` now runs the probe **twice** — once
refusing the routing call (the regression witness for the first-call contract,
unchanged) and once answering it. Answering in the same pass would have
destroyed the thing the first pass pins, which is why it is a second run rather
than an edited stub.

**Finding 1 — the doubled routing call is a 404 FALLBACK, not unconditional.**
This is the question `e2e/xmla`'s README flags as unestablished, and it is now
settled by contrast rather than by reading:

| Listener | Captured |
|---|---|
| Refuses (404) | `…workspaces?PreferClientRouting=true` **then** `…workspaces` — twice per connection |
| Answers (200) | `…workspaces?PreferClientRouting=true` **only** — once per connection |

An implementation therefore does *not* need to serve the un-flagged form to a
client it answers; that form only ever appears after a refusal.

**Finding 2 — the reply is CONSUMED, and the failure moved from transport to
lookup.** With a JSON body of the hypothesised shape, ADOMD.NET no longer
complains about the response at all. It reports:

```
AdomdConnectionException :: The specified Power BI workspace ('ws') is not found.
```

That is a *lookup* failure, not a parse failure — the client deserialised the
body, searched it for the workspace named in the Data Source, and did not find
a match. So the envelope is close enough to be consumed, and what remains
unknown is narrower than "what shape": it is **which field the client matches
the workspace name against**. The hypothesis sent `name`, `id`,
`capacityObjectId` and `clusterUri`; one of those is wrong or one is missing.

**Nothing here is asserted as a contract.** The reply shape is still a guess,
and a suite that asserted it would pass by confirming its own guess. Both
findings are printed observations; the next iteration turns one into a test.

### Screen 2 — RUN: the envelope is required, the field spelling is not the variable

Three candidate shapes, one per connection, in a single run. The single-shape
run is the **control**: all three connection types reported identically there,
so any difference here is attributable to the shape rather than to the Data
Source form.

| Shape | Client's answer |
|---|---|
| bare array (no `value`) | `ArgumentNullException :: Value cannot be null. (Parameter 'value')` |
| `value` envelope + 10 field spellings | `…workspace ('ws') is not found` |
| PascalCase (`Value`/`Name`/…) | `…workspace ('ws') is not found` |

**What this appeared to establish — and screen 3 DISPROVED it.** The reading at
the time was "removing the envelope changes the failure to a null argument, so
the client requires it," with PascalCase `Value` seeming to confirm a
case-insensitive envelope. That was one correlation with one data point, and it
was wrong. Left here rather than deleted because the correction is the finding.

### Screen 3 — RUN: the body is not the variable at all

Designed so two slots would settle screen 2's `value` ambiguity from opposite
sides. They settled it against screen 2's conclusion.

| Shape | Client's answer |
|---|---|
| A `{"value": []}` — key present, list empty | `…workspace ('ws') is not found` |
| B name nested under `properties`/`workspace` | `…workspace ('ws') is not found` |
| C `{"workspaces":[…]}` — **no `value` key at all** | `…workspace ('ws') is not found` |

**C is the disproof.** Screen 2's bare array also lacked `value` and produced
`ArgumentNullException`; C lacks it too and produces the ordinary not-found. Two
shapes missing the same key, two different answers — so **the missing key was
never the cause.** The variable in screen 2 was the top-level JSON *type*: an
array where an object was expected deserialises to null, and the
`ArgumentNullException` was about that, exactly as the "may name a method
parameter, not our JSON key" caveat warned.

**What is actually established, after three screens.** Every top-level JSON
*object* tried so far returns the identical not-found, across: `value` present
or absent, empty or populated, ten field spellings, PascalCase, and nesting.
Only changing the top-level type changed anything. **The body's content does not
determine this outcome.**

That is a stronger negative than any of the individual shapes, and it redirects
the search off the body entirely. Remaining candidates are things no body edit
can reach: the response's `Content-Type`, its status code, a header the client
requires, or a second request it makes and we have not yet answered. The next
screen should vary one of those and leave the JSON alone.

**Method note worth keeping.** Screen 2's conclusion survived exactly one round.
It was a single correlation stated with a caveat, and the caveat is what made it
cheap to overturn — the discriminator was already written down, so screen 3 only
had to run it. A conclusion published without its discriminator would have been
built on instead.

### Screen 4 — RUN: the body IS read, and the serializer names itself

Varied status, headers and emptiness; left the JSON alone. **The pairing guard
fired** — `NOT PAIRED: 4 shape(s) served, 3 powerbi case(s) reported` — so the
per-shape labels in that run are misaligned and were not read. Recording what
is *directly observed* and what is *inferred*, separately, because the guard
exists precisely to stop the second being published as the first.

**Observed.** Two connections returned

```
SerializationException :: Expecting element 'root' from namespace ''..
Encountered 'None' with name '', namespace ''.
```

one returned the familiar not-found, and four responses were served to three
connections.

**What that error names, and it is the biggest find so far.** "element 'root'
from namespace ''" is not XML being demanded — it is
**`DataContractJsonSerializer`**, which deserialises JSON by mapping it onto an
XML infoset whose root element is literally called `root`. An empty body has no
root, hence this exact message.

Two consequences:

1. **The body IS consulted.** The worry after screen 3 — that every shape was
   varying something the client never reads — is answered: an empty body fails
   *at deserialisation*, so the populated ones were deserialised and then found
   wanting. Screens 2–3 were measuring a real variable after all.
2. **The matching rule is a `[DataContract]`, not a guess.** With
   `DataContractJsonSerializer`, member names come from `[DataMember(Name=…)]`
   on a concrete type inside the shipped assembly. That is a fact **readable
   from `Microsoft.AnalysisServices.AdomdClient`** rather than searchable by
   trial — which makes the next step inspection, not another screen.

**Inferred, and flagged as such.** Shapes rotate per REQUEST, so four requests
across three connections means one connection made two — consistent with the
`307` being followed, which would also explain a redirect-following connection
receiving the next shape's body. That reconstruction fits every observed
message, but it is arithmetic on a counter, not a captured sequence. **The
harness printed the request log and the operator truncated it** when reading
the output; re-run capturing full stdout before treating the redirect-follow as
established.

### The assembly read — DONE: the contract, no longer a guess

Reflecting over `Microsoft.AnalysisServices.AdomdClient` **19.84.1.0** (429
types, inspected in `mcr.microsoft.com/dotnet/sdk:8.0`) yields the reply's
declared shape:

```
PbiPremiumAuthenticationHandle+Workspace201606
    id · name · type · capacitySku · capacityUri        [DataMember] ×5
WorkspaceType201606            = User | Group | Folder
WorkspaceCapacitySkuType201606 = Shared | Premium
ResolvePbiWorkspaceErrorReason = None | WorkspaceNotFound
                               | WorkspaceNotOnPbiPremium | WorkspaceNameDuplicated
```

Two things follow that no screen could have reached:

1. **No type in the assembly holds a `Workspace201606` collection.** The
   payload is a **bare array**, not an enveloped list — so every enveloped
   shape in screens 2–4 was answering a question the client never asks.
   Screen 2's bare array failed for an unrelated reason, which is precisely
   how it produced a wrong conclusion that survived a round.
2. **Three of the five members were never sent.** Screens 1–4 sent `name` and
   `id`; `type`, `capacitySku` and `capacityUri` deserialised null every time.
   No amount of re-spelling could have helped — the names were already right
   and the *set* was short.

The error enum is also a free discriminator: a reply that reaches the workspace
but fails its premium check reports `WorkspaceNotOnPbiPremium`, distinct from
`WorkspaceNotFound`. Which error appears is therefore progress information, not
just failure.

### Screen 5 — RUN: routing is answered, and the failure moved downstream

Shape A — bare array, all five members, `type=Group`, `capacitySku=Premium`,
`capacityUri=https://<host>/`.

**Observed.**

- **One** routing request in the answered run, against **six** in the refusing
  control (two per connection: with `PreferClientRouting=true`, then without).
  The retry a rejected reply provokes did not happen.
- All three `powerbi://` connections reported
  `IndexOutOfRangeException :: Index was outside the bounds of the array.`
- Neither error that pinned every earlier screen appeared: no
  `SerializationException`, no `The specified Power BI workspace ('ws') is not
  found`.
- The listener ran `serve_forever` for the whole probe and was not shut down
  between connections, so the two missing requests are the client's choice and
  not the harness closing.
- The probe's `catch` cannot raise this itself — its only indexing is
  `e.Message.Split('\n')[0]`, and `Split` always yields index 0 — so the throw
  is inside `AdomdConnection.Open()`.

**Inferred, and flagged as such.** The reply deserialised and the workspace
matched: both diagnostic errors are gone and the client stopped re-asking.
Connections 2 and 3 issued no request at all, which fits a process-wide cache —
consistent with the `WorkspaceResolver.Initialize` / `friendlyNameMap` /
`personalWorkspace` members the same assembly read turned up, though nothing
here observes the cache directly. The surviving failure is most plausibly
`capacityUri` being parsed into a cluster host (the assembly carries
`ASAzureUtility+PowerBIClusterResolutionResult` with `FixedClusterUri` and
`DynamicClusterUri`), but that is **untested**.

**Shapes B and C were never served, so they remain untested.** This is a real
limit of the screen design, now recorded in the harness itself: rotating one
shape per request only screens three hypotheses *while shapes fail*. The first
shape the client accepts ends the request stream. A screen is a tool for
failure; past the first success the harness must vary shapes across runs.

### Screen 6 — RUN: the frames name the parser, and `capacityUri` is the field

Two changes made this screen possible, both forced by screen 5 succeeding:

- **One probe run per shape.** An accepted reply is cached for the life of the
  client process, so the 2nd and 3rd shapes of a rotating screen are never
  requested. The phase-0 loop now restarts the container per shape.
- **The probe prints the exception's frames.** `IndexOutOfRangeException`'s
  message names nothing. Its stack names the method. One run collects that;
  screening candidate URI formats costs one run per guess.

**Observed — the throw moved one frame shallower.** In the refusing control the
stack ends inside the resolver's HTTP call, with an inner
`WebException :: (404) Not Found` under
`ConnectivityHelper.ExecuteJsonBasedHttpRequestImpl(…, DataContractJsonSerializer
responseSerializer, …)` — which also confirms screen 4's serializer reading from
a frame rather than from an error string. With an accepted shape,
`TryResolveWorkspaceWithWorkspaceResolver` is **absent from the stack** and the
throw is in its caller, `PbiPremiumAuthenticationHandle.TryResolvePbiWorkspace`.
The resolver returned successfully. That is screen 5's inference re-derived
from the frames instead of from request counts.

**Observed — the screen.**

| shape | `capacityUri` | outcome |
| --- | --- | --- |
| A | `https://<listener>/` | `IndexOutOfRangeException` |
| B | `""` | `InvalidDataException :: Internal error: the Power BI premium workspace's capacity uri is null or empty!` |
| C | `https://wabi-…redirect.analysis.windows.net/` | same as A |
| D | `host.docker.internal:18446`, no scheme | same as A |

B was the built-in discriminator and it fired: the field is read, validated
non-empty, then parsed, with the client's own text naming it. **`capacityUri`
is implicated.** C and D rule out the two obvious candidates — not the scheme,
and not that the host must resemble a real cluster.

**Inferred, and flagged as such.** What A, C and D share is an **empty path**,
and `TryResolvePbiWorkspace` derives two values nothing in the reply carries —
`pbiDedicatedRolloutFqdn` and `capacityObjectId` (both `out` in its signature).
Splitting the URI and indexing absent segments fits every observation, but no
run has yet varied path depth, so it is untested.

### Where this leaves the search

A **path-segment ladder** — `capacityUri` with 1, 2, 3, 4, 5 segments — to find
how many the parser indexes. The failure mode dictates the experiment rather
than a guess at a URL format: `IndexOutOfRange` means indexing past the end, so
the variable to sweep is length.

`pbiDedicatedRolloutFqdn` is the strategically important one. It is the host
the client dials after routing, which makes it the hook for steering ADOMD.NET
back to the emulator — the point at which Phase 0 ends and Phase 1 has a
client to talk to.

### Original framing, kept because the reasoning still holds

Replace the capture stub's 404 with a real handler for
`GET /powerbi/databases/v201606/workspaces`. It is JSON REST over the workspace
list we already hold: no SOAP, no envelopes, no rowsets.

The deliverable is not the feature. It is **measurement**: answering this call
advances the client to its next request, which nobody has ever observed. One
small change converts the roadmap's largest unknown into a recorded sequence,
and settles the open question above — whether the doubled routing call is a
404 fallback or unconditional. `e2e/xmla`'s README flags that as unestablished
precisely because the stub refuses the first call.

Everything below should be priced *after* Phase 0, not before.

## Phases 1–3, in the order the client forces

1. **Discover.** The schema rowsets — `DISCOVER_PROPERTIES`,
   `DBSCHEMA_CATALOGS`, `MDSCHEMA_MEASURES` — as SOAP envelopes wrapping
   rowset payloads. Specs pinned in `third_party/`.
2. **Execute.** `EVALUATE <DAX>` inside a SOAP Execute, returning a `rowset`.
   This is where the existing evaluator plugs in unchanged: encoding work
   rather than engine work.
3. **Write.** TMSL over XMLA — `Alter` / `Create` / `Refresh`. The largest
   piece, and the one semantic-link-labs needs for Direct Lake migration.

Throughout, `e2e/xmla` changes character: from a capture stub that records what
a client demands, to a real client-against-emulator suite. It keeps its weekly
cadence either way — ADOMD.NET ships new versions, and the first-call contract
can move underneath us.

## Risks, stated rather than discovered later

- **`[MS-SSAS-T]` is open-ended.** Phase 0 exists to bound it before anyone
  commits to the rest. A plan that skipped straight to Phase 1 would be
  estimating a protocol nobody in this project has yet seen a byte of.
- **XMLA alone does not deliver semantic-link-labs.** It also wants `sempy`
  and the Power BI REST surface. The notebook runtime is done; those two are
  separate work, and finishing XMLA without them buys a transport with no
  caller.
- **The evaluator is bounded.** It answers the fixture's DAX, not all of DAX
  (`docs/24` tracks full DAX as its own open-ended L). An XMLA surface will
  reach queries it cannot answer, and the honest failure there is a fault the
  client can read — not an empty rowset, which reads as "no data" and is the
  same class of lie a clock-derived job status was.
