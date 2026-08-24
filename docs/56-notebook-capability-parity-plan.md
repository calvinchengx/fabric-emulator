# Notebook capability parity

> **Status: all six phases delivered, except that Phase 5 has no tenant.**
> Axes A–C are complete and every row below is measured rather than searched.
> Phase 5's differential is written and green on the emulator leg; until a
> parity row cites a real-tenant run, the honest phrasing is *"conforms to the
> published contract"*. Companion to
> [38-framework-conformance.md](38-framework-conformance.md) and
> [39-run-multiple-parity-plan.md](39-run-multiple-parity-plan.md), whose
> "what done buys, precisely" convention this follows.

The runtime conformance matrix is **18 of 18**. That proves seven structural
contracts on three backends, and it is **not** capability parity with real
Fabric. This document was the distance between the two; it is now the record of
closing it, and of what closing it turned up.

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

**Phases 0–2 are delivered, so this table has a denominator and it reads
zero.** The module list is not ours: it is the table on the [NotebookUtils
overview page](https://learn.microsoft.com/en-us/fabric/data-engineering/notebook-utilities),
read 2026-08-04. Every member in `notebookutils-reference.json` carries its
source page and that page's own last-updated date.

| Module | Documented | Present | Absent | Signature mismatches |
|---|---:|---:|---:|---:|
| `notebook` | 11 | 11 | 0 | 0 |
| `fs` | 15 | 15 | 0 | 0 |
| `lakehouse` | 8 | 8 | 0 | 0 |
| `credentials` | 4 | 4 | 0 | 0 |
| `session` | 2 | 2 | 0 | 0 |
| `udf` | 1 | 1 | 0 | 0 |
| `runtime` | 1 | 1 | 0 | 0 |
| `variableLibrary` | 2 | 2 | 0 | 0 |
| **Total** | **44** | **44** | **0** | **0** |

Graded by contract 2 on both lakehouse backends, every run.

### The eight that existed and would have failed anyway

The finding worth Phase 0, and the reason Phase 2 was not just "write the
missing methods". These were shipped, worked, and were used — and a framework
introspecting them **declined to run**, because contract 2's asymmetry is about
names, not counts:

| Member | Documented | Was shipped as |
|---|---|---|
| `fs.put` | `(file, content, overwrite)` | `(path, content, overwrite)` |
| `fs.head` | `(file, max_bytes)` | `(path, maxBytes)` |
| `fs.append` | `(file, content, createFileIfNotExists)` | `(path, content)` |
| `fs.cp` | `(src, dest, recurse)` | `(src, dst)` |
| `fs.rm` | `(path, recurse)` | `(path, recursive)` |
| `lakehouse.get` | `(name, workspaceId)` | `(lakehouseId, workspaceId)` |
| `lakehouse.create` | `…, definition, workspaceId` | `definition` absent |
| `lakehouse.list` | `(workspaceId, maxResults)` | `maxResults` absent |

`dst` for `dest`, `recursive` for `recurse`, `maxBytes` for `max_bytes`,
`lakehouseId` for `name` — each is the reasonable spelling somebody picks
writing a method from its description rather than from the page. Which is
precisely how this reference came to be needed.

### Correcting `head` found a live bug

`head` is a PREVIEW — the first `max_bytes`, 100 KB by default — and the shim
defaulted to the whole file. `python/spark_agent/json_multiline.py` called
`fs.head(path)` to parse JSON, **depending on that divergence**. Once `head`
matched the page, any document over 100 KB would have been truncated mid-parse,
or worse parsed as a shorter valid one. It now calls `read()`, and the
regression guard is a 20,000-record body a `head`-shaped reader cannot pass.

### Three refusals and emulations, stated rather than discovered

- **`lakehouse.loadTable` refuses, by name.** Fabric runs a server-side
  ingestion job — schema inference, format options, load modes. The plausible
  shortcut (read the CSV client-side, write Delta) is a *different operation
  wearing the same name*, succeeding silently for options it never applied. The
  member exists so introspection passes; calling it raises, and the error names
  the `spark.read…saveAsTable` one-liner that does the real work.
- **`fs.mount` is emulated, not faked.** Fabric's is blobfuse-backed and live;
  this is a point-in-time copy to a per-session local directory.
  `fileCacheTimeout` and `timeout` are accepted and ignored — correct emulation
  when there is nothing to switch — and that divergence is written here rather
  than left to be found at 2am.
- **`session.stop()` asks the agent.** A `sys.exit()` would end the process and
  take every other live notebook with it: contract 5's shared-agent leak in its
  most destructive form. The agent decides what "this session" means.

### What the old scope field got right, and what it hid

Before Phase 0 the reference declared its scope in one field,
`modules_not_yet_covered` — eight modules contract 2 did not grade. It was the
most useful line in the file, and it flattened a distinction that decides how
much work each entry is:

| Declared uncovered | Actual state | What the entry means |
|---|---|---|
| `fs`, `credentials`, `env`, `runtime`, `lakehouse`, `variableLibrary` | exists, ungraded | Shipped code nothing checks. Work is **go and check** — and expect some of it to be wrong, because nothing has ever failed on it. |
| `session`, `udf` | does not exist | Not written. Work is **go and build**. Same list, different order of magnitude. |
| `mssparkutils` | ~~on neither~~ **present** | Recorded as absent on both counts. Wrong: it is an alias in `__init__.py`, and the check looked for a filename. |

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
- **`mssparkutils` — and this is a correction to Phase 0, not a finding.**
  It was recorded as absent from the tree *and* from the list of known
  absences. It is present: `notebookutils/__init__.py` aliases the package to
  itself, so both `notebookutils.mssparkutils` and a top-level
  `import mssparkutils` resolve to the same module, and the agent binds it as a
  notebook global. The original check was `find -name 'mssparkutils*'`, which
  found no *file* — **a question about the filesystem, not about the
  namespace**. Importing it takes one line and answers the actual question.
  The overview states the rename is complete and the namespace **will be
  retired**, and aliasing rather than reimplementing is the right shape for
  that: one surface, two names, nothing extra to keep in step when the old name
  goes away.

The file now separates the two things that field conflated: every module is
**cited**, and `graded_by_contract_2` names the far smaller set the live probe
actually asserts. Reading a citation as a check is how a partial reference gets
mistaken for coverage.

## Axis C — the execution model

**Every row here has now been probed, and the original table was wrong three
times — twice in the worse direction.** It was built from `grep`, and said so:
*"'Absent' means no handler was found by search, not that it was tried and
failed."* That caveat earned its place.

| Capability | Was recorded | Measured | Now |
|---|---|---|---|
| `%%sql` / `%%pyspark` / `%%lang` | parsed | parsed | unchanged |
| parameters cell | parsed | parsed | unchanged |
| `%%configure` | absent | **recognised, then executed as Python** | accepted and ignored, out loud |
| `%%spark` (Scala) / `%%sparkr` | absent | **recognised, then executed as Python** | refused by name |
| `%%html` / `%%markdown` | absent | **recognised, then executed as Python** | rendered, not executed |
| `%run` | absent | absent | implemented |
| notebook resources (`builtin/`) | absent | absent | `nbResPath`, root-notebook semantics |
| `display()` / `displayHTML()` | *unverified* | **absent — `NameError`** | implemented |
| Files mount (one point) | "decision needed" | **already decided in docs/37** | no action; see below |

### Absent would have been better than what was there

The parser *recognises* `%%configure`, `%%spark`, `%%html` and `%%markdown` —
and the run loop then sent everything that was not `sql` to the **Python
executor**. So correct Scala came back as a Python `SyntaxError` pointing at the
user's own code, and a `%%configure` block of JSON failed the same way. That is
not a missing feature. It is a wrong answer, and it is the reason "no handler
found by search" is a claim about the search rather than about the system.

`internal/notebook/celllang.go` now gives a cell four dispositions, and the two
in the middle carry the judgement:

- **`%%configure` is accepted and IGNORED, never silently.** The cell records
  that it changed nothing and that the requested executors, memory and conf were
  not applied. Refusing would be *worse*: `%%configure` must be the first cell
  on Fabric, so a refusal makes every notebook carrying one unrunnable here —
  and the results it would produce are correct, just not on the requested
  hardware. One session, nothing to size, nothing to switch: contract 2's own
  definition of correct emulation.
- **Scala, R and C# are named**, with what to do instead, and explicitly *"Real
  Fabric runs it"* so the message cannot be misread as "your cell is invalid".

An unknown magic still falls back to Python. Fabric adds magics, and refusing
every one this build has not heard of would break notebooks on upgrade.

### `display()` was absent, not merely unverified

One of the most common lines in any Fabric notebook raised
`NameError: name 'display' is not defined`. Not a fidelity nuance — a notebook
written the ordinary way did not run. `display` and `displayHTML` are
**builtins** on Fabric, not imports, so they are bound in the session namespace.

`summary=True` is honoured rather than accepted-and-ignored, and the split from
`%%configure` is deliberate: Fabric documents summary as column name, type,
unique values and missing values — a data *quality* read a notebook branches on,
so there **is** something to switch. `%%configure` asks for hardware this
emulator does not have; that genuinely has nothing to switch.

What is *not* emulated is stated in the module: Fabric renders an interactive
table with charts and an inspect panel; this renders text. Same data, same
shape, no interactivity.

### The mount divergence was not an open decision

This document asked to "either close 2c′ or promote it to a documented,
permanent difference", and called leaving it undecided illegitimate.
[37-runtime-fidelity-gaps.md](37-runtime-fidelity-gaps.md) had already decided
it: **refuse, do not switch**, with the deeper fix deferred for a stated reason
— one agent container per session touches the whole compute model. **The item
was mine, not the system's**, and it is withdrawn rather than answered.

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

## The phases, and what each one found

Delivered. Numbered because they genuinely sequenced — each was unbuildable, or
merely decorative, without the one above it. Kept as a record rather than
rewritten into a summary, because in every phase the *finding* mattered more
than the work.

| | Phase | What it did | What it found |
|---|---|---|---|
| **0** | Cite the surface | `notebookutils-reference.json` from one module to eight, every member carrying its source page and read date | A denominator: **44 members, 25 wrong** |
| **1** | Grade every module | Contract 2 across all eight, with the module list substituted in *from the reference* | Went red on both backends, naming all 25 |
| **2** | Close the absences | 17 members written, 8 signatures corrected | Correcting `head` exposed a live truncation bug |
| **3** | Behaviour contracts | Executed on the real stack, confirmed out of band | `listTables` first passed **vacuously** |
| **4** | Execution model | Cell dispositions, `%run`, `display()`, `nbResPath` | The magics were *recognised then run as Python* |
| **5** | Differential | The surface's REST endpoints, either target, no branching | A harness that had never been re-runnable |

**Phase 0 was the cheapest and the one most likely to be skipped**, and it is
the reason the rest was possible: without a denominator there was no way to be
wrong.

**Phase 1 going red was the job, not a setback.** Two cells had been green
because they were not asking.

**Phase 5 is the only one still incomplete**, and only for a reason outside the
code: it needs a tenant. The suite runs against emulator or tenant with no
branching and is green on the emulator leg. Until a parity row *cites* a
real-tenant run, the accurate phrasing stays *"conforms to the published
contract"*, never *"matches Fabric"*.

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
