# Notebook capability parity — the plan

> **Status: Phase 0 delivered; the rest is a draft for discussion.** Axis A is
> now measured against a cited reference rather than estimated — 44 documented
> members, 25 of them wrong or missing. Axis C rows are still search results,
> not probe results, and each needs measuring before it becomes a claim. Companion to
> [38-framework-conformance.md](38-framework-conformance.md) and
> [39-run-multiple-parity-plan.md](39-run-multiple-parity-plan.md), whose
> "what done buys, precisely" convention this follows.

The runtime conformance matrix is **18 of 18**. That proves seven structural
contracts on three backends, and it is **not** capability parity with real
Fabric. This document is the distance between the two, and the order it would
have to be closed in.

## What 18 of 18 licenses you to say

Seven contracts, each proven by executing on a real backend and confirmed by
something that is not the component doing the work. They were chosen because
each had produced a *silent wrong answer*: a write that reported success and
landed nowhere, a context that answered from an environment fallback while both
control-plane links were broken, a token that died mid-run and read as a storage
outage.

That is a meaningful bar — it is the bar that catches false greens. It is a bar
about **failure classes**, not about surface coverage.

Two limits are load-bearing:

- **Contract 2 is titled "the API shape is the contract, independent of
  behaviour."** A method can carry every documented parameter, in the right
  order, and do entirely the wrong thing. That cell stays green.
- **Contract 2 grades one module.** `notebookutils-reference.json` now cites
  all eight documented namespaces (Phase 0), but `graded_by_contract_2` names
  the single one the live probe actually asserts. Cited is not graded, and
  reading a citation as a check is how a partial reference gets mistaken for
  coverage.

## Parity is four axes, not one

"Notebook capability parity" collapses four independent questions. Each has a
different owner, a different kind of evidence, and a different failure mode.

| | Axis | What it asks |
|---|---|---|
| **A** | The utils surface | What `notebookutils.*` exposes, and whether each member exists with the documented signature. Enumerable, therefore gradeable. |
| **B** | Behaviour | Whether those members *do* what Fabric's do. Not derivable from a signature. |
| **C** | The execution model | Magics, the parameters cell, notebook resources, the Files mount, session lifecycle — what the cell runs *inside*. |
| **D** | The engine | Whether Spark behaves like Spark. Already owned by [engine-matrix.md](engine-matrix.md), row by row. Out of scope here. |

## Axis A — the surface, measured

**Phase 0 is done, so this table now has a denominator.** The module list is
not ours: it is the table on the [NotebookUtils overview
page](https://learn.microsoft.com/en-us/fabric/data-engineering/notebook-utilities),
read 2026-08-04, which is what lets it name a module nobody here thought to look
for. Every member in `notebookutils-reference.json` carries its source page and
that page's own last-updated date.

**44 documented members. 19 are correct. 17 are absent and 8 more exist with the
wrong parameter names.**

| Module | Documented | Present | Absent | Signature mismatches |
|---|---:|---:|---:|---:|
| `notebook` | 11 | 11 | 0 | 0 |
| `fs` | 15 | 8 | 7 | 5 |
| `lakehouse` | 8 | 3 | 5 | 3 |
| `credentials` | 4 | 2 | 2 | 0 |
| `session` | 2 | — | 2 | — |
| `udf` | 1 | — | 1 | — |
| `runtime` | 1 | 1 | 0 | 0 |
| `variableLibrary` | 2 | 2 | 0 | 0 |
| **Total** | **44** | **27** | **17** | **8** |

Absent: `fs.fastcp`, `fs.mv`, `fs.getProperties`, `fs.mount`, `fs.unmount`,
`fs.mounts`, `fs.getMountPath`, `lakehouse.update`, `lakehouse.delete`,
`lakehouse.getWithProperties`, `lakehouse.listTables`, `lakehouse.loadTable`,
`credentials.putSecret`, `credentials.isValidToken`, and the whole of
`session` and `udf`.

### The eight that exist and would fail anyway

This is the finding worth the phase. These methods are shipped, work, and are
used — and a framework that introspects them **declines to run**, because
contract 2's asymmetry is about names, not count:

| Member | Documented | Shipped |
|---|---|---|
| `fs.put` | `(file, content, overwrite)` | `(path, content, overwrite)` |
| `fs.head` | `(file, max_bytes)` | `(path, maxBytes)` |
| `fs.append` | `(file, content, createFileIfNotExists)` | `(path, content)` |
| `fs.cp` | `(src, dest, recurse)` | `(src, dst)` |
| `fs.rm` | `(path, recurse)` | `(path, recursive)` |
| `lakehouse.get` | `(name, workspaceId)` | `(lakehouseId, workspaceId)` |
| `lakehouse.create` | `(name, description, definition, workspaceId)` | `(name, description, workspaceId)` |
| `lakehouse.list` | `(workspaceId, maxResults)` | `(workspaceId)` |

`dst` for `dest`, `recursive` for `recurse`, `maxBytes` for `max_bytes`,
`lakehouseId` for `name` — each is the reasonable spelling somebody would pick
writing the method from its description rather than from the page. Which is
precisely how this reference came to be needed.

**None of these is caught today**, because contract 2 grades one module and
`fs`/`lakehouse` are not it. Phase 1 is what turns all 25 into red cells.

### What the old scope field got right, and what it hid

Before Phase 0 the reference declared its scope in one field,
`modules_not_yet_covered` — eight modules contract 2 did not grade. It was the
most useful line in the file, and it flattened a distinction that decides how
much work each entry is:

| Declared uncovered | Actual state | What the entry means |
|---|---|---|
| `fs`, `credentials`, `env`, `runtime`, `lakehouse`, `variableLibrary` | exists, ungraded | Shipped code nothing checks. Work is **go and check** — and expect some of it to be wrong, because nothing has ever failed on it. |
| `session`, `udf` | does not exist | Not written. Work is **go and build**. Same list, different order of magnitude. |
| `mssparkutils` | on neither | Absent from the tree *and* absent from the list of known absences. |

The deeper limit was structural: **the list was bounded by what its author knew
to list.** An honest record of known absences, and by construction unable to
name a module nobody thought of.

Reading the surface from Microsoft's own pages settled it, and the list was
wrong in both directions:

- **`env` is not a documented module.** The overview table has eight
  namespaces and `env` is not among them. What it answered — workspace id,
  lakehouse id — are keys on `notebookutils.runtime.context`. It is an
  mssparkutils-era holdover this shim still ships. Harmless (contract 2 allows
  extra surface) but it must not be counted as parity, and nothing should be
  built against it.
- **`mssparkutils` was missing from the record of what is missing** — absent
  from the tree and absent from the list, while the shim's own docstrings
  reference it. The overview states the rename is complete, that old code stays
  backward compatible, and that **the namespace will be retired**. Emulating a
  namespace Microsoft is retiring is a different decision from emulating the
  current one, and it is now recorded as a decision rather than an oversight.

The file now separates the two things that field conflated: every module is
**cited**, and `graded_by_contract_2` names the far smaller set the live probe
actually asserts. Reading a citation as a check is how a partial reference gets
mistaken for coverage.

## Axis C — the execution model

What the cell parser handles today, read from `internal/notebook/parse.go`.
"Absent" means no handler was found by search, **not** that it was tried and
failed — each row needs a probe before it becomes a claim.

| Capability | State | Note |
|---|---|---|
| `%%sql` / `%%pyspark` / `%%lang` | parsed | Recognised by the cell parser. |
| parameters cell | parsed | Position handled; behaviour has its own tests. |
| `%%configure` | absent | Session sizing and conf at cell scope. |
| `%run` | absent | Distinct from `notebook.run` — shares the caller's session. |
| `%%spark` (Scala) / `%%sparkr` | absent | Language cells other than Python and SQL. |
| `%%html` / `%%markdown` | absent | Output-shaping magics. |
| notebook resources (`builtin/`) | absent | Per-notebook file storage. |
| `display()` / `displayHTML()` | unverified | No hit in the shim; needs measuring before it is claimed either way. |
| Files mount | divergent | One mount point, refuses a second lakehouse rather than switching — 2c′ in [37-runtime-fidelity-gaps.md](37-runtime-fidelity-gaps.md). |

## The part that makes all of it provisional

Every green in this repo rests on Microsoft's published contracts and on
unmodified third-party clients. That is the strongest evidence available here,
and it is **still not Fabric**. The differential workflow against a real tenant
exists, is gated behind repository secrets, and **no parity row cites it**.

So the honest ceiling on Axes A–C, absent Phase 5, is *"conforms to the
published contract"* — never *"matches Fabric."* Those diverge exactly where the
documentation is wrong or silent, and that is precisely where an emulator is
most likely to be wrong too, because both were built from the same page.

**Recent evidence for taking this seriously.** Three limitations recorded in
`docs/38` as measured facts turned out to be wrong on inspection this week: the
JVM overlay "cannot reach OneLake by path" (it always could — one probe spelled
a URL scheme differently from the rest of the tree), a catalog entry vanishing
"for reasons not established" (sail's credential refresh restarts the engine),
and `OPTIMIZE <name>` failing product-wide (sail only, and not for the predicted
reason). Each was written from a real measurement and then generalised one step
past what the measurement supported. All three survived because **nothing failed
while they were wrong.**

## The plan

Phases are numbered because they genuinely sequence: each is unbuildable, or
merely decorative, without the one above it.

### Phase 0 — Cite the surface

*Blocks 1–4.*

Extend `notebookutils-reference.json` from one module to all ten, every member
carrying its source page and that page's own last-updated date — the discipline
the file already applies to `notebook`.

First because without it there is no denominator and every later phase is
guesswork wearing a number. Also the cheapest phase, and the one most likely to
be skipped.

**What it buys.** A defensible target. Nothing about behaviour and no new
capability — but every subsequent claim becomes checkable, and a stale citation
becomes findable without re-reading every page.

### Phase 1 — Grade shape across every module

*Needs 0.*

Contract 2 runs against all ten modules instead of one. Missing members fail;
extra members pass — the asymmetry is the contract, because a framework
introspects a signature and declines to run when a parameter is absent, without
ever calling anything.

**What it buys.** Every absent member becomes a red cell instead of an unasked
question. Still says nothing about behaviour. Expect this to go red on first
run — that is its job.

### Phase 2 — Close the absences

*Needs 1.*

Implement what Phase 1 turned red: `session`, `udf`, the missing `fs` members,
the rest of `env` and `lakehouse` — and the `mssparkutils` alias, which real
notebooks still import and which this tree does not have at all.

**What it buys.** A framework that introspects the surface stops declining to
run. A notebook that *calls* these still gets whatever the shim does, which is
Phase 3.

### Phase 3 — Behaviour contracts

*Needs 2. Largest phase.*

The step from "the method exists" to "the method is right", and the only one
that cannot be done by enumeration. Each member needs an assertion in the shape
contract 4 established: execute through the real path, then **verify out of
band** — the component that acted is never the one that confirms.

Scope by blast radius rather than alphabetically. `fs` and `credentials` touch
storage and identity, where a wrong answer is silent and expensive; `env`
returns strings a pipeline branches on.

**What it buys.** The first honest claim of the form "this behaves like
Fabric's" — bounded to the members actually covered, and only as true as the
citations from Phase 0.

### Phase 4 — The execution model

*Independent of 1–3; shares no code and no reviewer.*

`%%configure`, `%run`, the remaining language cells, notebook resources, and a
decision on the single-mount divergence — either close 2c′ or promote it from a
gap to a documented, permanent difference. Both are legitimate; leaving it
undecided is not.

**What it buys.** Notebooks that are authored normally stop needing to be
written specially. The axis most visible to someone trying the emulator for the
first time.

### Phase 5 — Differential against a real tenant

*Converts every phase above.*

Run the same probes against real Fabric and diff. The workflow already exists
and is secret-gated; what is missing is a parity row that *cites* it.

Until this runs, everything above is conformance to a published contract. This
is the only phase that converts it into parity, and it is also the phase that
will find the places the documentation is wrong — the failures worth the most.

**What it buys.** The word "parity", used accurately. Nothing else earns it.

## What must NOT be done

- **Do not report a coverage percentage before Phase 0.** A denominator
  assembled from memory produces a number that looks like progress and measures
  nothing.
- **Do not let a member pass because it exists.** Shape and behaviour are
  separate axes; conflating them is how contract 2's own heading came to need
  the words "independent of behaviour".
- **Do not record what you can grade.** Every limitation that turned out to be
  wrong this week was recorded and ungraded. A number nobody reads is
  indistinguishable from a number nobody checked.
- **Do not describe a gap more broadly than it was measured.** All three
  retractions this week were true observations generalised one step — from one
  engine to the product, from one symptom to a cause.
- **Do not claim parity from Axes A–C alone.** Without Phase 5 the accurate
  phrasing is "conforms to the published contract", and the difference is not
  pedantic: it is exactly where an emulator built from the same page as the docs
  will be wrong in the same direction.
