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

**What this establishes.** Removing the envelope changes the failure from a
lookup to a *null argument*, so the client requires it — and PascalCase
`Value` was accepted where a bare array was not, which says the deserialiser
is case-insensitive on the envelope. With the envelope present the client
parses, searches, and reports not-found against **ten** spellings at once
(`name`, `workspaceName`, `displayName`, `folderName`, `id`, `objectId`,
`capacityObjectId`, `clusterUri`, `fixedClusterUri`, `backendUri`). Field
spelling is therefore **not** the remaining variable.

**What it does NOT establish, stated because the correlation is tempting.**
`ArgumentNullException (Parameter 'value')` is the standard .NET shape for
`ArgumentNullException(nameof(value))` and may name a *method parameter*, not
our JSON key. The correlation is suggestive — the only shape lacking a `value`
key produced the only null error — but causation is unproven, and the cheap
discriminator is `{"value": null}` versus `{"value": []}` versus a bare array.
Nobody should build on the envelope claim without running that.

**Where this leaves the next hypothesis: structural, not cosmetic.** The client
matches the Data Source's workspace name against something that is not any
plain string field on the workspace object. Candidates worth one run each:
a nested object (the name under a `properties`/`workspace` sub-object), a
different top-level key than `value`, or a match against something derived
rather than carried — in which case no field spelling would ever have worked
and the ten-spelling result is the evidence for it.

### The next experiment, now one edit and ~5 minutes

Vary a single field at a time against the same harness and read the error. The
client's message names the workspace it wanted, so a matching reply advances
past routing for the first time — and that is the point at which
`[MS-SSAS-T]`'s real size becomes observable rather than estimated.

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
