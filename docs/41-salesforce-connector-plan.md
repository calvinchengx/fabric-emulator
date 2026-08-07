# 41 — Salesforce: a job lifecycle, not a request

**Status: S1 delivered; S2–S3 scoped.** `SalesforceV2Source` runs the real Bulk
API 2.0 query lifecycle ([salesforce.go](../internal/api/salesforce.go)).

[40-rest-connector-plan.md](40-rest-connector-plan.md) deferred this deliberately
and said why: Fabric's Salesforce connector is **not** the REST connector with a
different URL. It runs on **Bulk API 2.0** — create a job, poll it to completion,
download a CSV result set, page it by locator — which shares nothing with
`RestSource` beyond bounded HTTP. Building it on the REST path would have meant
pretending a lifecycle is a request.

Unlike BMC Helix, Salesforce **has** a first-party Fabric connector, so there is
a real surface to match rather than a generic one to reach it through.

## The contract, read off Salesforce's own docs

Verified against the Bulk API 2.0 developer guide, not remembered. The one that
bites: the upload verb is **PUT**, not POST — a summary claimed POST, and a
wrong verb here would have made the whole ingest path fiction.

### Query — what S1 implements

| Step | Call |
|---|---|
| Create | `POST /services/data/{v}/jobs/query` — `{operation, query, contentType, lineEnding}` |
| Poll | `GET /services/data/{v}/jobs/query/{id}` → `state` |
| Results | `GET /services/data/{v}/jobs/query/{id}/results?maxRecords=&locator=` → CSV |

`operation` is `query`, or **`queryAll`** when `includeDeletedObjects` is set —
that flag is not a filter the emulator applies, it is a different operation.

States run `Open → UploadComplete → InProgress → JobComplete`, with `Failed` and
`Aborted` terminal. Result paging is by the **`Sforce-Locator`** response header,
and it ends when that header is the **literal string `"null"`** — not an absent
header, not an empty one. That is the detail most likely to be got wrong, so it
is asserted directly.

### Ingest — S2

`POST /jobs/ingest` → `{id, contentUrl}`, **`PUT` the CSV** to
`/jobs/ingest/{id}/batches` as `text/csv`, `PATCH` the job to `UploadComplete`,
then poll. Operations `insert` / `update` / `upsert` / `delete` / `hardDelete`;
Fabric's `writeBehavior` exposes `insert` and `upsert` only.

## Fabric's surface

Source: `objectApiName` | `reportId` | SOQL `query`, plus `includeDeletedObjects`.
Sink: `objectApiName`, `writeBehavior`, `externalIdFieldName`, `ignoreNullValues`,
`writeBatchSize`.

**`reportId` is refused by name in S1.** Reports are a different API entirely
(the Analytics REST API), not a Bulk query, and Salesforce documents its own
limitations on them. Accepting the property and quietly running a Bulk query
instead would return the object's rows rather than the report's — plausible data,
wrong data.

## Authentication, and why it looks like the REST connector's

Fabric puts Salesforce credentials on a **connection**, which this emulator does
not model — the same wall `RestSource` hit. So S1 takes `instanceUrl` and
`accessToken` directly on the source, both expression-resolved.

That is not a shortcut so much as the same shape the repo already uses where
Fabric's connection model is absent: the Script activity names its target
`{workspaceId, itemId}` directly for exactly this reason. And it composes — a
Web activity can run the OAuth client-credentials call and hand the token over
as `@activity('Login').output.access_token`, which is precisely how
`e2e/rest-helix` threads BMC Helix's AR-JWT.

## No dependency

[`k-capehart/go-salesforce`](https://github.com/k-capehart/go-salesforce) was
surveyed in doc 40 and fits the surface. S1 does not use it.

The query lifecycle is three endpoints and a locator loop against a bounded HTTP
client this repo already owns. Against that, a dependency brings its own auth
model, its own retry policy and its own CSV handling — none of which this needs,
all of which would have to be understood before the first bug. The repo is
pure-Go and stdlib-heavy on purpose; this is not the place to spend that.

Revisit if S2's ingest lifecycle or OAuth flows turn out to be where the cost is.

## Phasing

| | Scope | Size |
|---|---|---|
| **S1** ✅ | `SalesforceV2Source` — query lifecycle, `objectApiName`/`query`, `includeDeletedObjects`, locator paging, CSV → Delta | **M** |
| **S2** | `SalesforceV2Sink` — ingest lifecycle, insert/upsert, `writeBatchSize` | **M** |
| **S3** | e2e against a stand-in Salesforce, as `e2e/rest-helix` does for Helix | **S** |

S1 and S2 split at a real seam, the same way R1/R2 did: reading and writing share
only the job-state vocabulary.

## Bounds

A job that never reaches a terminal state must not hang a pipeline, and a query
that returns more than the emulator can hold must refuse rather than truncate.
Same ceilings and same reasoning as the REST connector: 8 MiB per result page,
1M rows total, and a bounded poll that gives up with a message naming the job id
and its last state.

## What is not settled

- **Whether `reportId` is worth building.** It is a different API, and no demand
  has been named. Refusing it costs nothing; guessing at it would.
- **Whether the emulator should model connections.** Third time this has come up
  (Script, `RestSource`, now here). It is bigger than any one connector, and the
  workaround has held three times, which is itself evidence about priority.
