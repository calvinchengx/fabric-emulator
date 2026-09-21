# Fabric T-SQL surface area — the page, and what this emulator does with it

Microsoft's statement of which T-SQL a Fabric SQL analytics endpoint or
Warehouse does not support. The nearest thing to a spec for "what may a client
send here", which no OpenAPI document covers.

## Provenance

- **Upstream:** https://github.com/MicrosoftDocs/fabric-docs, `docs/data-warehouse/tsql-surface-area.md`
  (rendered at https://learn.microsoft.com/en-us/fabric/data-warehouse/tsql-surface-area)
- **Pinned revision:** `91e4905639b7727f44376feff3f0134bf95f5370` (2026-09-09, "Fabric august 2026 whats new digest (#16331)")
- **Retrieved:** 2026-09-21
- **Integrity:** `sha256:a276907a82078476fbbbaa6d795e0ee5832b707a7bf7740cc347683badcff71c`, 5920 bytes (`tsql-surface-area.md`, byte-for-byte upstream)
- **License:** © Microsoft Corporation, CC-BY-4.0 (the repository's license is
  CC-BY-4.0 for documentation). Copied in full, unmodified, with this
  attribution: a single page, and the file is the tamper check.
- **Used by:** `internal/tds/tsqlsurface_test.go`, which holds
  `unsupported.json` to the page's Limitations list and holds each row's
  `endpoint` to what `isEndpointWrite` does with its probe statements.

## `unsupported.json` is ours, not Microsoft's

`text` is Microsoft's, verbatim, one row per bullet under **Limitations**. Every
other field is this repository's classification of the lakehouse endpoint's
write guard:

| `endpoint` | Meaning |
|---|---|
| `refused` | the guard refuses every probe statement, so it never reaches the engine |
| `forwarded` | the guard passes it to the engine. Fabric documents it as unsupported; this emulator does not refuse it. **A divergence**, each with a written `reason` |
| `unspecified` | the sentence names no statement, so there is nothing to probe |

`forwarded` measures the guard, not the engine: SQL Server may still reject the
statement, and for most of these it does not.

## Refresh

    curl -sfL https://raw.githubusercontent.com/MicrosoftDocs/fabric-docs/<sha>/docs/data-warehouse/tsql-surface-area.md \
      -o third_party/fabric-tsql-surface/tsql-surface-area.md

Bump the pin in its own commit, update the sha256 and size above, and read the
diff: a bullet added under Limitations fails the test until it is classified.
