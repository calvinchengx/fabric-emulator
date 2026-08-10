# 45 — Reverse-engineering Power BI, and what it demands of this emulator

## What this is, and what it is not

A synthesis of a published methodology for reverse-engineering Power BI reports
during a BI migration, credited to Aman Nandan's playbook on the subject. **This
document is a summary in our own words, not a copy**, because the source is
someone else's article and belongs to them; the value added here is the last
section, which the source has no reason to contain.

It is in this repo for one reason. The playbook is the best available
**demand specification** for a Fabric emulator. It was written to describe how a
migration team extracts metadata from a real Power BI estate, so it enumerates,
concretely and from practice, exactly which surfaces such a team touches. Every
one of those is a candidate parity row. A demand map assembled from what real
practitioners actually call beats one assembled from what a specification
contains.

## The core claim

Treat a Power BI report as a database of metadata rather than as a picture.
Every layer, from the pixel on the canvas down to the physical source column, is
serialised somewhere machine-readable, so reverse engineering is a series of
JOINS OVER EXTRACTS rather than an expedition through a UI.

Why it is hard, in one line each:

- Logic is spread across five layers and any one can silently change a number.
- Filter context is invisible: the same measure returns different values per
  visual, and none of that is in the measure's definition.
- Names lie: `Revenue` may be `AMT_NET_LCL_03`, and `Sales` may be four tables
  with anti-joins.
- Dead logic accumulates; migrating it all triples scope, and guessing what is
  unused breaks a scheduled export nobody mentioned.
- The DAX definitions are often the only written record of a business rule, so
  losing one is a business failure rather than a technical one.

## The five-layer stack

    Layer 5  VISUAL       fields per visual; visual/page/report filters; slicers;
                          bookmarks; RLS
    Layer 4  DAX          measures, calculated columns/tables, calculation
                          groups; the dependency graph
    Layer 3  MODEL        tables, columns, types, relationships, cardinality,
                          cross-filter direction, hierarchies
    Layer 2  POWER QUERY  M per table; merges, appends, pivots, native queries,
                          parameters, dataflows
    Layer 1  SOURCE       databases, views, files, APIs; physical
                          schema.table.column

Reverse engineering walks this stack downward until every field reference
terminates at a physical source column. Two properties make it tractable: each
layer has a machine-readable form (PBIR/`report.json`, TMDL/`model.bim`, DAX
`INFO` functions and DMVs, M in partitions), and **the layers join on names**.

## The seven phases, compressed

0. **Inventory and acquisition.** Find every artifact and get extractable
   copies. The trap is the thin report: report and semantic model are often
   separate items in different workspaces, so a report PBIX alone gives visuals
   with no model. Freeze a numeric baseline before decoding anything.
1. **Model metadata extraction.** Five DAX queries (`INFO.TABLES/COLUMNS/
   MEASURES/RELATIONSHIPS/PARTITIONS`) dump the whole structural skeleton.
2. **Source tracing through Power Query.** Read each M expression as four zones:
   source step, native query, structural transforms, type cleanup. Recurse
   through referenced queries, dataflows, and composite chains.
3. **DAX decoding.** Build the dependency graph FIRST
   (`$SYSTEM.DISCOVER_CALC_DEPENDENCY` / `INFO.CALCDEPENDENCY()`), then read
   measures in dependency order, annotating the constructs that carry meaning:
   `ALL*` variants, `USERELATIONSHIP`, `TREATAS`, iterator grain, time
   intelligence, context transition, hardcoded literals.
4. **Visual layer decoding.** Parse report JSON for fields and filters. The
   shortcut that beats all interpretation is Performance Analyzer: it yields the
   literal DAX a visual executes, with every filter level and calculation group
   already merged.
5. **Assembling the lineage matrix.** Join the extracts into one table, one row
   per (metric, contributing source column), plus a metric dictionary, a source
   bill of materials, a logic-relocation map, and a risk register.
6. **Validation.** Three levels: reproduce the visual's own query, reproduce it
   from source, then test behavioural parity (slicers, TopN, drillthrough,
   bookmarks, per RLS role).

The discipline underneath all of it: **extract first, interpret second.**

## What it demands of this emulator

This is the part worth acting on. Each surface below is something the playbook
tells us a real migration workload calls. Status measured 2026-08-10 by grepping
`internal/`, so treat it as a pointer, not a parity grade: `docs/parity.md` is
authoritative.

| Surface | Playbook phase | Emulator |
|---|---|---|
| `executeQueries` (DAX over REST) | 1, 3, 6 | implemented, graded green |
| Scanner / `WorkspaceInfo.getInfo` lineage | 0 | present |
| Activity events (usage evidence) | 4 | present |
| TMDL | 0, 1 | present |
| **`INFO.*` DAX functions** | **1** | **absent** |
| **`$SYSTEM.*` DMVs, incl. `DISCOVER_CALC_DEPENDENCY`** | **3** | **absent** |
| **XMLA endpoint** | 0, 1, 3, 4 | **absent** (connect path measured, see doc 32) |
| PBIR report JSON | 4 | thin |

Three observations follow, and the middle one is the useful one.

**The `INFO.*` functions may be the cheapest high-value surface in the repo.**
The playbook calls them "the single highest-leverage technique in the whole
playbook", and they are DAX queries — which means they arrive through
`executeQueries`, a route that is already implemented and green. `INFO.TABLES()`,
`INFO.COLUMNS()`, `INFO.MEASURES()`, `INFO.RELATIONSHIPS()` and
`INFO.PARTITIONS()` are projections over model metadata this emulator already
holds in `internal/semanticmodel`. No new transport, no SOAP, no rowset codec.

**This sharpens the XMLA sizing question in doc 32.** The open question there is
which parts of the ecosystem genuinely need XMLA rather than REST. This playbook
answers part of it from practice: Phases 1 and 3 are described as `INFO`/DMV
work, and DMVs are XMLA-only, while the DAX evaluation of Phase 6 goes through
`executeQueries`. So the demand splits, and the XMLA-only remainder looks
narrower than "implement `[MS-SSAS-T]`".

**The playbook is also a test oracle.** Its phases are a realistic end-to-end
workload: scan an estate, dump model metadata, walk the dependency graph,
reconcile numbers. An e2e that runs those phases against the emulator would
exercise more surfaces in one scenario than most single-API suites, and its
assertions are already written as the phase exit criteria.

## Where the analogy stops

Two honest limits.

The playbook assumes a REAL estate with real history: dead measures, renamed
columns, dataflow chains, years of debris. A seeded emulator has none of that,
so a suite modelled on this cannot claim to witness the hard part of the
methodology — it witnesses that the API surfaces respond.

And this repo grades parity by third-party witness. Reading a playbook tells us
what to build; it does not witness anything. A green row still needs a real
client (`sempy`, Tabular Editor, DAX Studio) driving these surfaces in CI.

Related: [24-parity-completion.md](24-parity-completion.md),
[32-xmla-plan.md](32-xmla-plan.md),
[18-semantic-model-references.md](18-semantic-model-references.md),
[33-pbix-tooling.md](33-pbix-tooling.md)
