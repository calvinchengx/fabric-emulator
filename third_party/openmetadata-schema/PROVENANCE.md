# OpenMetadata entity schema — `column`

Golden reference for the payloads `scripts/govern_ingest.py` POSTs to
OpenMetadata. Copied in full (Apache-2.0, small).

| Field | Value |
|---|---|
| **Upstream** | https://github.com/open-metadata/OpenMetadata — `openmetadata-spec/src/main/resources/json/schema/` |
| **Pinned revision** | `2763bf97ce265662793a1a38d353147cc6d6c2e3` (tag `1.13.2-release`) |
| **Retrieved** | 2026-08-07 |
| **License** | Apache-2.0 — Collate Inc. / OpenMetadata contributors. Text in [`LICENSE`](LICENSE) |
| **Used by** | `python/tests/test_govern_column_schema.py`, which validates every column `govern_ingest.om_column` produces against `table.json#/definitions/column` |
| **Refresh** | `python3 scripts/vendor_openmetadata_schema.py <commit-sha>` — re-fetches, re-hashes, and rewrites the table below |

## Why only these eight files

The emulator builds **columns**, not whole tables, so the validated node is
`table.json#/definitions/column`. That subtree reaches exactly three external
schemas (`basic.json`, `tagLabel.json`, `customMetric.json`), whose own
transitive closure adds four more. Eight files, 64 KB.

Validating the whole of `table.json` instead would pull **146 files, 451 KB** —
`table.json` → `databaseService.json` → every connector configuration
OpenMetadata supports, so the repo would carry Snowflake, Oracle and Synapse
connection schemas, pinned and kept in sync, in order to check a column. The
narrower node buys the same coverage of what we actually send.

Nothing here is edited. A subsetting script that rewrote upstream JSON would
make the `sha256` column meaningless.

## What this does NOT catch

`dataLength` is required for `char`/`varchar`/`binary`/`varbinary` — and that
rule is **not in this schema**. In 1.13.2 `column` declares only
`required: ["name", "dataType"]`, and `dataLength` is a plain integer whose
description states the constraint in prose. OpenMetadata enforces it in its
Java service layer, which is why violating it returns a runtime 400 rather than
a validation error.

**A validator passes the exact payload that costs the whole table.** That is
what `scripts/check_govern_types.py` exists for, and why it stays: three layers,
each catching what the others cannot.

## Integrity

`sha256` and byte size of every vendored file, as retrieved.

| File | Bytes | sha256 |
|---|---|---|
| `LICENSE` | 11356 | `43070e2d4e532684de521b885f385d0841030efa2b1a20bafb76133a5e1379c1` |
| `schema/entity/data/table.json` | 43154 | `85275eba82dc01b6d82ad3600648501e00e11e31757bf6f3477b37b5bf08cf46` |
| `schema/tests/customMetric.json` | 1551 | `4c96cf4fa7d9c58455bceb322e226b6f7c6940fb321d634543d64d2e3094feed` |
| `schema/type/basic.json` | 11111 | `9c27781a905d71e5a7df94d7c2ab64c0ece35b8956cff575fec57119e67e2895` |
| `schema/type/entityReference.json` | 2052 | `fb10ed9b19a05d2dfe5dc3e593573e4f569621b6e820dcf688baa3a4b0e2f47d` |
| `schema/type/entityReferenceList.json` | 606 | `92b9ae8bd904b1d792bab74b936e74c70f2295db9373e709bce1efeeb61ceb57` |
| `schema/type/tagLabel.json` | 2857 | `e4a656db038f6a81ba09d42abdcdc4c38ab3ce423176eb777ea89544f4a6b04c` |
| `schema/type/tagLabelMetadata.json` | 756 | `0691ce9036ff862b60923a4131d57f969e1f32b4815b21d45408786990038ca2` |
| `schema/type/tagLabelRecognizerMetadata.json` | 1989 | `f77b15815bd396585743e9c0963f884a13a322c1100a427644c2e042e01de210` |

## The pin must match the running image

This copy is only evidence if it is the version the stack runs.
`docker-compose.yml` pins `1.13.2` in **three** places (the postgres, server and
migration services), and `test_govern_column_schema.py` fails when any of them
disagrees with the version recorded here — a schema validated against a version
nobody runs is confidence, not coverage.
