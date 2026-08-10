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

## The connect path, measured 2026-08-10

`AdomdConnection.Open()` now **succeeds** against a hand-written stub, for all
three `powerbi://` connection-string forms. Four gates stood between routing and
an open session, and each cost exactly one thing:

| Gate | What it needed | How it was found |
|---|---|---|
| Cluster resolution | `NameResolutionResult.clusterFQDN`, a BARE FQDN (the port is inherited from the Data Source) | reflection, then decompile |
| Session open | a SOAP `ExecuteResponse` plus a `Session` header carrying the id | XMLA reference |
| Response processing | the `x-ms-xmlacaps-negotiation-flags` header ON THE RESPONSE | the client's own error text |
| Transport framing | a trailing `0x00` byte after the envelope | decompile |

The first XMLA envelope the client sends is an `Execute` carrying
`BeginSession mustUnderstand="1"`, `Version Sequence="922"`, an EMPTY
`<Statement/>` and a `PropertyList`. **Opening a connection is a session
handshake, not a query.**

Two findings worth more than the gates themselves:

- **The trailing byte is a protocol, not padding.**
  `PaasInfraXmlaOperation.ReadResponsePayloadImpl` switches on the last byte of
  the response: `0` = complete, `1` = **continuation, reconnect under the LRO
  protocol**, anything else = `TransportProtocolError`. So long-running queries
  have a resume mechanism that any real implementation must support. That is a
  cost item nobody had counted, and building from the spec would have found it
  late.
- **Three shapes failing identically eliminated a variable rather than choosing
  one.** Screen 12 swept three cluster-URI forms and all three failed with
  `UriFormatException`. A bare hostname cannot fail to parse, so the string was
  never reaching the parser: the reply CONTRACT was wrong, not its shape.

## The query path, measured 2026-08-10

**A real ADOMD.NET client now round-trips end to end against a stub.** Past
`Open()`, the probe issues a DAX query and a schema `Discover`, and both are
accepted:

    PROBE powerbi-userid/dax    :: OK
    PROBE powerbi-userid/schema :: OK     SCHEMA rows=1

What the client sends, captured off the wire:

| Envelope | Contract when refused |
|---|---|
| `Execute` + `<Statement/>` empty | session open; `ExecuteResponse`, empty root |
| `Execute` + `EVALUATE ROW("x",1)` | `not a rowset` — `XmlaDataReader..ctor` |
| `Discover` + `MDSCHEMA_MEASURES` | `unrecognizable` — `SoapFormatter.ReadDiscoverResponse` |

**The inline XSD is the whole trick.** ADOMD.NET reads the schema to learn the
row shape BEFORE it reads a row, which is why an empty root came back as
"unrecognizable" rather than "empty": the reader never found the element it
expects. `Execute` and `Discover` take the SAME
`urn:schemas-microsoft-com:xml-analysis:rowset` payload under a different
wrapper element, so the harness factors one builder rather than two.

This half was built from `[MS-SSAS]`, not from the decompiler — the documented
surface, per the boundary above. That rule is now load-bearing rather than
aspirational.

## What SemPy actually needs, measured against the installed wheel

The demand question, settled by reading the package rather than the docs. Two
sessions had previously reached OPPOSITE conclusions, both from Microsoft Learn.

| sempy call | Transport | This emulator |
|---|---|---|
| `evaluate_dax` | **XMLA, always** — no `use_xmla` parameter exists; the body calls `get_dataset_client(mode=ConnectionMode.XMLA)` unconditionally | **the gap** |
| `evaluate_measure`, `read_table` | REST `executeQueries` (`use_xmla: bool = False`) | already 🟢 |
| `list_measures`, `list_tables` | XMLA, via **TOM** (`Microsoft.AnalysisServices.Tabular`) — NOT TMSCHEMA | **the gap** |
| `list_partitions`, `list_columns`, `list_relationships`, `list_hierarchies` | XMLA; `$SYSTEM.TMSCHEMA_*` appears in their modules | **the gap**, but see the wire evidence below |
| `INFO.*` | never called | no sempy case |

### What REAL sempy puts on the wire (measured 2026-08-10)

Everything above is read from sempy's source. This section is different: a
driver runs **real sempy 0.14.2 in a container** against a capture listener, so
these are bytes, not readings. 36 requests captured, 21 of them XMLA.

| count | call |
|---|---|
| 14x | `Execute` — session handshake (`BeginSession`, empty `<Statement/>`) |
| 6x | `Discover` — **`RequestType=DISCOVER_XML_METADATA`**, `Restrictions: ObjectExpansion=ExpandObject` |
| 1x | `Execute` — `<Statement>EVALUATE {1}</Statement>` (`evaluate_dax`) |

That run showed **zero `TMSCHEMA`** — but it was bounded, and the bound
mattered. Every `list_*` call ended at `ResponseFormatException` on a stubbed
session, so it measured only the FIRST XMLA call per function.

**Once the `Discover` is actually ANSWERED, TMSCHEMA appears:**

| count | call |
|---|---|
| 5x | `Execute` — **`TMSCHEMA_MODEL`** (a DMV query in a `<Statement>`) |
| 4x | `Execute` — session handshake |
| 2x | `Discover` — `DISCOVER_XML_METADATA` |

So the metadata surface is **BOTH mechanisms in sequence**, not a choice between
them: an ASSL `Discover` first, then TMSCHEMA.

**TMSCHEMA itself arrives TWO WAYS, and the SOAP verb hides the difference.**
Both are `Execute`; the payload grammar is not the same:

| issuer | payload |
|---|---|
| **TOM** (`list_measures`, `list_tables`) | `Execute` > `Command` > `<Batch>` of **~35** `<Discover RequestType=TMSCHEMA_*>`, restricted by `<DatabaseName>`. No SQL. |
| **sempy's Python** (`list_columns`, `list_partitions`, `list_relationships`, `list_hierarchies`) | SQL `SELECT [ID] AS [...] FROM $SYSTEM.TMSCHEMA_*` sent through `evaluate_dax` |

An earlier version of this section said only "the DMV leg arrives as an
`Execute`... so it shares the rowset path". True on the verb, and it misled a
second session into scoping a SQL parser as if that covered TOM as well.
Deleted rather than annotated.

TOM's batch asks for the whole catalogue in one round trip: `TMSCHEMA_MODEL`,
`_DATA_SOURCES`, `_TABLES`, `_COLUMNS`, `_PARTITIONS`, `_RELATIONSHIPS`,
`_MEASURES`, `_HIERARCHIES`, `_LEVELS`, `_KPIS`, `_CULTURES`, `_PERSPECTIVES`,
`_ROLES`, `_CALCULATION_GROUPS`, `_REFRESH_POLICIES` and twenty more. The
`*_STORAGES` DMVs are **absent** from it — they appear only in sempy's Python
SELECTs, which is consistent with backing `extended=True` paths.

**Unmeasured:** whether TOM REQUIRES all ~35 answered or tolerates empty rowsets
for the ones a model does not use. That decides whether a five-rowset
implementation suffices, and it is one cheap run away.

**This is why the earlier bound was written rather than the clean version.** A
"zero TMSCHEMA" headline would have scoped the work to one ASSL serialiser and
found the DMV requirement after building it.

`ObjectExpansion=ExpandObject` is **not** full expansion. Per `bi-shared-docs`
`assl-objects-and-object-characteristics.md`: `ExpandObject` returns the object
with its MINOR contained objects expanded, and contained MAJOR objects as
name/ID/timestamp stubs. Full expansion is `ExpandFull`. So the reply is a
projection at a named level, not the whole model.

### Gates a real client demands, each named by the client itself

Every one cost a run to find and none is guessable. In order:

1. **JWT signature segment must be valid base64url.** A raw marker string
   (`len % 4 == 1`) cannot decode at any padding; PyJWT raises
   `DecodeError: Invalid crypto padding`, sempy catches it into `exp = 0`, and
   it surfaces as **"The token has expired"** for a token that never expired.
2. **Routing reply is a BARE ARRAY**, not `{"value": [...]}`. An envelope fails
   as `ConnectionException: The specified Power BI workspace (...) is not found`.
3. **`capacitySku` is the enum value `Premium`**, not an SKU name. `"P1"` gives
   `ArgumentException: Requested value 'P1' was not found`.
4. **`capacityUri` needs >= 1 path segment.** A bare origin gives
   `IndexOutOfRangeException`.
5. **`POST /powerbi/databases/v201606/workspaces/{ws}/getDatabaseName`** with
   `{"datasetName": "...", "workspaceType": 1}`. **Discovered only by driving a
   real client** — a hand-written probe connects with a database name it already
   knows, so this call is invisible to it.
6. **The client dials `:443`** regardless of the port in the Data Source; the
   endpoint address is derived.
7. **Trailing `0x00`** after the SOAP body (`0` complete, `1` LRO continuation).
8. **`Discover` needs the inline XSD** — the schema is read before the rows.
9. **`DISCOVER_XML_METADATA`'s payload is ASSL**, embedded as XML rather than
   escaped, and its ROOT depends on the restriction list:
   `<DatabaseID>` absent -> `<Server>`; present -> `<Database>`. The wrong one
   gives `XmlSerializationException: Unexpected root 'Server' ... when trying to
   read 'Microsoft.AnalysisServices.Tabular.Database'`.

With 1-9 answered by a stub, `list_workspaces` and `list_datasets` both return
DataFrames from real sempy.

**The ASSL `<Database>` contract, read off `Microsoft.AnalysisServices.Tabular.dll`:**

    Database.CompatibilityLevel   XmlElement ns=.../analysisservices/2010/engine/200
    Database.Model                XmlIgnore  -> must NOT appear in the ASSL

`CompatibilityLevel` is namespace-qualified to `2010/engine/200` — not the
`2003/engine` base, and not `200/200`. `Model` is ignored on this path
entirely; the tabular model does not travel in the ASSL.

**Method note.** Three guesses at that shape produced three different
rejections. Reflecting on the shipped assembly answered each question in one
call. Technique 2 of the boundary above should have been the FIRST move, not the
fourth.

**Still open:** answering the `TMSCHEMA_*` DMV queries, which is the
`internal/semanticmodel` half.

    sempy/fabric/_flat.py:947      def evaluate_measure(          <- 954's owner
    sempy/fabric/_flat.py:954          use_xmla: bool = False
    sempy/fabric/_flat.py:1038     def evaluate_dax(...)          <- no use_xmla
                                       get_dataset_client(mode=ConnectionMode.XMLA)
    sempy/fabric/_client/_pbi_rest_api.py:274
        path = f"v1.0/myorg/datasets/{dataset_id}/executeQueries"

**Three sessions got this wrong before it was executed**, and the same way each
time. `_flat.py:954` was cited as `evaluate_dax`'s default; it is
`evaluate_measure`'s. Each reader confirmed the LINE EXISTED and had the quoted
text, and none checked which `def` it sat inside — **the citation was verified,
the proposition was not.** What settled it was running the thing:
`evaluate_dax(..., use_xmla=True)` raises
`TypeError: unexpected keyword argument 'use_xmla'`, and a parameter that does
not exist cannot have a default. Prefer executing a claim about behaviour over
reading any number of sources about it.

XMLA is escalated to only on `use_xmla=True`, a readwrite connection, or
`num_rows > 30000`. sempy also ships `Microsoft.AnalysisServices.AdomdClient.dll`
— the same assembly `e2e/xmla` drives, so the oracle and the consumer are the
same client.

**Consequences, and they narrow the work:**

1. The sempy gap is the **DMV metadata surface plus `evaluate_dax`**. Narrower
   than "implement `[MS-SSAS-T]`", but larger than the DMV-only version this
   document carried for one revision — and it points at exactly the `Execute`
   rowset this harness now serves.
2. The next rowset to build is `$SYSTEM.TMSCHEMA_*`, NOT the `MDSCHEMA_*` this
   harness probed first. `MDSCHEMA_*` proved the shape; `TMSCHEMA_*` is the
   demand.
3. An `INFO.*`-over-`executeQueries` plan was proposed and dropped: `INFO.*` may
   still be worth building for DAX Studio and Desktop, but NOT as a sempy path.
4. `StaticFabricContext(pbi_shared_host=…)` redirects sempy standalone and
   yields the exact `powerbi://host:port/v1.0/myorg/ws` form this harness proves
   works — the most credible third-party witness lead for a 🟢 row.

**Method note worth keeping.** Both sessions read Learn's `use_xmla=False` and
drew opposite conclusions; one grep of the installed wheel settled it.
Documentation about a transport is not a transport.

## What is NOT settled

Reachability and the wire shape were settled first, then the implementation.
This section is rewritten rather than annotated, because a plan doc that still
lists finished work as open misprices the next increment.

**Settled since, each by a client rather than by reading:**

- **Rows that mean something.** `$SYSTEM.TMSCHEMA_*` and the `Discover` batch
  are served from `internal/semanticmodel`. Real sempy reads 3 tables, 6
  measures, 12 columns and 3 partitions off the seeded model in CI.
- **The TOM surface.** TOM materialises a Database from the ~35 element
  `<Discover>` batch. `semantic-link-labs` 0.17.0 reads that same model through
  `connect_semantic_model(readonly=True)` and gets the same counts, which is a
  second reader on the surface rather than a second run of the first.
- **What labs needed was NOT XMLA.** Measured 2026-08-10: labs failed at
  `resolve_workspace_name` with `ModuleNotFoundError: No module named
  notebookutils`, before reaching XMLA at all. It calls
  `notebookutils.credentials.getToken("pbi")` for every REST call it makes
  (`_helper_functions._base_api`). Wiring this repo's own shim from `python/`
  into the harness was the whole fix. The risk note below predicted "XMLA alone
  does not deliver labs, it also wants the Power BI REST surface"; measurement
  narrowed that to the notebook runtime shim specifically.
- **Type fidelity for DAX results.** Columns were all declared `xsd:string`,
  and dtypes come from the inline schema rather than the cell text, so sempy
  handed back string columns for a model declaring int64 and arithmetic on a
  measure concatenated. `FromDAX` now declares a type per column, inferred from
  the values because this evaluator has no DAX type system. Asserted end to end
  by the dtypes check in `e2e/sempy`.

**Still open, and deliberately not claimed:**

- **TMSL writes.** `<Alter>` / `<Create>` / `<Refresh>`, which is what
  `connect_semantic_model(readonly=False)` and Direct Lake migration need. The
  read path does not foreclose it and does not deliver it.
- **MDX.** No client in this repo has issued any.
- **The LRO continuation protocol.** The trailing byte can be `1`, meaning
  reconnect and resume. Long-running queries need it; nothing here implements
  it.
- **Errors.** Only success paths have been driven. What a client does with a
  SOAP fault mid-rowset is still unknown.

**The `L` in `docs/24` has been re-priced to `S`**, with the reasoning kept
there rather than the number quietly swapped. It was right about the shape and
wrong about the multiplier: the DMV/TOM surface was indeed the cost, but each
gate cost one measured thing rather than a design.

## Method, and its boundary

Three techniques are in use here and they are NOT equivalent. Calvin's call,
2026-08-10, is that all three stay available, with decompilation as a diagnostic
of last resort:

1. **Wire capture** — what the client sends. Always preferred.
2. **Reflection** over the shipped assembly's `[DataContract]` / `[DataMember]`
   names. Public type metadata of a redistributable, closer to reading a header
   file than reading source. This is what `e2e/xmla/contract/` does, and it is
   the only one of the three with committed tooling.
3. **Decompilation** of method bodies, when capture and the published references
   both come up empty.

**The boundary, and why it exists.** This repo claims to be a CLEAN-ROOM
implementation, and that claim is load-bearing for every parity grade. So:

- **No decompiled source is ever committed**, in any form, including quoted in
  comments or docs. Verify with
  `git log --all --diff-filter=A --name-only | grep -iE "ilspy|decompil"` —
  it must stay empty.
- **What may be recorded is a PROTOCOL FACT**, phrased as a statement about the
  wire ("the response must end with `0x00`"), never as an implementation.
- **Decompile only after capture and the references have failed.** Both
  decompiler findings above are ones no published reference contains, because
  they are Power BI PaaS transport glue rather than XMLA.
- **Prefer references for the documented surface.** Rowsets and `Discover` are
  specified in `[MS-SSAS]` and implemented by third-party clients such as
  `RadarSoft/xmla-client`; reaching for the decompiler there would be choosing
  the riskier source over the safer one for no gain.

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

### Screen 7 — RUN: **PHASE 0 IS ANSWERED.** The client advances past routing

A ladder of `capacityUri` path depths, plus a host-label arm. Depth was the
variable the failure mode pointed at — `IndexOutOfRange` means indexing past
the end, so the thing to sweep is length, not URL format.

| rung | outcome |
| --- | --- |
| `L0` — 0 segments | `IndexOutOfRangeException`, 1 request |
| `L1`–`L5` — 1 to 5 segments | **ADVANCED**, 7 requests |
| `H5`, `H7` — 5 and 7 host labels | identical to `L0` |

**One path segment is enough**, and the host-label hypothesis is dead. That arm
earned its two containers: screen 6's hosts had 3 and 4 dot-separated labels,
so a parser wanting label 5 would have fitted every observation to that point.
Retiring a live alternative is worth as much as confirming the leading one.

With a segment present, `TryResolvePbiWorkspace` leaves the stack entirely, the
`PbiPremiumAuthenticationHandle` **constructs** — `workspaceObjectId` and
`capacityObjectId` both derived — and the client makes a call nobody in this
project had ever seen:

```
GET  /powerbi/databases/v201606/workspaces?PreferClientRouting=true   (once)
POST /metadata/v201606/generateastoken?PreferClientRouting=true       (per connection)
POST /metadata/v201606/generateastoken
```

throwing at `PbiPremiumAuthenticationHandle::GetMwcToken` on the stub's reply.
Note the **500 is ours** — the capture stub answers every POST with a SOAP
fault — so that is the harness refusing, not the client failing. Note also that
routing is resolved ONCE for the process while the token is fetched PER
CONNECTION: the two have different cache lifetimes.

**A harness regression, and what it cost.** Restructuring the loop for
per-shape runs narrowed each run's record to `(method, path)`, dropping the
body print the original had. So the `generateastoken` body — the first sight of
anything past routing, and the entire point of getting there — was captured and
discarded, costing a full extra run to recover. The record now keeps whole
requests and prints headers and body in full, with `authorization` and `cookie`
reduced to a length.

### Screen 8 — RUN: the token request's contract, and the segment rule

Three rungs with DISTINCT GUIDs per path position, so the value the client
echoes names the index it read.

| rung | segments offered | `capacityObjectId` sent |
| --- | --- | --- |
| `L1` | GUID-1 | `11111111-…-111111111111/` |
| `L2` | GUID-1, GUID-2 | `11111111-…-111111111111/` |
| `L5` | GUID-1 … GUID-5 | `11111111-…-111111111111/` |

Always the FIRST segment, at every depth, retaining a TRAILING SLASH.

**Inferred, but from a fingerprint rather than a fit.** That is precisely
.NET's `Uri.Segments`, which yields `["/", "seg1/", "seg2/", …]` with each
segment carrying its slash — so `capacityObjectId = new Uri(capacityUri)
.Segments[1]`. The same rule explains `L0` without adjustment: an empty path
gives `Segments` length 1, and `Segments[1]` throws `IndexOutOfRangeException`.
One rule, four observations, no free parameters.

The request the emulator must now answer:

```
POST /metadata/v201606/generateastoken[?PreferClientRouting=true]
content-type: application/json        user-agent: ASClient/.NET-Core
authorization: Bearer <token from the connection string>

{"applyAuxiliaryPermission":false,"auxiliaryPermissionOwner":null,
 "bypassBuildPermission":false,
 "capacityObjectId":"<first path segment of capacityUri, trailing slash>",
 "datasetName":null,"intendedUsage":0,"sourceCapacityObjectId":null,
 "workspaceObjectId":"<the id sent in Workspace201606>"}
```

`workspaceObjectId` is echoed from the routing reply, so the emulator controls
it. `datasetName` is null because the probe's Data Source carries no
`Initial Catalog`; that is where a database name would appear.

### The token read — DONE: `generateastoken`'s reply is one field

Read off the same assembly (19.84.1.0) rather than screened, because the
routing read had already shown that beats guessing. `GetMwcToken` returns
`PbiPremiumAuthenticationHandle+MWCToken`, and the contract is:

```
MWCToken
    Token : string        [DataMember] ×1
```

One member, PascalCase. **The emulator's reply to `generateastoken` is
`{"Token":"<string>"}`** — and the token is one the emulator mints, so its
contents are ours to choose.

**A free check on the method.** The same read dumped the *request* contract:

```
MWCASTokenRequest
    capacityObjectId · workspaceObjectId · datasetName · applyAuxiliaryPermission
    auxiliaryPermissionOwner · bypassBuildPermission · intendedUsage
    sourceCapacityObjectId
```

That matches screen 8's captured wire body field for field. The declared
contract and the observed request agree, so the reflection route is confirmed
against a measurement rather than trusted on its own — which is the check the
routing read never got.

The read is now a tool rather than an ad-hoc run:
[`e2e/xmla/contract`](../e2e/xmla/contract/README.md). `run.py [substring]`
prints any matching `[DataContract]`'s member names. Reach for it *before*
screening a payload shape: a screen is right when the client's behaviour is
unknown, wrong when the answer is already written in an assembly on disk.

### Screen 9 — RUN: the token is accepted, and the client leaves for :443

Serving `{"Token":"emulator-mwc-token"}` — the contract read above, not a
hypothesis — against the `L1`/`L2`/`L5` capacity-path rungs.

**Observed.** The token was served three times (once per rung). Every
connection then failed with

```
SocketException :: Connection refused  host.docker.internal:443
```

thrown from `XmlaClient.OpenConnection` → `HttpStream.Create` →
`ConnectionInfo.ResolveHTTPConnectionPropertiesForPaaSInfrastructure`.

**What that establishes.** The token is accepted — nothing rejected it, and the
client proceeded to open its XMLA connection. It went to the routing host on
**port 443**, discarding the port named in the Data Source. That is the first
time the client has tried to reach anything other than the listener it was
pointed at, and it exercises the hook `docs/32` recorded as untested:
`pbiDedicatedRolloutFqdn`. With no rollout FQDN in the reply, the fallback is
host-from-routing plus the default HTTPS port.

**The consequence for Phase 1.** The XMLA endpoint's address is *derived*, not
taken from the connection string, so a suite cannot reach it by pointing the
Data Source somewhere. Either the reply must carry a host:port the client will
honour, or the listener must occupy 443. The next screen should vary
`capacityUri`'s **authority** — it already carries the path segment the client
reads as `capacityObjectId`, and its host is the obvious candidate for the one
it dials.

**Not established.** Whether the client would accept a port in that authority
at all, and whether `pbiDedicatedRolloutFqdn` is settable from any field in
`Workspace201606` (the contract has five members, and `capacityUri` is the only
one shaped like a host). No run has varied it.

### Screen 10 — RUN: a call nobody knew existed, and 443 is reachable after all

Screen 9 left the client dialling `:443`, which looked like a hard constraint:
the harness cannot bind a privileged port. It is not — **the Docker daemon
publishes 443 without sudo**, and socat piping 443 to the capture listener
keeps TLS terminating on the same self-signed cert. A door, not a proxy.

With that door open the client got further and revealed a request this plan
had never recorded:

```
POST /webapi/clusterResolve
content-type: application/json      host: host.docker.internal   (portless — it came in on 443)

{"databaseName":null,"premiumPublicXmlaEndpoint":true,
 "serverName":"11111111-1111-1111-1111-111111111111/"}
```

`serverName` is the `capacityObjectId` — the first `capacityUri` path segment,
trailing slash intact, exactly as screen 8 established. `databaseName` is null
because the Data Source carries no `Initial Catalog`. The call appears **twice
per connection**: once before routing and once after the token.

**This also retired a screen before it was run.** The plan was to vary
`capacityUri`'s authority to test whether a port is honoured. Checking first
showed the shape is built from the request's `Host` header, so `capacityUri`
already carried `:18446` — and the client dialled `:443` regardless. The port
is not unhandled, it is **discarded**, and the screen would have spent several
containers re-deriving that.

### Screen 11 — RUN: the client asks us where to go, and takes an answer

`PowerBIClusterResolutionResult` — read from the assembly with
[`e2e/xmla/contract`](../e2e/xmla/contract/README.md), one run, no screening:

```
FixedClusterUri · DynamicClusterUri · NewTenantId · RuleDescription · TTLSeconds
```

That is the steering hook open since screen 5. The client does not derive its
XMLA endpoint unilaterally — **it asks, and we answer.**

Answering with `{"FixedClusterUri":"https://host.docker.internal:18446", …}`
moved the failure again:

| screen | reply to clusterResolve | client's answer |
|---|---|---|
| 10 | SOAP fault (the stub's refusal) | `NotSupportedException :: Specified method is not supported` |
| 11 | the declared contract, URIs as full URLs | `AdomdConnectionException :: A connection cannot be made` |

**Consumed, then rejected.** `NotSupportedException` was the client refusing to
parse a SOAP body where JSON belongs; it is gone, so the contract is right. The
new throw is inside `ASAzureUtility.ResolvePaaSConnectionEndpointDetail` — the
same method that posts `clusterResolve` — and **no request reached the listener
afterwards**. So this is resolution rejecting the value, not a failed dial.

**The next screen, and it is one variable.** The URIs were sent as full URLs.
If the client prefixes a scheme itself, `https://https://…` would fail exactly
like this without a socket ever opening. Send `FixedClusterUri` as a bare
`host:port`, and as a bare host, in one run each. The discriminator is already
known: a value the client accepts produces a request at the listener, and that
request is the first XMLA/SOAP envelope this project has seen.

### Where this leaves the search

Phase 0's question — "what does the client ask after routing?" — is **answered**.
The roadmap's largest unknown is now a recorded request with a known body.

~~Next is `generateastoken`'s RESPONSE contract~~ — **done**, see the token
read above: `{"Token":"<string>"}`. "MWC" is the token the client carries into
the XMLA calls themselves, so answering this is the boundary where Phase 0 ends
and Phase 1 has a client to talk to.

**The next unknown is therefore what the client does with a token it accepts.**
Nothing in this project has seen a request past `generateastoken`, and that is
the first XMLA/SOAP call — the point where `[MS-SSAS-T]` stops being a paper
spec and becomes a recorded sequence. The step is small: serve
`{"Token":"…"}` from the capture stub and record what arrives next. Whether
`pbiDedicatedRolloutFqdn` can then steer that call at a host of our choosing
remains untested and is the other half of the same run.

`pbiDedicatedRolloutFqdn` — the other value `TryResolvePbiWorkspace` derives —
remains the hook for steering the client's later calls at a host of our
choosing. It has not been exercised: every request so far has come back to the
listener named in the Data Source.

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
