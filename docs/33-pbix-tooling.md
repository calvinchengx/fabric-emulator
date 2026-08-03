# 33 — PBIX tooling: what can read one, what can write one

**Status: Phase 0 RUN and passed** against pbix-mcp 0.9.78 / pbixray 0.15.3.
Nothing adopted yet — Phases 1–2 are live options, not decisions, and Phase 0b
(does Power BI Desktop itself read the file) is the honest next question.

This exists because a belief in this project was wrong, in the direction that
matters: a `.pbix` carrying a data model was recorded as not programmatically
producible, and it is. Getting that backwards shaped two conversations about
what the emulator could ever hand a BI user, so the correction is written down
rather than remembered.

[32-xmla-plan.md](32-xmla-plan.md) is the neighbouring plan — the two are the
read and write halves of the same "make the semantic model reachable by real BI
tools" question.

## The formats, and who can actually do what

| Format | What it is | We can emit it? |
|---|---|---|
| **TMSL** (`model.bim`) | The model, as JSON. What Fabric's REST API takes | ✅ already — `semantic_model.py` publishes one |
| **TMDL** | The model, as a folder of readable text | ✅ the emulator parses it; emitting is mechanical |
| **PBIP** | Report + model as plain text files. What Fabric git integration writes | ✅ scoped — `.platform` + `definition.pbism` + `model.bim` |
| **`.pbit`** | The zip container, **without** data | ✅ via `pbi-tools compile` |
| **`.pbix`** | The zip container, **with** data — ABF/VertiPaq inside | ⚠️ see below |

The `.pbix` row is the one that was wrong here. Two true facts had been
generalised into a false one:

- Microsoft's own answer is Desktop-only. The PBIP docs state it plainly:
  *"Can I convert PBIX into PBIP and vice-versa programmatically? **No.**"*
- `pbi-tools compile` produces `.pbix` **only for report-only ("thin")
  projects**; anything carrying a model must compile to `.pbit`.

Both remain true *of those tools*. Neither establishes the general claim, and
[pbix-mcp](https://github.com/d0nk3yhm/pbix-mcp) is the counter-example: a pure
Python reimplementation of every layer — ZIP shell, JSON layouts, ABF binary
containers, VertiPaq column storage, XPress9 compression, SQLite metadata — with
no Power BI Desktop, no Windows, no Microsoft binaries, and no dependency on
pbixray. MIT.

## What is worth having, in order

### 1. A DAX conformance oracle — the real prize

pbix-mcp ships **per-function goldens captured from Power BI Desktop's own
engine**: 435 of the 467 functions (the other 32 being Desktop's own refusals),
plus full-corpus verification across 432 grand totals and 1,705 filter-context
cells.

[`internal/semanticmodel`](../internal/semanticmodel/) is a deliberately bounded
evaluator, and [24-parity-completion.md](24-parity-completion.md) carries "Full
DAX" as an open-ended `L` precisely because growing it has had no oracle — every
addition is someone's reading of the docs. A Desktop-derived corpus converts
that into evidence-driven work.

This is the same move `e2e/xmla` made for the ADOMD.NET contract and
`e2e/notebook-run/run-jvm.py` made for Spark: a real implementation as ground
truth for the emulated one. **It is worth having even if no `.pbix` is ever
shipped.**

### 2. The artifact loop, closed

Our TMSL → a `.pbix` Desktop opens → which **pbixray can read**. That finally
gives pbixray a job: an independent cross-check on pbix-mcp's output, rather
than the producer it can never be (it reads `.pbix`/`.xlsx`/`.abf` and writes
nothing).

Two independent implementations disagreeing about a file we generated is a
useful signal. One implementation agreeing with itself is not.

## What bites US specifically

Not general criticism — these are the four places our shapes meet its edges:

1. **Partitions cannot be added to a PBIX.** It needs VertiPaq storage for
   them; reading and removing existing partitions works. We are in the middle
   of adding partitions to the semantic model, so an emitted `.pbix` would
   diverge from our TMSL on exactly the field being introduced.
2. **Direct Lake is unmentioned.** Import and DirectQuery are covered. The
   advanced medallion's model is Direct Lake, so anything here applies to the
   import model only.
3. **VertiPaq is verified to ~11 tables / 72 columns / 121K rows.** The
   medallion runs 100K customers × 101 columns — at the edge of tested scale
   rather than inside it.
4. **MCP is the only supported interface** — and Phase 0 showed this is a
   correctness boundary, not just a support boundary. `PBIXBuilder` writes
   files correctly; the internal DAX engine returns wrong answers without
   erroring. Details below.

And the ordinary caution: pre-1.0 (0.9.78 as measured), and every accuracy
figure quoted from the project is self-reported — the table in Phase 0 below is
not, having been measured here.

## Phase 0 — RUN, and it passed

Measured against `pbix-mcp 0.9.78` and `pbixray 0.15.3`, both installed into a
throwaway venv — nothing entered `uv.lock`, `pyproject.toml`, or either
project's dependency set.

The model under test is the **real** one: `semantic_model.definition()` imported
with stubs, so what was built is what the platform publishes, not a
transcription of it. Row data is `gold/_export.json`.

| Check | Result |
|---|---|
| Build a `.pbix` from our TMSL | ✅ 2,088,332 bytes — `Version`, `[Content_Types].xml`, `DiagramLayout`, `Settings`, `Metadata`, `Report/Layout`, `DataModel` (2.1 MB) |
| Metadata survives, read by **pbixray** | ✅ 2 tables, 8 columns, 3 measures with exact DAX, 1 relationship typed `M:1` / `Single` |
| Row data survives | ✅ Customer 100,000 rows, Revenue 84 — `SUM(Revenue)` = 75,182,346.1 and `SUM(Units)` = 742,749, both matching `_export.json` exactly |
| DAX agrees with `executeQueries` | ✅ worst **relative** divergence `1.48e-16`, below IEEE double epsilon (`2.2e-16`) |

The scale caveat above is now partly discharged: 100,000 rows is inside what
was built and read back, not merely inside what the author tested.

### The finding that matters more than the numbers

**The internal DAX engine answers wrongly, silently.** Driven through
`pbix_mcp.dax.engine.evaluate_measure`, a filter context on the *dimension*
table returned the **grand total** for every group — 75,182,346.1 for GB, SG
and US alike — rather than an error. No relationship shape fixed it: as-read,
reversed, with explicit cardinality, with a both-directions flag. Filtering the
*fact* table's own column worked and matched us exactly, which is what makes
the failure convincing rather than obvious.

Through the **supported tool layer** (`pbix_open` → `pbix_evaluate_dax_grouped`)
the same query is correct to the last bit, and the tool *validates its input* —
`Customer[Country]` is rejected with `DIMENSION_INVALID`, demanding
`Customer.Country`.

So the README's "MCP is the only supported interface" is not a licensing
preference to be routed around. `PBIXBuilder` is fine for *writing* — it built
the file above. The internal DAX engine is not fine for reading, and a Phase 1
oracle built on it would have produced confident, wrong goldens.

### What Phase 0 does NOT establish

**Power BI Desktop was not run.** Two independent Python implementations agree
about this file; no Microsoft code has read it. That is a weaker claim than
"verified `.pbix`" and the difference is exactly the kind `docs/10` already
records twice.

That gap is **closable**, and more cheaply than first written here. This
repository already runs `windows-latest` in five CI jobs (`ci.yml`,
`make-targets.yml`), and Power BI Desktop is a free download. Desktop also
hosts a local Analysis Services instance, which is how Tabular Editor and DAX
Studio attach — so a Windows job could open the `.pbix`, discover the
`msmdsrv` port, and query it over ADOMD.NET, which `e2e/xmla` already proves we
can drive. That would upgrade the claim from "two Python libraries agree" to
"Desktop loaded it and answered."

It is a spike, not a given: a GUI app on a headless runner, Desktop's
auto-update moving under the assertion, and the EULA's position on automated
use all need checking first. Scoped as **Phase 0b** below.

## Phase 0b — let Power BI Desktop read it

A spike on a `windows-latest` runner, in this order, stopping at the first
answer that kills it:

1. **Licence.** Does the EULA permit automated/CI use? If not, everything below
   is moot and the honest ceiling stays where Phase 0 left it.
2. **Install.** Power BI Desktop on an Actions runner — the standalone
   installer rather than the Store package, which is awkward there.
3. **Attach.** Open the `.pbix`, discover the `msmdsrv` port, connect over
   ADOMD.NET. `e2e/xmla` already drives that client, so this is reuse.
4. **Assert.** Run the fixture DAX through Desktop's own engine and diff
   against `executeQueries` — the same comparison Phase 0 ran, against the real
   implementation instead of a reimplementation of it.

Treat flakiness as a first-class result: a GUI app driven headlessly that
passes four times in five is not an oracle, it is a coin. Decide on the
measured rate, not on whether it ever went green.

## Phases 1–2, only if Phase 0 passes

**Phase 0 passed**, so both are live options.

1. **The oracle.** Run the fixture DAX through both engines and diff. Wired
   like `e2e/xmla`: weekly, version-pinned, failing loudly on disagreement —
   because the thing being tracked is another project's engine, which moves.
   **Through the MCP tool layer, never the internal engine** — see above.
2. **`.pbix` as a demo artifact** from the import medallion. Blocked for the
   advanced model by the Direct Lake gap above.

## Non-goal

pbix-mcp must **not** become a runtime dependency of the emulator or of
`contoso-data-platform`. It is verification and artifact tooling, held at arm's
length exactly as the JVM Spark oracle is — a thing we check ourselves against,
never a thing the product needs in order to work.
