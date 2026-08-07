# 40 — REST connector: the honest way to reach Salesforce, ServiceNow and BMC Helix

**Status: R1 + R2 delivered; R3–R4 scoped.** `RestSource` makes the real request and
commits real rows ([restconnector.go](../internal/api/restconnector.go)), and
**really pages** ([restpagination.go](../internal/api/restpagination.go)) —
cursors, ranges, end conditions, RFC 5988, with a ceiling that refuses an
endless `next` rather than looping. This plan replaces the `External-connector
leaves` row in [parity.md](parity.md) — the last **stubbed success** left in the
pipeline interpreter after [webactivity.go](../internal/api/webactivity.go)
removed the other one.

The conclusion it argues for is not the obvious one. The request that started it
was "we want external connectors for Salesforce and BMC Helix." The answer is
**build Fabric's generic REST connector first**, and reach BMC Helix through it —
because that is what real Fabric does, and a `BMCHelixSource` activity type would
diverge from Fabric in the one direction this emulator must never diverge.

## The finding that shapes the plan

BMC Helix ITSM — ServiceNow's competitor, formerly BMC Remedy, still `arsys` on
the wire — **has no Fabric connector**. Not in Fabric, not in ADF, not in Power
Query. Its competitor does: Fabric ships a first-party
[ServiceNow connector](https://learn.microsoft.com/en-us/fabric/data-factory/connector-servicenow-copy-activity).
Helix does not, and Microsoft's own answer to "how do I connect BMC Helix" is a
custom connector over its REST API.

| | Fabric first-party connector | How a real user reaches it |
|---|---|---|
| Salesforce | **Yes** — `objectApiName` / `reportId` / SOQL `query`, Bulk API 2.0 | The connector |
| ServiceNow | **Yes** — but **Basic auth only** | The connector, or REST when they need OAuth |
| BMC Helix | **No** | **The generic REST connector** |

That table is the whole argument. Two of the three already route through REST for
real users, and the third has no other route at all.

### Why not just add `BMCHelixSource`

Because it would run here and fail to parse in Fabric.

Every other gap in this emulator errs toward *refusing* work that Fabric accepts.
A fictional item type errs the other way: a pipeline authored against the
emulator would work locally and be rejected by the product. That is worse than a
stub and worse than a 501, because it is the only failure mode that makes the
emulator actively misleading about what Fabric can run. The stub we have today at
least fails toward "nothing happened."

`RestSource` is a real Fabric type. `BMCHelixSource` is not. That decides it.

### What this buys beyond Helix

The REST connector is not a consolation prize — it is the higher-leverage piece.
One implementation unstubs Helix, Jira, ServiceNow-over-OAuth, and every other
REST leaf at once, and it is a genuine parity claim in its own right. Salesforce
stays a separate, later piece of work, because it is a genuinely different
connector (Bulk API 2.0 job lifecycle, not request/response) — see
[Deferred](#deferred-salesforce).

## What Fabric's REST connector actually is

Read off the
[REST copy-activity reference](https://learn.microsoft.com/en-us/azure/data-factory/connector-rest),
not remembered. These are the names an authored pipeline will carry, so they are
the names to parse.

### `RestSource`

| Property | Fabric's behaviour |
|---|---|
| `requestMethod` | `GET` (default) or `POST` |
| `additionalHeaders` | Extra request headers |
| `requestBody` | Body for the request |
| `paginationRules` | Composes the next page's request — see below |
| `httpRequestTimeout` | TimeSpan, default `00:01:40` |
| `requestInterval` | Wait before the *next page*, default `00:00:01` |

Two constraints worth encoding as tests, because both are surprising:

- **`Accept` is ignored.** The connector overrides whatever the author set and
  sends `Accept: application/json`. JSON responses only.
- **Pagination is unsupported when the top-level response is a JSON array.**

### `RestSink`

| Property | Fabric's behaviour |
|---|---|
| `requestMethod` | `POST` (default), `PUT`, `PATCH` |
| `additionalHeaders` | Extra request headers |
| `writeBatchSize` | Records per batch, default **10000** |
| `httpRequestTimeout` | TimeSpan, default `00:01:40` |
| `requestInterval` | Milliseconds, valid range **[10, 60000]** |
| `httpCompressionType` | `none` or `gzip` |

The sink sends a **JSON array of row objects** per batch — `[{…}, {…}]`. That is
the entire wire contract.

### Pagination rules

A case-sensitive dictionary. Iteration stops on **HTTP 204**, or when any
JSONPath in the rules resolves to null.

| Key | Meaning |
|---|---|
| `AbsoluteUrl` | Next request's URL (absolute or relative) |
| `QueryParameters.{name}` | Sets a query parameter on the next request |
| `Headers.{name}` | Sets a header on the next request |
| `EndCondition:{expr}` | Terminates the loop |
| `MaxRequestNumber` | Hard cap; empty means unlimited |
| `SupportRFC5988` | Follows `Link: …; rel="next"`. **Defaults to true when no other rule is defined** |

Values are either `Headers.{response_header}`, a JSONPath starting `$`, or
`RANGE:start:end:step` (an empty `end` means open-ended). `EndCondition` values
are `Empty`, `NonExist`, `Exist`, or `Const:<value>`.

Microsoft's own first pagination example is
`baseUrl/api/now/table/incident?sysparm_limit=1000&sysparm_offset=0` — a
ServiceNow table paged by offset. **BMC Helix's `limit`/`offset` is structurally
identical**, which is a useful signal: the ITSM shape is the documented case, not
an exotic one.

## Where it plugs in

`copyActivity` ([pipelines.go:418](../internal/api/pipelines.go)) resolves both
sides through `resolveLoc` into a `oneLakeLoc` before it does anything else. A
REST endpoint is not a OneLake location, so the dispatch has to move earlier.

The seam already half-exists. `copyIntoTable` produces a `*warehouse.Table` and
commits it via `warehouse.WriteDeltaTableAs`. **A REST source is a second way to
produce that same `*warehouse.Table`** — the write half is untouched.

```
copyActivity
  ├─ read source.type
  │    ├─ "RestSource" ──────────► restSource() ──► *warehouse.Table
  │    └─ OneLake types ─────────► resolveLoc + copyIntoTable (unchanged)
  └─ read sink.type
       ├─ "RestSink" ────────────► restSink(table)
       └─ OneLake types ─────────► existing Delta / byte paths
```

Concrete edits:

- `copySideTypes` gains `RestSource` / `RestSink`. It is currently the gate that
  makes an unknown type fail loudly instead of degrading to an opaque byte copy —
  that property must survive, so the new types are added deliberately, not by
  loosening the check.
- New file `internal/api/restconnector.go`, alongside `webactivity.go`, which
  already owns the "real HTTP from a pipeline activity" concerns.
- Reuse `httpx.ReadBounded` and `pipeline.ParseTimeout`. Both exist because of
  the Web activity; neither needs changing.

### JSON → rows

Fabric shapes the response through copy-activity **schema mapping**
(`translator.mappings` with `collectionReference` naming the array). Full
translator support is its own project. Phased:

- **R1** — `collectionReference` (a JSONPath to the record array) plus
  auto-flattening of each object's scalar fields into columns. Column order is
  the union of keys in first-seen order, so it is deterministic. Absent a
  `collectionReference`, the response must be a JSON object containing exactly one
  array — otherwise fail loudly and name the ambiguity.
- **Later** — explicit `mappings` entries, nested paths, type coercion.

Values stay as JSON-typed as they arrive (string/number/bool). This matches
`parseTabular`'s existing reasoning inverted: CSV keeps strings because the file
does not describe types, and JSON *does* describe them, so discarding that would
be the guess.

## Authentication: the part to get right

Fabric puts auth on the **linked service** (`RestService`): `Anonymous`, `Basic`,
`AadServicePrincipal`, `OAuth2ClientCredential`, `ManagedServiceIdentity`, plus
free-form `authHeaders`.

The emulator **does not model connections or linked services** at all — parity.md
says so explicitly for the Script activity. Building a connection registry to
land this connector would be a much larger change wearing a small change's
clothes.

**R1 therefore supports `Anonymous` + `additionalHeaders` only, and says so.**
That is not a shortcut — it is the case that actually matters here, because:

**BMC Helix's auth is not one of Fabric's schemes anyway.** `POST /api/jwt/login`
returns a raw JWT in the body, presented as `Authorization: AR-JWT <token>` — a
proprietary scheme, not `Bearer`. In real Fabric a Helix user gets that token
with a **Web activity** and passes it to the REST source through an expression.
Which the emulator can already do, today, because the Web activity is real:

```jsonc
{ "name": "Login", "type": "WebActivity", "typeProperties": {
    "url": "@{pipeline().parameters.helix}/api/jwt/login",
    "method": "POST", "body": "username=…&password=…" } },

{ "name": "Incidents", "type": "Copy",
  "dependsOn": [{ "activity": "Login", "dependencyConditions": ["Succeeded"] }],
  "typeProperties": {
    "source": {
      "type": "RestSource",
      "additionalHeaders": { "Authorization": "AR-JWT @{activity('Login').output.body}" },
      "paginationRules": { "QueryParameters.offset": "RANGE:0::100",
                           "EndCondition:$.entries": "Empty" }
    },
    "sink": { "type": "LakehouseTableSink", "tableActionOption": "Overwrite" }
  } }
```

That pipeline is Fabric-shaped end to end. Every activity type in it is real in
the product, and every one of them would execute for real here.

The lesson from the Web activity applies directly: the danger is never the
missing feature, it is the fabricated success. An unsupported `authenticationType`
must **fail loudly and name itself**, never fall through to an anonymous request
that 401s and gets reported as a connector bug.

## Bounds

`webactivity.go` bounds one response at 8 MiB. A *paginated* read has no such
natural ceiling — a thousand pages of 8 MiB is 8 GiB, held in memory, then
written to a run record. New constants in `internal/httpx`:

| Bound | Proposed | Why |
|---|---|---|
| Per-page body | 8 MiB | Same as the Web activity, same reasoning |
| Total rows | 1,000,000 | Refuse rather than OOM |
| Total pages | 1,000 | Backstop when `MaxRequestNumber` is absent and an endpoint's `next` never terminates — Microsoft documents that exact endless-loop case |

Each refusal names the bound and the knob that would have prevented it.
`requestInterval` is honoured on the **virtual clock**, like `policy.retry`
backoff — real sleeping between pages would make the test suite slow for no
fidelity gain.

## Phasing

Four PRs, each independently shippable and independently useful.

| | Scope | Size |
|---|---|---|
| **R1** ✅ | `RestSource`, single page, `collectionReference` + auto-flatten, anonymous/`additionalHeaders`, `httpRequestTimeout`, bounds, → Lakehouse table sink | **M** |
| **R2** ✅ | `paginationRules`: `AbsoluteUrl`, `QueryParameters.{p}`, `Headers.{h}`, `RANGE:`, `EndCondition:`, `MaxRequestNumber`, RFC 5988, 204 stop | **M** |
| **R3** | `RestSink` — batching by `writeBatchSize`, `POST`/`PUT`/`PATCH`, `gzip`, `requestInterval` | **S** |
| **R4** | BMC Helix worked example under `examples/`, parity + witnesses + docs | **S** |

R1 and R2 split at a real seam: R1 is one request and the row-shaping; R2 is the
loop around it. Shipping R1 alone is honest — a single-page REST read is a
complete, useful capability, and pagination arriving unimplemented-but-declared
would be the failure this plan exists to avoid.

## Testing

`httptest`, throughout, exactly as
[webactivity_test.go](../internal/api/webactivity_test.go) does it. **No network
in CI, no stub mode.** The Web activity needed `FABRIC_WEB_ACTIVITY=stub` only
because it had prior behaviour to preserve; this connector has none, so shipping
a stub switch would be inventing the hazard.

The cases that must exist, because each is a way to be silently wrong:

- Pagination really loops — assert the **sequence of request URLs the server
  saw**, not just the final row count
- `EndCondition` terminates; `MaxRequestNumber` caps; a `next` pointing at itself
  hits the page bound instead of hanging
- HTTP 204 stops the loop
- An `Accept` header in `additionalHeaders` is **overridden** to
  `application/json`
- A top-level JSON array with pagination rules fails loudly
- Non-2xx fails the activity, carrying status and body — as the Web activity does
- Row bound and page bound both refuse rather than truncate
- Sink batching: `writeBatchSize: 2` over 5 rows produces **3 requests**, with
  the array payload asserted per batch
- An unsupported `authenticationType` fails and names itself

Mutation check, as with the Web activity: restoring the stub must fail a
countable number of these.

## Parity, witnesses, docs

- Split the `External-connector leaves` row again. `RestSource`/`RestSink` become
  **🟢 Real**; named vendor connectors (Salesforce, ServiceNow) stay **🟡** with
  the REST route documented as the answer.
- Register `rest-connector` in [witnesses.json](witnesses.json) under
  ``Data Factory (`data-factory/`)``, with `go:` witnesses for the pagination
  loop, the bounds, and the sink batching — the three claims a reader is most
  entitled to doubt.
- [04-configuration.md](04-configuration.md) for any new knob.
- R4's example goes under `examples/`, driven by a local fake Helix so it runs in
  CI without credentials.

## Deferred: Salesforce

Not folded into this plan, deliberately. Fabric's Salesforce connector runs on
**Bulk API 2.0** — create job, upload, poll state, download result — which is a
job lifecycle, not a request/response, and shares almost nothing with the REST
connector's code path beyond bounded HTTP.

When it comes, the surface to match is `objectApiName` / `reportId` / SOQL
`query` / `includeDeletedObjects` on the source and `objectApiName` /
`writeBehavior` / `externalIdFieldName` / `ignoreNullValues` / `writeBatchSize`
on the sink. Library options were surveyed:
[`k-capehart/go-salesforce`](https://github.com/k-capehart/go-salesforce) (MIT,
Bulk 2.0 query *and* ingest, three OAuth flows) fits the surface;
[`simpleforce`](https://github.com/simpleforce/simpleforce) has more stars but no
Bulk API and no OAuth, so it does not.

## What is not settled

- **Whether the emulator should ever model connections.** R1 sidesteps it. If
  `Basic` and `OAuth2ClientCredential` turn out to be wanted, that question has to
  be answered rather than sidestepped again, and it is bigger than this connector.
- **How much of `translator.mappings` is worth building.** R1's auto-flatten
  covers the ITSM shape. Nested extraction and type coercion are unmeasured
  demand.
- **Whether `SupportRFC5988` defaulting to true is worth matching exactly.** It
  means a source with *no* pagination rules still follows `Link` headers. Faithful,
  and surprising — a test either way, but the default is worth a second look
  before it is copied.
