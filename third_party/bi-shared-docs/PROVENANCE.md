# MicrosoftDocs/bi-shared-docs — pinned by reference (not copied)

Microsoft's open documentation corpus for Analysis Services / Power BI: the
authoritative **paper specs** for the semantic-model layers the emulator does
not yet implement — the XMLA protocol, its rowset encodings, the schema
rowsets, and the TMSL/TMDL model formats.

**Not vendored in full.** The content is CC-BY-4.0 (share-alike attribution)
and the repo is large, so under the `third_party/` pattern it is *pinned by
reference*: clone upstream at the SHA below to read these files. Only the
provenance + attribution live here.

## Provenance

- **Upstream:** https://github.com/MicrosoftDocs/bi-shared-docs
- **Pinned revision:** `328074568ef216894d2d6743e277858dd6da4802` (2026-07-13)
- **Retrieved / pinned:** 2026-07-14 (local clone at `~/calvinchengx/bi-shared-docs`)
- **License:** CC-BY-4.0 (`LICENSE`) for documentation content; MIT (`LICENSE-CODE`)
  for code samples. **Attribution required** — © Microsoft Corporation,
  "Microsoft BI shared documentation," used under CC-BY-4.0.

## Files we rely on (at the pinned SHA)

XMLA wire protocol + encodings:
- `docs/analysis-services/xmla/xml-for-analysis-xmla-reference.md`
- `docs/analysis-services/xmla/xml-data-types/rowset-data-type-xmla.md` — the
  `rowset` shape a DAX `EVALUATE` returns
- `docs/analysis-services/xmla/xml-data-types/mddataset-data-type-xmla.md`
- `docs/analysis-services/instances/analysis-services-schema-rowsets.md` —
  `MDSCHEMA_MEASURES`, `DISCOVER_STORAGE_TABLES` (the tutorial's DMV asset)

Model definition formats (semantic-model item definition parts):
- `docs/analysis-services/tmsl/` — Tabular Model Scripting Language (JSON / `model.bim`)
- `docs/analysis-services/tmdl/tmdl-overview.md` (+ the `tmdl/` dir) — the
  YAML-like text format, TOM-complete

## Related references not in this repo

- **DAX function reference** — https://learn.microsoft.com/en-us/dax/ (corpus:
  `MicrosoftDocs/dax-docs`). Descriptive per-function semantics; **no formal
  grammar or OpenAPI exists** — DAX is a language, and its executable oracle is
  a live Analysis Services engine (Power BI Desktop / DAX Studio / SSAS /
  Azure AS), none bundleable in a pure-Go CI. See
  [docs/18-semantic-model-references.md](../../docs/18-semantic-model-references.md).
- **[MS-SSAS-T]** / **[MS-SSAS]** — the normative open-spec PDFs
  (learn.microsoft.com/openspecs/sql_server_protocols) for the XMLA-based
  tabular commands and the `xmla-rs:rowset` complex type. Pin the exact
  revision if/when the wire layer is implemented.

## Refresh

```sh
git -C ~/calvinchengx/bi-shared-docs fetch origin && git -C ~/calvinchengx/bi-shared-docs checkout <new-sha>
git -C ~/calvinchengx/bi-shared-docs rev-parse HEAD   # update Pinned revision above
```
