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

## Why ids are never hardcoded

Ids cannot match across targets — a workspace GUID in the emulator has no
relationship to one in your tenant. So everything durable is addressed **by
name** and resolved to a GUID per target, at startup, by the resolver. This is
not an emulator concession; it is the only thing that can work.

## How this is enforced

- **`scripts/check_example_portability.py`** (in `make check`) fails when an
  example hardcodes the seeded principal or a `localhost` endpoint outside the
  resolver, when the resolver stops consuming `fabric-target`, or when a
  definition part uses a path that is not Fabric's.
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

**One caution on that Git source-code-format page**: its "item definition files"
section lists only six item types and **omits data pipelines**, even though the
Data Factory CI/CD docs document pipeline Git integration and
`pipeline-content.json`. Treat it as authoritative on layout and `.platform`, not
as an inventory of what Git supports.
