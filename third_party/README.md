# third_party — vendored golden references

Authoritative external specs and fixtures the emulator is built and tested
against — our "golden references." Everything here is **someone else's artifact,
pinned**: we do not author it, we conform to it.

The emulator's correctness bet is that real, unmodified clients can't tell it
from real Fabric. That only holds if we validate against the same specs Fabric
publishes. This directory is where those specs live, pinned to an exact upstream
revision so a reference can never silently drift.

## The pattern

Each golden reference is a subdirectory with a `PROVENANCE.md` that records,
without exception:

| Field | Why |
|---|---|
| **Upstream** | repo URL + exact file path(s) |
| **Pinned revision** | the commit SHA (never a moving branch/tag) + its date |
| **Retrieved** | the date we pulled it |
| **Integrity** | `sha256` + byte size of each vendored file |
| **License** | SPDX id + copyright holder + where the license text is |
| **Used by** | the code/docs/e2e in *this* repo that depend on it |
| **Refresh** | the exact command to re-fetch and re-pin |

Two vendoring modes, chosen by license + size:

- **Copied in full** — for small, permissively-licensed artifacts (e.g. an
  MIT OpenAPI). The file lives here verbatim; its `sha256` in `PROVENANCE.md`
  is the tamper check. Ship the upstream `LICENSE` beside it.
- **Pinned by reference** — for large or share-alike-licensed corpora
  (e.g. CC-BY docs). We do **not** copy the bytes; `PROVENANCE.md` records the
  repo, the pinned commit SHA, and the specific files we rely on, with the
  required attribution. Clone upstream at that SHA to read them.

## Refreshing a reference

Bump the pinned SHA deliberately, in its own commit: re-fetch, update `sha256`
+ size + `Retrieved` in `PROVENANCE.md`, and diff the artifact so the change to
the golden truth is reviewable. Never let a reference float.

## Contents

- [`powerbi-rest-swagger/`](powerbi-rest-swagger/) — the official Power BI REST
  OpenAPI (MIT, copied in full). Golden reference for the `executeQueries` DAX
  query contract a semantic-model query endpoint would conform to.
- [`openmetadata-schema/`](openmetadata-schema/) — OpenMetadata's own entity
  schema for a table **column** (Apache-2.0, copied in full). Golden reference
  for the payloads `scripts/govern_ingest.py` sends; only the eight files the
  `column` node reaches are vendored, not the 146 the whole table schema pulls.
- [`bi-shared-docs/`](bi-shared-docs/) — Microsoft's open BI documentation
  corpus (CC-BY-4.0, pinned by reference). Golden reference for the XMLA
  protocol, rowset encodings, schema rowsets, and the TMSL/TMDL model formats.

- [`fabric-activity-types/`](fabric-activity-types/) — Microsoft's own
  `DataPipelineActivityTypes` table, the list of data-pipeline activity
  discriminators Fabric documents. Pinned by **content hash rather than a
  commit SHA**, because the article is a REST-API reference published only as
  generated HTML and no public repo carries it — the exception is argued in its
  `PROVENANCE.md`. Golden reference for `internal/api/fabricActivityTypes`,
  which `scripts/check_fabric_activity_types.py` holds to it; that list is what
  `TestEveryDocumentedFabricActivityTypeIsHandled` walks, so a type Fabric adds
  becomes a failing check instead of a fabricated success.

- [`notebookutils-stubs/`](notebookutils-stubs/) — Microsoft's own
  `dummy-notebookutils` wheel, the stub package they publish so notebook code
  can be developed off-cluster. It states the `notebookutils.*` surface in a
  machine-readable form, which is what `scripts/check_notebookutils_surface.py`
  holds our shim to. Synapse-lineage and therefore broader than Fabric, so the
  checker carries Fabric's module list as a second source.

See [docs/18-semantic-model-references.md](../docs/18-semantic-model-references.md)
for how these map onto the (as-yet-unbuilt) semantic-model engine and the DAX
oracle strategy.
