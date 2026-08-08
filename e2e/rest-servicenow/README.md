# rest-servicenow — the REST connector against the vendor Microsoft documents

`e2e/rest-helix` proves the REST connector against a vendor Fabric has **no**
connector for. This proves it against the one Microsoft's own documentation
uses to *teach* the feature: Example 1 of connector-rest's pagination rules is
`api/now/table/incident?sysparm_limit=…&sysparm_offset=…` with
`"QueryParameters.{offset}" : "RANGE:0:10000:1000"`.

The two suites are complements, not duplicates:

| | rest-helix | rest-servicenow |
|---|---|---|
| Record shape | nested under `values` | **flat** under `$.result` |
| Row shaping | needs `translator.mappings` | **no mappings** — auto-flatten must infer |
| Auth scheme | `AR-JWT` from a Web-activity login | **Basic**, the scheme the real connector uses |
| Paging | `limit`/`offset` only | offset RANGE **and** RFC 5988 `Link` |

## Scope

Proves the Table API's documented **shape** is reachable through `RestSource`
against a modelled server. It does **not** prove Fabric's first-party
ServiceNow connector type, which the emulator does not implement — auth here
goes through `additionalHeaders`, because a native `authenticationType` is
refused by name (the emulator models no connections). That is a real Fabric
pattern and the documented reason to choose `RestSource`: the built-in
ServiceNow connector is Basic-only, so OAuth requires this route.

## Why the counts are what they are

5 incidents at a page size of 2. A connector that reads one page reports 2, one
page short reports 4, and only a complete read reports 5 — the row count is the
assertion because a truncating read otherwise looks perfectly healthy. Both
paging routes must produce 5 independently.

The negative controls run **first**: an anonymous read and a wrong-credential
read must both be refused, or a later pass could not distinguish "the connector
sent credentials" from "the target lets anyone in".
