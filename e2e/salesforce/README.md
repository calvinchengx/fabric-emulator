# Salesforce round trip, through Bulk API 2.0

The worked example for [docs/41](../../docs/41-salesforce-connector-plan.md), and
the end-to-end proof of both halves of the connector.

## Why a round trip

Reading alone can pass while the rows are subtly wrong — a column mis-ordered, a
type flattened, a page dropped — and still produce a plausible count. Writing
them back and comparing what the org **received** against what it **served**
catches a shape that only looked right.

```
Login (Web activity)   ──▶  OAuth client credentials → access_token
   │
   ▼
Import (Copy)          ──▶  SalesforceV2Source: create a Bulk query job, poll it
   │                        out of InProgress, page results by Sforce-Locator
   ▼
Lakehouse  Tables/bronze_accounts  (Delta)
   │
   ▼
Export (Copy)          ──▶  SalesforceV2Sink: create an ingest job, PUT the CSV,
                            PATCH to UploadComplete, poll
```

Every activity type is real in Fabric, and every one executes for real here.

## The four things the stand-in org gets right

Each would let a convenient fiction pass while a real org failed:

| | Real Bulk API 2.0 | Why it matters |
|---|---|---|
| **A query is a lifecycle** | the job sits `InProgress` before results exist | a connector that fetched results immediately works against a canned mock and 404s against an org. The org counts polls; the driver asserts it was waited out |
| **Paging ends on `"null"`** | `Sforce-Locator` carries the literal string, not an absent header | one way you loop forever, the other you fetch a page named `null` |
| **Upload is `PUT`, `text/csv`** | not POST, not JSON | the org **rejects** any other content type, so the header is proven rather than assumed |
| **`PATCH` starts processing** | a job left `Open` accepts the upload and never runs | the driver asserts every job reached `JobComplete` — success with nothing written is the failure this catches |

## What the assertions catch

- **`rowsCopied == 5` over `resultPages == 3`.** A connector reading only the
  first page reports 2 and looks healthy.
- **The query job was polled ≥3 times**, read back from the org's own counter —
  proof the lifecycle ran rather than the results being grabbed directly.
- **`jobsWritten == 3`** at `writeBatchSize: 2`, and every ingest job created as
  an `upsert` on `Id`.
- **Every record identical after the round trip**, compared against what the org
  originally served.
- **`_delta_log` under the table**, read back through OneLake: the rows really
  committed as Delta between the two legs.

## Running it

```bash
uv run --frozen --no-sync python e2e/salesforce/run.py
```

No credentials and no network beyond the compose project: entra-emulator,
fabric-emulator, the stand-in org, and the driver.
