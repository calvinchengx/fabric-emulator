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
- [`bi-shared-docs/`](bi-shared-docs/) — Microsoft's open BI documentation
  corpus (CC-BY-4.0, pinned by reference). Golden reference for the XMLA
  protocol, rowset encodings, schema rowsets, and the TMSL/TMDL model formats.

See [docs/18-semantic-model-references.md](../docs/18-semantic-model-references.md)
for how these map onto the (as-yet-unbuilt) semantic-model engine and the DAX
oracle strategy.
