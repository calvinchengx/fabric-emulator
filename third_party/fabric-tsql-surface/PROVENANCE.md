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
  `unsupported.json` to the page's Limitations list and holds each row to what the
  endpoint's write guard (`isEndpointWrite`) and Class B strict mode
  (`tsql.CheckStrict`, [docs/29](../../docs/29-tsql-parity.md)) do with its probe
  statements.

## `unsupported.json` is ours, not Microsoft's

`text` is Microsoft's, verbatim, one row per bullet under **Limitations**. Every
other field is this repository's classification. Two things here can refuse a
statement Fabric does not support, and they are different in kind:

- the **guard** — the lakehouse SQL analytics endpoint's write guard. Always on,
  the endpoint only, and it judges what a batch *writes*;
- **strict mode** — `-tsql-strict` / `FABRIC_TSQL_STRICT`. **Off by default**,
  because it removes capability; applies to every TDS connection, endpoint and
  Warehouse alike.

| Field | Values |
|---|---|
| `guard` | `refused` — every probe is refused, so it never reaches the engine. `forwarded` — it reaches the engine. `unspecified` — the sentence names no statement |
| `strict` | `refused` — strict mode refuses every probe, and `feature` is the name it gives. `not-enforced` — it does not, with the reason in `reason`. `unspecified` |

Read together, for the 16 Limitations: the guard refuses 6 (on the endpoint only),
strict mode refuses 10, the two refuse 12 between them, three are refused by
neither — `FOR JSON` in a subquery, names containing `/` or `\`, and the vector
type — and one names no statement. Neither measures the engine: a statement that is
"forwarded" may still be rejected by SQL Server, and for most of these it is not.

## Refresh

    curl -sfL https://raw.githubusercontent.com/MicrosoftDocs/fabric-docs/<sha>/docs/data-warehouse/tsql-surface-area.md \
      -o third_party/fabric-tsql-surface/tsql-surface-area.md

Bump the pin in its own commit, update the sha256 and size above, and read the
diff: a bullet added under Limitations fails the test until it is classified.
