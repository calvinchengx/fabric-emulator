# 33 — PBIX tooling: what can read one, what can write one

**Status: nothing adopted. Phase 0 is an afternoon's verification, and the rest
should not be planned until it passes.**

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
4. **MCP is the only supported interface.** A `PBIXBuilder` class exists in
   examples but is explicitly not a supported API. Our harnesses are Python
   scripts and Go tests; driving a stdio MCP server from one is awkward, and
   building on an unsupported internal is how a patch release breaks CI.

And the ordinary caution: v0.9.63, pre-1.0, and every accuracy figure above is
self-reported.

## Phase 0 — verify the claim on our shapes

**An afternoon, and everything else waits on it.**

Take `ContosoRevenue`'s TMSL, build a `.pbix`, and check three things:

1. **Power BI Desktop opens it.** Manual and Windows-only; this hop can never
   run in CI and should not be pretended otherwise.
2. **pbixray reads back what we put in** — the tables, columns and measures.
3. **The DAX agrees** with what `executeQueries` answers for the same query.

Any one of those failing tells us more than a month of planning would.

## Phases 1–2, only if Phase 0 passes

1. **The oracle.** Run the fixture DAX through both engines and diff. Wired
   like `e2e/xmla`: weekly, version-pinned, failing loudly on disagreement —
   because the thing being tracked is another project's engine, which moves.
2. **`.pbix` as a demo artifact** from the import medallion. Blocked for the
   advanced model by the Direct Lake gap above.

## Non-goal

pbix-mcp must **not** become a runtime dependency of the emulator or of
`contoso-data-platform`. It is verification and artifact tooling, held at arm's
length exactly as the JVM Spark oracle is — a thing we check ourselves against,
never a thing the product needs in order to work.
