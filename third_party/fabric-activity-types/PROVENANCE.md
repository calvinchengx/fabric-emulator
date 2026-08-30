# Fabric `DataPipelineActivityTypes` — pinned by content hash

Microsoft's own list of data-pipeline activity type discriminators: the oracle
`internal/api/fabricActivityTypes` is held to, and the reason the dispatch can
be checked for completeness at all.

**Why not a commit SHA.** `third_party/README.md` asks for a pinned upstream
revision. This artifact has none available: the article is a REST-API reference
published as generated HTML, and MicrosoftDocs/fabric-docs does not carry it
(the `docs-api/...` path 404s). The pin is therefore the sha256 of the extracted
table. That cannot distinguish an upstream edit from a bad fetch, so a `--check`
diff is **read**, never rubber-stamped.

## Provenance

- **Upstream:** https://learn.microsoft.com/en-us/rest/api/fabric/articles/item-management/definitions/datapipeline-definition
- **Section:** `DataPipelineActivityTypes`
- **Retrieved / pinned:** 2026-08-30
- **Integrity:** `sha256:e2d754880fdf2cebd6319ee6da3338d2cfc4e3a3f4b3ff3fc605fb1a6f2d56a8`, 2742 bytes (`activity-types.json`)
- **License:** © Microsoft Corporation. Microsoft Learn content is published
  under CC-BY-4.0; only the type NAMES and their one-line descriptions are
  extracted, as the factual list this repo conforms to.
- **Used by:** `scripts/check_fabric_activity_types.py` (in `make check`), which
  holds `internal/api/fabricactivitytypes.go` to this table; that list in turn
  drives `TestEveryDocumentedFabricActivityTypeIsHandled`.

## Refresh

    scripts/vendor_fabric_activity_types.py            # re-fetch and re-pin
    scripts/vendor_fabric_activity_types.py --check    # diff without writing

`--check` needs the network and is deliberately NOT part of `make check`: the
offline checker compares the Go list against this file, so the gate stays
runnable with no network and an upstream change is a deliberate, reviewable
re-pin rather than a surprise red.
