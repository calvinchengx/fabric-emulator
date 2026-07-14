# 16 — Semantic-model golden references & DAX-oracle strategy

Before any semantic-model / DAX work, this pins the **golden references** we'd
build and validate against, and — the crux — records *where an executable
oracle exists and where it doesn't*. That split decides the whole approach.

See [parity.md](parity.md) for why Power BI is 🔴 today (item management
only, no modeling engine), and [third_party/](../third_party/) for the vendored
references and the provenance pattern.

## The four layers, and whether each has a live oracle

| Layer | Golden reference (paper) | Machine-readable? | Executable oracle (CI-able?) |
|---|---|---|---|
| **Model format** (TMSL/TMDL) | `bi-shared-docs` `tmsl/`, `tmdl/` | TMSL JSON schema; TMDL grammar | ✅ round-trip vs real `.tmdl`/`model.bim` fixtures |
| **DAX language** | learn.microsoft.com/dax (function ref); [MS-SSAS-T] query semantics | ❌ no grammar/OpenAPI | ⚠️ **only a live AS engine** → oracle = *captured (query → rows) fixtures* |
| **XMLA wire** | [MS-SSAS-T], [MS-SSAS] `xmla-rs:rowset`, `bi-shared-docs` XMLA ref | XSD for envelopes/rowsets | ❌ **ADOMD.NET + a real AS server** — native .NET, not endpoint-overridable |
| **executeQueries REST** | `third_party/powerbi-rest-swagger/swagger.json` | ✅ **official OpenAPI (MIT)** | ✅ real REST client + the schema |

## The decisive distinction

Every e2e we ship works by pointing a **real, unmodified client** at the
emulator via an endpoint override (delta-rs, ABFS, azure-sdk, fabric-cicd, the
Livy proxy). Two of these four layers cannot support that:

- **XMLA** — the client is ADOMD.NET/MSOLAP (native .NET) talking a proprietary
  SOAP/rowset protocol; it can't be pointed at a hand-rolled Go server, and
  reimplementing enough Analysis Services to fool it is out of scope and would
  be "mostly faked." So **real SemPy over XMLA stays deferred-with-cause.**
- **DAX** — correctness is defined by a live engine (Power BI / SSAS / AS),
  none of which is pure-Go or CI-runnable. Its golden reference can only be
  **captured `(DAX query → rows)` fixtures**, recorded once from a real engine
  and vendored as test data — a snapshot oracle, weaker than a live client.

Only **executeQueries** has both a machine-readable golden spec *and* a live
oracle. That is why, if the semantic-model engine is built, it should expose
the **executeQueries REST contract** (conforming to the vendored swagger),
backed by a bounded-but-real DAX evaluator — not the XMLA endpoint.

## The DAX oracle on macOS (this dev box)

Power BI Desktop and DAX Studio are **Windows-only** — neither has a macOS
build; on a Mac they run only inside a Windows VM or a cloud PC. macOS-native
options for capturing golden `(query → rows)` fixtures:

1. **executeQueries REST against a real dataset** (best) — callable from macOS
   over HTTP against a Power BI **PPU/Fabric-trial** dataset. Captures goldens
   through *the exact contract we'd implement*, so it doubles as a real-API
   shape check. Needs a Power BI account with a supported capacity.
2. **Hand-computed goldens** — for a small hand-authored model, compute
   expected rows by hand. Fully hermetic and macOS-native, but validates
   against our own arithmetic, so keep the queries unambiguous (SUM / COUNT /
   DIVIDE / single-group `SUMMARIZECOLUMNS`).
3. **Windows VM** — Parallels/UTM (Windows 11 ARM; Power BI Desktop x64 under
   emulation) → DAX Studio capture. Heaviest; only if we need complex DAX.

Recommended starting point: a small authored model with **hand-computed
goldens** for the deterministic core, optionally strengthened by
**executeQueries-REST captures** against a PPU trial when an account is
available.

## Vendored now

- ✅ `third_party/powerbi-rest-swagger/swagger.json` — the executeQueries golden
  OpenAPI (MIT, pinned `c7755706`).
- ✅ `third_party/bi-shared-docs/PROVENANCE.md` — XMLA/rowset/TMSL/TMDL specs,
  pinned by reference (SHA `328074568`, CC-BY-4.0).

Still to identify before implementation: a **golden model fixture** (a small
TMDL/TMSL model mirroring the tutorial's Store/Time/Sales + `TotalUnits` /
`Total Units This Year|Last Year` measures) and its captured `(query → rows)`
goldens.
