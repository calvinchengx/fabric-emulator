# 46 — Where a data engineer's artifacts actually live

**Status: enforced** (`scripts/check_example_portability.py`, in `make check`).

This is the contract that makes [docs/21](21-real-fabric-toggle.md)'s one-flag
toggle *mean* something. The toggle resolves endpoints and credentials; this
document says what your artifacts **are** and where they **persist** — because a
pipeline that runs against both targets while persisting its work in a shape real
Fabric would refuse has parity in the demo and none in production.

Verified against Microsoft's own documentation, not inferred from the emulator.

## Two stores, and the line between them

| | Definitions (item source) | Data |
|---|---|---|
| Lives in | the **workspace** metadata store | **OneLake** |
| Reached by | `getDefinition` / `updateDefinition` | ADLS/Blob APIs, Spark, TDS |
| In Git? | **yes**, when the workspace is connected | **never** |
| Overwritten by a deployment? | yes, that is the point | **no, never** |

The second row of the last column is the one people get wrong. Fabric's own
documentation is explicit: tables (Delta and non-Delta) and folders under
`Files/` **aren't tracked or versioned in Git**, and Git and deployment
operations never overwrite them. Your notebook *code* is versioned; the lakehouse
*tables it writes* are not. A CI/CD pipeline moves definitions between
workspaces — it does not move data, and a design that assumes otherwise will
silently lose it or silently keep stale copies.

## Definitions are base64 parts

`getDefinition` returns, and `updateDefinition` accepts, a list of parts:

```json
{"definition": {"parts": [
  {"path": "notebook-content.py", "payload": "<base64>", "payloadType": "InlineBase64"},
  {"path": ".platform",           "payload": "<base64>", "payloadType": "InlineBase64"}
]}}
```

The `path` values are **Fabric's, not ours**. Inventing one produces an item the
emulator will happily store — it keeps parts verbatim by design — and real Fabric
will reject. That asymmetry is why this is a gate and not a guideline.

| Item | Parts |
|---|---|
| Notebook | `notebook-content.py` (or `notebook-content.ipynb` with `?format=ipynb`) |
| Data pipeline | `pipeline-content.json` |
| Semantic model | `definition.pbism` + `definition/` TMDL files |
| Report | `definition.pbir` + `report.json` |
| *every* item | `.platform` |

## `.platform`, and the identity that survives a rename

```json
{
  "version": "2.0",
  "$schema": "https://developer.microsoft.com/json-schemas/fabric/platform/platformProperties.json",
  "config": {"logicalId": "e553e3b0-0260-4141-a42a-70a24872f88d"},
  "metadata": {"type": "Notebook", "displayName": "silver", "description": "..."}
}
```

`logicalId` is the load-bearing field: a cross-workspace identifier linking an
item to its source-control representation, stable across renames and directory
moves. **Do not change it.** Version 1 split this into `item.metadata.json` +
`item.config.json`; a directory must carry one form or the other, never both.

## The Git layout

One directory per item, named `{display name}.{public facing type}`, containing
the definition files plus `.platform`:

```
silver.Notebook/
  notebook-content.py
  .platform
ingest.DataPipeline/
  pipeline-content.json
  .platform
```

Invalid, leading, or trailing characters in a display name are replaced by their
HTML number; if the name is unavailable the `logicalId` is used instead.

## The two addresses a definition needs at runtime: compute, and SQL

A definition is portable. What it *runs on* and what it *queries through* are
assigned by the service, and this is where an example stops being portable
without noticing — everything above can be perfect while a step still dials
`localhost:1433`.

### Spark: you do not get a pool, you get a job

There is no Spark endpoint on Fabric to connect to from outside. Spark lives
inside the service and the unit of work is a **submitted job**, not a session you
attach to:

| | What it is | Who picks the compute |
|---|---|---|
| **Starter pool** | Microsoft-managed pre-warmed Medium nodes, the workspace default. Best-effort: when warm capacity exists a session starts in seconds, otherwise it falls back to on-demand provisioning and takes longer. Node ceilings are per capacity SKU (F2 → 1 node, F64 → 16, F2048 → 200). | workspace Spark settings |
| **Custom pool** | Your node family, size and autoscale. A *custom live pool* keeps clusters warm on a schedule you control, so sessions start in about 5 seconds inside that window and environment libraries are already installed. | an **Environment** attached to the item or workspace |
| **Environment** | The Spark runtime + libraries + pool selection, as a first-class item. This is the only handle you get on "which pool", and it is passed by id. | you, per item or per job |

Two ways to submit:

1. **Run the notebook item** — `POST /v1/workspaces/{ws}/notebooks/{id}/jobs/instances?jobType=RunNotebook`,
   which takes parameters, a session/compute configuration, an environment, and a
   default lakehouse. This is the portable one: the emulator accepts the same
   call, so `run_job(notebook_id, "RunNotebook")` works on both targets.
2. **The Livy API** — `POST /v1/workspaces/{ws}/lakehouses/{lh}/livyapi/versions/2023-12-01/{sessions|batches}`.
   Sessions are interactive and keep state across statements (idle-terminated
   after 20 minutes); batches are one application per submission. A session runs
   on the workspace's starter pool unless you pass
   `conf: {"spark.fabric.environmentDetails": "{\"id\": \"<environmentId>\"}"}`.
   Beyond the usual Fabric scopes this needs `Lakehouse.Execute.All` plus the
   `Code.Access*` family (`Code.AccessFabric.All` and `Code.AccessStorage.All`
   are required; Key Vault, ADLS, Kusto and SQL are opt-in scopes).

The consequence for this repo: the emulator has no pool, so it parses a notebook
into cells, records a `Pending` run, and waits for an engine (Sail over Spark
Connect) to execute them and report back. That engine is scaffolding for the
missing half of the target. A step that drives Spark Connect **directly** is
emulator-only by construction. That is why `silver.py` no longer does it: its
transform lives in `definitions/silver.Notebook/` and is submitted with
`run_job(nb, "RunNotebook")`, so the emulator's agent executes it locally and a
Fabric pool executes the same cells on a tenant. `engine.py` skips under `real`
(Fabric runs the queued notebook itself), and `star_silver.py` in the advanced
examples still refuses, because that transform has not been moved yet.

### SQL: the address is per-item and only the API knows it

Both SQL surfaces are assigned by the service at creation time. Ask the item, on
the **typed** route (the generic `/items/{id}` answers the generic record):

| Surface | Where the address lives | Writable |
|---|---|---|
| Warehouse | `GET /warehouses/{id}` → `properties.connectionString` | yes |
| Lakehouse SQL analytics endpoint | `GET /lakehouses/{id}` → `properties.sqlEndpointProperties.connectionString` | **no**, read-only over the Delta tables |

Both look like `<opaque>-<opaque>.datawarehouse.fabric.microsoft.com` on port
1433, with TLS required. Three things that bite:

- **`provisioningStatus`** on the lakehouse endpoint is `InProgress` until it is
  `Success`. It is provisioned asynchronously, so a client that reads the
  connection string and dials immediately can be early.
- **The database is the DISPLAY NAME**, and the workspace is encoded in the
  server name. This is why the emulator accepts *either* the item id or the
  display name (`internal/server/warehouse.go`): the id is the only addressing
  that works when one host serves every workspace.
- **The analytics endpoint lags the lakehouse.** Metadata sync is normally under
  a minute but is not instant, so a table Spark just wrote can be briefly absent
  from T-SQL. `POST /v1/workspaces/{ws}/sqlEndpoints/{sqlEndpointId}/refreshMetadata`
  forces the sync rather than waiting for the background one. **The emulator does
  not implement that endpoint**, and does not report the endpoint's `id` either —
  it has no separate `SQLEndpoint` item and reflects on connect, so there is
  nothing to sync. Code that needs the refresh must be written against real
  Fabric, and will 404 locally.

`examples/contoso-fixtures/common.py:sql_endpoint()` is the portable form:
discover the address, use the item id locally and the display name on real
Fabric, TLS on real and off against the emulator's FedAuth-without-TLS front.
`TDS_SERVER` remains only as an override, because the emulator advertises the
port it *listens* on rather than the one Docker published.

## Why ids are never hardcoded

Ids cannot match across targets — a workspace GUID in the emulator has no
relationship to one in your tenant. So everything durable is addressed **by
name** and resolved to a GUID per target, at startup, by the resolver. This is
not an emulator concession; it is the only thing that can work.

## How this is enforced

- **`scripts/check_example_portability.py`** (in `make check`) fails when an
  example hardcodes the seeded principal, a `localhost` control-plane endpoint or
  a SQL host outside the resolver, when the resolver stops consuming
  `fabric-target`, or when a definition part uses a path that is not Fabric's.
- **`python/tests/test_check_example_portability.py`** proves the gate fails on
  each of those, *and* that it does not fire on a OneLake shortcut target or on
  installed dependencies — the false positives its first version produced.
- **`examples/contoso-fixtures/common.py`** is the single resolver for all four
  medallion examples. It consumes `fabric-target`; it does not restate it.

## The failure this prevents, from the field

`contoso-data-platform` restated the target contract locally while
`fabric-target` was unpublished, and the restatement drifted: it resolved the
real target to an Entra client-credentials flow requiring `AZURE_CLIENT_SECRET`.
That meant `az login` did not work, a managed identity did not work, and the
platform **could not have run inside a Fabric notebook at all** — a notebook has
no client secret to give. It looked correct and passed its own tests, because its
tests ran against the emulator.

A contract you copy is a contract you get wrong. Consume `fabric-target`.

## Sources

- [Overview of item definitions](https://learn.microsoft.com/en-us/rest/api/fabric/articles/item-management/definitions/item-definition-overview)
- [Notebook definition](https://learn.microsoft.com/en-us/rest/api/fabric/articles/item-management/definitions/notebook-definition) · [Get Notebook Definition](https://learn.microsoft.com/en-us/rest/api/fabric/notebook/items/get-notebook-definition)
- [Git source code format](https://learn.microsoft.com/en-us/fabric/cicd/git-integration/source-code-format) — directory naming, `.platform`, `logicalId`
- [CI/CD for pipelines in Data Factory](https://learn.microsoft.com/en-us/fabric/data-factory/cicd-pipelines) · [Pipeline REST API](https://learn.microsoft.com/en-us/fabric/data-factory/pipeline-rest-api)
- [Lakehouse Git integration and deployment pipelines](https://learn.microsoft.com/en-us/fabric/data-engineering/lakehouse-git-deployment-pipelines) — the tables-and-`Files/` exclusion
- [Notebook source control and deployment](https://learn.microsoft.com/en-us/fabric/data-engineering/notebook-source-control-deployment)

Compute and SQL endpoints:

- [Run On Demand Notebook](https://learn.microsoft.com/en-us/rest/api/fabric/notebook/background-jobs/run-on-demand-notebook) · [Notebook public API](https://learn.microsoft.com/en-us/fabric/data-engineering/notebook-public-api)
- [Livy API overview](https://learn.microsoft.com/en-us/fabric/data-engineering/api-livy-overview) · [Livy sessions](https://learn.microsoft.com/en-us/fabric/data-engineering/get-started-api-livy-session) — the URL pattern, the `Code.*` scopes, and the Environment override
- [Configure starter pools](https://learn.microsoft.com/en-us/fabric/data-engineering/configure-starter-pools) — best-effort warm capacity, per-SKU node ceilings
- [Get Lakehouse](https://learn.microsoft.com/en-us/rest/api/fabric/lakehouse/items/get-lakehouse) · [Get Warehouse](https://learn.microsoft.com/en-us/rest/api/fabric/warehouse/items/get-warehouse) — where the connection strings live
- [SQL analytics endpoint metadata sync](https://learn.microsoft.com/en-us/fabric/data-engineering/sql-analytics-endpoint-metadata-sync) — the lag, and `refreshMetadata`

**One caution on that Git source-code-format page**: its "item definition files"
section lists only six item types and **omits data pipelines**, even though the
Data Factory CI/CD docs document pipeline Git integration and
`pipeline-content.json`. Treat it as authoritative on layout and `.platform`, not
as an inventory of what Git supports.
