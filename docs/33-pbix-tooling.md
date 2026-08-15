# 33 — PBIX tooling: what can read one, what can write one

**Status: Phase 0 and Phase 0b BOTH RUN and passed.** A `.pbix` built from this
platform's own TMSL is opened by **Microsoft's Power BI Desktop**, which
evaluates the fixture DAX to bit-identical numbers — measured on a GitHub
Windows runner, 5 runs out of 5. Nothing is adopted yet; what changed is that
the ceiling on what can be claimed has moved, and one of the two documented
blockers turned out not to exist.

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

### What Phase 0 did not establish — closed by Phase 0b

**Power BI Desktop was not run** in Phase 0. Two independent Python
implementations agreed about the file; no Microsoft code had read it. That was
a weaker claim than "verified `.pbix`", and the difference is exactly the kind
`docs/10` already records twice — so it was left stated rather than glossed,
and then closed.

The gap turned out to be **closable**, and more cheaply than first written here. This
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

## Phase 0b — RUN, 5 of 5

[`e2e/pbix-desktop`](../e2e/pbix-desktop/) on `windows-latest`. Every stage
reached, on every attempt:

```
STAGE download :: OK (698 MB)
STAGE install  :: OK
STAGE locate   :: OK C:\Program Files\Microsoft Power BI Desktop\bin\PBIDesktop.exe
STAGE launch   :: OK pid=9396
STAGE port     :: OK …\AnalysisServicesWorkspace_3da091a2…\Data\msmdsrv.port.txt
analysis services port: 63321
STAGE connect  :: OK
STAGE query    :: OK
DESKTOP AGREES WITH executeQueries: True
```

| | |
|---|---|
| Pass rate | **5 / 5** (run 30821205384) |
| Duration | 6m41s – 8m05s per attempt |
| Divergence | `rel = 0.000e+00` on all six values — **bit-identical**, not merely inside tolerance |

Bit-identical is worth stating precisely. pbix-mcp agreed with us to `1.48e-16`
— one ulp, the signature of two engines summing in a different order. Desktop
agrees exactly, which says it is summing the same doubles the same way.

### Both documented blockers were wrong, and only running it could show that

- **Windows Server.** Microsoft says use a *client* Windows; `windows-latest`
  is Server 2025. It ran anyway — the stated rationale (IE Enhanced Security
  blocking sign-in to the Power BI service) genuinely does not apply to opening
  a local file, and the guidance turned out to be about the rationale.
- **Display.** Desktop documents a 1440x900 minimum and no system account. A
  hosted runner's virtual display under `runneradmin` satisfied it.

Neither could be settled by reading. Both were settled in eight minutes.

### What is still NOT established

The **licence** question. Microsoft documents `-quiet ACCEPT_EULA=1` as a
supported install switch, which is a vendor contemplating automation — but that
is the docs, not the EULA text, and this file is not the place a legal reading
belongs. Automated *install* is evidenced; automated *use in CI* is a decision
for a human, and it is the one thing here no amount of running will settle.

Also unmeasured: durability. Five runs on one afternoon against one Desktop
build is a pass rate, not a trend. Desktop ships monthly, which is why the
suite is scheduled weekly rather than run once and believed.

## Phases 1–2 — and Phase 0b reorders them

Both phases were scoped with pbix-mcp as the DAX oracle, because at the time it
was the only implementation available. Phase 0b changes that: **Power BI
Desktop is the better oracle and is now reachable.** A conformance suite should
be built against the real engine, with pbix-mcp as the cheap fallback — the
reverse of the original plan.

The argument is not preference. pbix-mcp's goldens are *captured from* Desktop,
so a suite built on them is a copy of an oracle we can now query directly; and
Phase 0 found its internal engine returning wrong answers silently, which is
not a thing an oracle may do. Desktop costs ~8 minutes and a 698 MB download
per run — real, and the reason this is weekly rather than per-commit.

1. **The oracle.** Grow `e2e/pbix-desktop` from one query to a corpus, diffing
   `internal/semanticmodel` against Desktop. This is what turns
   [24-parity-completion.md](24-parity-completion.md)'s open-ended "Full DAX"
   `L` into evidence-driven work: every function added is one Desktop agreed
   about, rather than one somebody read the docs for. pbix-mcp stays as the
   fast local check — **through its MCP tool layer, never the internal
   engine.** How a Mac or Linux **developer** reaches that same `msmdsrv`
   without waiting for the weekly Windows job is
   [52-msmdsrv-hosts.md](52-msmdsrv-hosts.md) — UTM on a Mac you own,
   `dockur/windows` on Linux+KVM metal, Desktop on a Windows host. Not a
   compose default, and not GitHub `macos-latest` / `ubuntu-latest`
   (nested Windows is not a PR job). Every-push CI on those runners tests
   the Go subset against the goldens this oracle produces.
2. **`.pbix` as a demo artifact** from the import medallion. Now genuinely
   deliverable, since Desktop is confirmed to open what we build. Still blocked
   for the *advanced* model by the Direct Lake gap above.

## Non-goal

pbix-mcp must **not** become a runtime dependency of the emulator or of
`contoso-data-platform`. It is verification and artifact tooling, held at arm's
length exactly as the JVM Spark oracle is — a thing we check ourselves against,
never a thing the product needs in order to work.
