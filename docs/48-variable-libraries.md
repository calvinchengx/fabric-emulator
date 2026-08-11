# 48 — Variable Libraries and their consumers

[47 — what must not be in data-engineering code](47-environment-abstraction.md)
says every value that differs between environments must be resolved *outside*
the artifact. A Variable Library is **Fabric's own answer to that rule**, and
this doc is about consuming one — from a Data Pipeline, from a notebook, and
what blocks the remaining consumers.

The shape of the answer matters: a pipeline does not embed the value and does
not embed a pointer to a workspace either. It embeds a *name*, and the
workspace resolves it. So the same `pipeline-content.json` deploys to DEV, QAT
and PROD unchanged and yields a different value in each — the `diff` test in 47
passes by construction.

## Why this doc exists at all

**The public documentation does not publish the wire format.** Both
[the pipeline integration article][pipeline-doc] and
[the variable library overview][overview-doc] describe the UI in screenshots
and stop there: no expression syntax, no JSON. The JSON schemas for the library
*definition* are public; the pipeline-side *declaration* is not documented
anywhere.

So the shapes below were **captured from a live tenant** on 2026-08-10 rather
than guessed: the binding was made in the Data Factory designer, and the
designer's own output read back through **View → Edit JSON code**. This repo's
rule is that an unpublished wire name is refused rather than invented, and the
capture is what lifted the refusal.

That rule earned its keep here. An earlier API-only probe guessed a declaration
keyed by variable name carrying a `variableLibraryObjectId`, and Fabric
**silently dropped the whole key** on write while preserving its siblings.
Nothing failed; the pipeline simply had no declaration. Every part of that
guess was wrong, as the capture below shows.

[pipeline-doc]: https://learn.microsoft.com/en-us/fabric/data-factory/variable-library-integration-with-data-pipelines
[overview-doc]: https://learn.microsoft.com/en-us/fabric/cicd/variable-library/variable-library-overview

## The library definition

The item's definition is one part per file:

```
variables.json          declarations and default values
settings.json           value-set ordering (presentation only)
valueSets/<name>.json   one alternative set, overriding a subset
```

Captured verbatim from the live tenant via `getDefinition` (see
[Reading a definition](#reading-a-definition-from-a-tenant) for how):

```json
// variables.json
{
  "$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/variables/1.0.0/schema.json",
  "variables": [
    {"name": "bronzePath", "note": "env-invariant relative path", "type": "String", "value": "Files/bronze"},
    {"name": "runId", "note": "", "type": "Guid", "value": "11111111-2222-3333-4444-555555555555"},
    {"name": "silverNotebook", "note": "", "type": "ItemReference",
     "value": {"itemId": "3f33c8a7-…", "workspaceId": "fd6cc69d-…"}}
  ]
}
```

Note `note` is emitted as `""` rather than omitted, and that the library's type
for a reference is **`ItemReference`** — the *pipeline* declaration for the same
variable says `Object`. Two vocabularies, and each side must be read on its own
terms.

```json
// valueSets/prod.json
{
  "$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/valueSet/1.0.0/schema.json",
  "name": "prod",
  "variableOverrides": [{"name": "bronzePath", "value": "Files/bronze-prod"}]
}
```

Two details that shape the implementation:

**A value set is a PARTIAL override.** It lists only what differs; everything
else keeps the default from `variables.json`. That is why a value set is not
simply a second copy of the variable list, and why resolution is a merge.

**A value set's identity is the `name` inside the file, not the filename.**
`activeValueSetName` names the *set*, and only the file can say what a set is
called. The emulator keys on the declared name and falls back to the filename
only when the file omits it.

### The active value set is not in the definition

`settings.json` carries `valueSetsOrder` and nothing else. The active set is
the **item property** `activeValueSetName`, set by `PATCH` on the item:

```http
PATCH /v1/workspaces/{workspaceId}/variableLibraries/{variableLibraryId}
{"properties": {"activeValueSetName": "prod"}}
```

This is the load-bearing separation. If the active set lived in the definition,
a git branch would carry it and promoting DEV to PROD would promote DEV's
choice of environment along with it. Keeping it as per-workspace state is what
makes one definition serve every stage.

## The pipeline-side declaration

Captured, not inferred. Under `properties`, a sibling of `activities`:

```json
"libraryVariables": {
  "emuProbeVarLib_bronzePath": {
    "type": "String",
    "variableName": "bronzePath",
    "libraryName": "emuProbeVarLib"
  }
}
```

and the expression the designer emits for it:

```
@pipeline().libraryVariables.emuProbeVarLib_bronzePath
```

### The lookup key is the alias

**The map key is what the expression resolves against — not `variableName`.**
The designer defaults the key to `<libraryName>_<variableName>` but exposes it
as an editable free-text field, so it must be used exactly as given and never
reconstructed by concatenation.

This is worth stating loudly because it is invisible until it bites. Real
Fabric's own diagnostics for getting it wrong:

* no declaration at all →
  `property 'bronzePath' doesn't exist, available properties are ''`
  — note the empty list. `libraryVariables` exists as an object even when the
  pipeline declares none, so an undeclared alias must fail on the *member*, not
  on `libraryVariables` itself. The emulator matches this.
* declaration present, expression naming the variable instead of the alias →
  the designer refuses to save: `Parameter bronzePath was not found under
  emuProbePipeline`.

### Binding is by name, with no GUID anywhere

`libraryName` and `variableName` are names. No workspace id, no item id, no
`variableLibraryObjectId`. **This is the portability result**: a pipeline that
consumes library variables moves between workspaces with no GUID rewriting,
because it resolves against whichever library of that name the target workspace
holds. In 47's taxonomy the reference itself is environment-invariant, while
the value it resolves to is environment-bound — which is exactly the split that
doc asks for.

### The declared type is the pipeline's vocabulary

`type` on the declaration is the **pipeline** type, not the library's. The
integration article maps them: *"Boolean as `Bool` type, Datetime as `String`
type, Guid as `String` type, Integer as `Int` type"*, and Number is not
supported in pipelines at all. The library remains authoritative for the value;
the declaration's `type` is a restatement.

The library's own type list, read off the New-variable dropdown (2026-08-10):

| Library type | Group | Pipeline `type` | How known |
|---|---|---|---|
| `String` | Basic | `String` | captured |
| `Guid` | Basic | `String` | **captured** |
| `DateTime` | Basic | `String` | **captured** |
| `Integer` | Basic | `Int` | **captured** |
| `Boolean` | Basic | `Bool` | **captured** |
| `Number` | Basic | *(unsupported in pipelines)* | **captured** |
| `Item reference` (preview) | Other | **`Object`** | **captured** |
| `Connection reference` (preview) | Other | `Object`, presumed | **value captured**, pipeline `type` still not |

### The library's type list, captured with a control (2026-08-11)

The remaining rows above were filled by creating one library carrying a variable
of each candidate type **plus one deliberate nonsense type**, and reading the
failed operation:

```
InvalidVariableType: The variable type 'TotallyMadeUpType' is not supported.
```

The control is the load-bearing part. Sending only plausible types and watching
them succeed proves nothing — a create that ignores `type` accepts those too,
and `getDefinition` would echo our own guesses back as if the tenant had
confirmed them. The published schema cannot settle it either: it constrains
`type` to `^[A-Za-z][A-Za-z0-9]{0,63}$` and declares `value: true` (any JSON).

Stored values, read back with `getDefinition`:

```json
{"name": "aDateTime",  "note": "", "type": "DateTime",  "value": "2026-08-11T00:00:00Z"},
{"name": "anInteger",  "note": "", "type": "Integer",   "value": 42},
{"name": "aBoolean",   "note": "", "type": "Boolean",   "value": true},
{"name": "aNumber",    "note": "", "type": "Number",    "value": 1.5},
{"name": "aConnection","note": "", "type": "ConnectionReference",
 "value": {"connectionId": "7feee8f6-0545-4647-a24f-c6f98693aea5"}}
```

`Integer` and `Boolean` are stored as JSON number and JSON bool, not strings —
so a resolver passing values through unchanged is right for these as it is for
`Guid`. `Number` is accepted by the **library** even though pipelines cannot
consume it; the two vocabularies are separate, and this is the library's list.

### A connection reference is a GUID, and it is checked

`ConnectionReference`'s value is `{"connectionId": "<guid>"}`, and the tenant
**verifies the connection resolves** at definition-write time. A made-up id is
refused:

```
InvalidContent (issue: InvalidValueOrTypeMismatch)
Item content cannot be used (ReferencedEntityNotFoundOrAccessDenied)
```

This does **not** contradict [Binding is by name, with no GUID
anywhere](#binding-is-by-name-with-no-guid-anywhere) — that result is about the
*pipeline's reference to the library*, which remains `libraryName` +
`variableName`. What this adds is the other half of 47's split: the reference is
environment-invariant, and here is a **value** that is environment-bound as
hard as a value can be. A library carrying a connection reference cannot be
promoted between workspaces unchanged; the id must exist on the far side, or a
value set must override it per environment.

`ItemReference` behaves the same way — its value is `{itemId, workspaceId}`.
The pattern is that Fabric's *reference-typed values* are GUID pairs while its
*bindings* are names.

Two of these are worth calling out because a reasonable person would guess them
wrong:

**A `Guid` variable declares `"type": "String"`.** Captured from
`emuProbeVarLib_runId`. The library value is a JSON string, so the value needs
no conversion — the resolver passing it through unchanged is correct, and this
is the evidence for that rather than an assumption.

**An `Item reference` variable declares `"type": "Object"`.** The pipeline's
Library-variables grid *displays* `ItemReference`, but the saved JSON says
`Object`:

```json
"emuProbeVarLib_silverNotebook": {
  "type": "Object",
  "variableName": "silverNotebook",
  "libraryName": "emuProbeVarLib"
}
```

The UI label and the wire name differ, so the grid is not a safe source for
this field. Note also that both reference types are marked **(preview)**.

## How the emulator resolves

`internal/varlib` parses a definition and merges the active value set over the
defaults. `internal/api` finds the library **by display name, case-insensitively**
(Fabric documents library names as not case sensitive), reads
`activeValueSetName` off the item, and resolves every declaration before the
run starts. `internal/pipeline` exposes the results as
`@pipeline().libraryVariables.<alias>`.

Resolution happens **up front, not lazily**. A reference that cannot be
resolved fails the whole run with `PipelineLibraryVariableUnresolved` rather
than failing at whichever activity happens to read it first, and rather than
resolving to blank. Blank is the dangerous outcome: a bronze path that silently
becomes `""` writes to the wrong place and succeeds while doing it.

### Falling back to the defaults is REQUIRED, not lenient

An **unknown** active-set name resolves to the defaults instead of failing.

This was first written as the single deliberate guess in the feature, then
downgraded to an observation. It is neither. **It is a correctness
requirement**, and the tenant says so directly:

```
GET /v1/workspaces/{ws}/variableLibraries/{id}
  "properties": { "activeValueSetName": "Default value set" }

settings.json  ->  { "valueSetsOrder": ["qat"] }
parts          ->  variables.json, settings.json, valueSets/qat.json, .platform
```

`activeValueSetName` is literally **`"Default value set"`** — a name with **no
file under `valueSets/`** and **absent from `valueSetsOrder`**. That is the
out-of-the-box state of every Variable Library.

So an implementation that treats "active set matches no file" as an error would
fail **every library in its default configuration**. The rule is not tolerance
for a typo; it is the only behaviour that works. Recording the reasoning's
history here on purpose: it went guess → observation → requirement, and only
the last one is load-bearing.

### A value set overrides a SUBSET, even when the UI suggests otherwise

`valueSets/qat.json` overrides `bronzePath` **and nothing else**, though the
library editor displays a value for every variable in the `qat` column. Those
other cells are the defaults being *shown*, not overridden — setting the item
reference in the default set made it appear under `qat` too, and no override
was written. Resolution is therefore a merge over the defaults, and reading the
UI as "a value set is a full second copy" would be wrong.

## Evidence

`TestVariableLibraryResolutionE2E` (`internal/server`) publishes a library and
a pipeline over real HTTP, runs it, and asserts the resolved value by making
the pipeline **fail** when the value is not the expected one. It then flips
`activeValueSetName` with a single PATCH, changing neither definition, and
asserts the same pipeline now resolves the prod value — the environment switch,
demonstrated rather than described. It also asserts the negative case, because
a check that cannot fail witnesses nothing.

The suite was mutation-tested: dropping resolution entirely, keying by variable
name instead of alias, and ignoring value-set overrides each turn it red.

## Not captured, and therefore not implemented

* **Connection-reference variables.** Only `ItemReference` was captured. Its
  value is `{"itemId", "workspaceId"}` and it now round-trips; a connection's
  value is presumably a connection id, but presumably is not captured.
* **Alias characters illegal in a property path.** Whether Fabric rejects such
  an alias at save time or escapes it is unknown.
* **`Int`, `Bool` and `DateTime` declarations.** Taken from the article's
  mapping table, not observed. `String`, `Guid` and `Object` are captured.
* **Consumers other than pipelines, notebooks and user data functions.** See
  [the reachable ceiling of each](#the-other-consumers-and-what-blocks-them)
  below — three of them are blocked upstream rather than merely unbuilt, and
  the distinction decides whether effort would produce anything.

## The notebook consumer

`notebookutils.variableLibrary` is the other half of the same mechanism, and
unlike the pipeline surface **Microsoft publishes this API in full** — so the
shim follows the reference rather than a capture:

```python
lib = notebookutils.variableLibrary.getLibrary("envLib")
lib.bronzePath            # property access
lib.getVariable("bronzePath")
lib["bronzePath"]

notebookutils.variableLibrary.get("$(/**/envLib/bronzePath)")
```

It resolves the same way the pipeline side does — the active value set merged
over the declared defaults — and is built entirely on the public item APIs
(list, `getDefinition`, and the item's `activeValueSetName`), so it needs no
emulator-specific route and the same code path works against real Fabric.

### Two documented rules that are easy to soften, and are not

**The `/**/` prefix is required.** A reference without it is refused rather
than quietly accepted, because accepting a shorter form would let a notebook
work here and fail in Fabric — the exact asymmetry this repo exists to remove.

**Names are CASE-SENSITIVE here, and that DIFFERS from pipelines.** The
notebook reference says *"Variable and library names are case-sensitive. Use
exact name matching"*, while the pipeline article says the library name is
*not* case sensitive. Same library, two consumers, two matching rules. Both are
implemented as documented, so a name that resolves in a pipeline can
legitimately fail in a notebook. Making them agree would be tidier and wrong.

### It follows the 202

`getDefinition` has two documented outcomes and a real tenant answers 202, so
the shim polls the operation rather than reading the 202 body — which would
yield `null` and present as an **empty library** instead of an error. With
`FABRIC_FORCE_LRO` the emulator produces that shape too, so this path is
exercised locally rather than trusted.

### Not covered by the shim

Cross-workspace access (unsupported by the API itself, so there is no
`workspaceId` argument), writes (libraries are read-only from notebooks), and
the Scala and R bindings.

## Reading a definition from a tenant

Worth writing down because it cost two sessions a detour. `getDefinition`
answers **401 `UserNotLicensed`** when the token is minted for the wrong
tenant. That reads like a licensing problem and is not one — it is `az`
defaulting to the signed-in user's *home* tenant. Name the tenant explicitly:

```bash
az account get-access-token --tenant <fabric-tenant-id> \
  --resource https://api.fabric.microsoft.com --query accessToken -o tsv
```

Then note the call is a **202 plus a long-running operation**, not a 200: poll
the `Location` header until `Succeeded`, then fetch `<operation>/result`. A
client that reads the 202 body gets `null`, which is how this first looked like
an empty definition rather than an async one.

The portal is the alternative and needs no token: a Data Pipeline's
**View → Edit JSON code** is the authoritative definition JSON. There is no
equivalent in the Variable Library editor, and that editor's own network calls
are invisible to a tab-level listener because the workload runs in a
cross-origin iframe — so for a *library*, the REST route above is the only way.

## The other consumers, and what blocks them

Fabric lists six consumers of a variable library. Two are implemented here and
witnessed; one more is implemented as far as it can be. **The remaining three
are blocked upstream, not merely unbuilt** — that distinction matters, because
"not implemented" invites someone to go and implement it, and for these there
is nothing to implement against.

Recorded with citations so nobody re-derives it.

| Consumer | Status | Ceiling |
|---|---|---|
| **Data pipeline** | Implemented, witnessed by `fabric-cicd` | — |
| **Notebook** (NotebookUtils) | Implemented | — |
| **User data function** | Client shape implemented; **no runtime** | 🟡 body testable, item not executable |
| **Shortcut** | Not implementable | 🔴 no REST surface exists |
| **Copy job** | Not implementable usefully | 🔴 parameterises what this emulator refuses |
| **Dataflow Gen2** | Not implementable | 🔴 no engine to attach |

### User data functions — the one with a published API

The programming model documents it in full: a `@udf.connection` for the library
item, an argument typed `fn.FabricVariablesClient`, then `getVariables()` and
either `variables["name"]` or `variables.get("name")`.

`notebookutils.variableLibrary.FabricVariablesClient` matches that shape, so a
function body ports with one import line. **It deliberately does not shadow
`fabric.functions`**: `fabric-user-data-functions` is a real installable PyPI
package, and providing a module of that name would override something a user
may have installed. `notebookutils` is a different case — Microsoft ships that
as an import-only stub outside the Fabric runtime, which is why this repo makes
it work.

What this does NOT give you: the emulator has **no user-data-function runtime**
(`UserDataFunction` is a registered item type and nothing more), so a function's
*body* is testable against a real library while the *item* still does not
execute.

### Shortcuts — Fabric itself has no API for it

> *"Currently, variable libraries are the supported option for assigning
> shortcut variables across environments. Other assignment methods, **including
> REST API assignment, aren't supported**."*
> — [Assign variables to shortcuts][shortcut-doc]

Assignment is UI-only. There is no request a client could send, no JSON to
capture from a definition, and therefore nothing for a REST emulator to
implement. This is not a gap in the emulator; it is the shape of the feature
today. It becomes implementable the moment Fabric ships an API for it.

### Copy job — it parameterises the one thing this emulator refuses

The Copy job integration is **connection parameterisation**: the source and
destination *connection IDs* come from the library, so each stage injects its
own. The article's own framing is "externalizing connection values", and the
linking is done in the Copy job UI with no JSON published.

The emulator runs Copy jobs, but only their **OneLake legs** — external sources
and destinations are refused by name (`CopyJobExternalSourceNotSupported` /
`…DestinationNotSupported`, see `docs/parity.md`). A connection id is exactly
what those refusals are about. So even with a captured shape, the parameter
would select between connections the emulator declines to use, and a witness
could not assert anything an external connector isn't already blocking.

Reachable ceiling: it moves when external connections do, not before.

### Dataflow Gen2 — no engine exists to attach

Dataflow refresh, publish and in-pipeline execution already fail with
`DataflowEngineNotImplemented`, because no open Power Query M engine exists to
attach ([parity.md](parity.md)). A library variable feeding a dataflow would be
resolved into something that cannot run.

This is the same ceiling the row already records, not a new one.

[shortcut-doc]: https://learn.microsoft.com/en-us/fabric/onelake/assign-variables-to-shortcuts
