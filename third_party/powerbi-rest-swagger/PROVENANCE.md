# Power BI REST API OpenAPI (swagger.json)

The official OpenAPI/Swagger definition for the Power BI REST API. Our golden
reference for the **`executeQueries`** contract — the HTTP+JSON DAX query
surface a semantic-model query engine would expose:

- `POST /v1.0/myorg/datasets/{datasetId}/executeQueries`
- `POST /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/executeQueries`

(operationIds `Datasets_ExecuteQueries` / `Datasets_ExecuteQueriesInGroup`),
plus the request/response schemas — `DatasetExecuteQueriesRequest`, `Query`,
`QueryRequest`, `DatasetExecuteQueriesResponse`, `QueryResults`, `Table`,
`ErrorResponse` — the request body and rowset JSON shape we must match.

Chosen because it is a **machine-readable spec with a live oracle**: unlike the
XMLA/DAX layers (paper specs only, no bundleable engine — see
`../bi-shared-docs/PROVENANCE.md`), this contract is plain HTTP+JSON, so a real
client + this schema can continuously gate the implementation.

## Provenance

- **Upstream:** https://github.com/microsoft/PowerBI-CSharp — `sdk/swaggers/swagger.json`
- **Pinned revision:** `c7755706ce7216b60ef6363386e677ccb7ab7b21` (2026-03-15) —
  the last commit to touch the file as of retrieval
- **Retrieved:** 2026-07-14
- **Integrity:** `swagger.json` — sha256
  `e1431f9030c41cc6d39435507184b0a7cef17ac1cce93435f0f9d9d0a821be6c`, 1,570,842 bytes
  (`info.title` "Power BI Client", `info.version` "v1.0")
- **License:** MIT — Copyright (c) Microsoft Corporation. Full text in
  [`LICENSE`](LICENSE) (vendored from the same commit's `license.txt`).
- **Used by:** _(pending)_ the semantic-model query endpoint + its conformance
  test, once built. Referenced now as the pinned target of that work.

## Refresh

```sh
REV=<new-commit-sha>
curl -sSL "https://raw.githubusercontent.com/microsoft/PowerBI-CSharp/$REV/sdk/swaggers/swagger.json" \
  -o third_party/powerbi-rest-swagger/swagger.json
curl -sSL "https://raw.githubusercontent.com/microsoft/PowerBI-CSharp/$REV/license.txt" \
  -o third_party/powerbi-rest-swagger/LICENSE
shasum -a 256 third_party/powerbi-rest-swagger/swagger.json   # update Integrity above
```

Then update **Pinned revision**, **Retrieved**, and **Integrity**, and review
the `swagger.json` diff before committing.
