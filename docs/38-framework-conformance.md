# 38 — Framework conformance: what a Fabric product assumes, and how to test it

**Status: all seven contracts are live. 14 of 18 cells.** Contract 4 is ✅ on all three
backends — a write through the emulator path, confirmed out of band (OneLake
DFS on sail/jvm; a fresh TDS connection on warehouse). The engine that wrote
is never the one that confirms. **Contract 1 is ✅ on sail and ❌ on jvm**,
and the jvm red is a real defect rather than a missing assertion: the JVM
overlay image has no `notebookutils` installed at all, so a notebook cannot
import the surface this repo grades 🟢 Real. **Both are now ✅ on both
backends**: the seven missing `notebookutils.notebook` methods are implemented,
and the JVM overlay has an interpreter that can import the shim at all.
**Contract 3 is now asserted too**, and green: both images declare the Fabric
Runtime they target and both meet its Python floor. **Contract 5 is green on
sail and jvm** — three notebooks submitted at once, each writing its own
artifact, each knowing only its own identity — and stays ❌ on warehouse, where
concurrent TDS sessions need a Go leg this kit does not have yet. **Contract 6 is green on sail** and records a gap on jvm for a reason worth its
own paragraph below. **Contract 7 is green on both.** What remains is the
warehouse column for 5, 6 and 7, which needs a Go leg driving concurrent TDS
sessions, and contract 6 on jvm. The offline half (`docs/conformance-matrix.md`,
`check_conformance.py --strict`) still gates `make check`.

**Two defects came out of contract 1's first run, and neither was visible to
anything else in the repo.** Both are the class this document names: green
parity rows, green witnesses, green test suite, and a notebook that cannot do
the thing.

1. **`notebookutils` is unconfigured in the spark-agent.** The shim defaults
   to `http://127.0.0.1:19080`, which inside that container is nothing, so
   `fs.put` from a `RunNotebook` cell raised `Connection refused`. No shipped
   compose (`docker-compose.override.yml`, `.compute.yml`, `.spark-jvm.yml`)
   and no `e2e/*` compose sets `NOTEBOOKUTILS_FABRIC_URL` on the agent;
   `jupyter` in `docker-compose.yml` sets all three. So the shim works in a
   Jupyter cell and fails in a notebook job — while the jupyter service's own
   comment claims "a cell here and a cell in a RunNotebook job execute
   identically". The conformance composes set them so the contract can be
   measured at all; **the shipped composes are unchanged and still carry the
   gap.**
2. **The JVM overlay has no `notebookutils` at all** — `ModuleNotFoundError`,
   not a configuration problem. Contract 3 already flags that image's Python
   as below the framework floor; it also lacks the shim.

Neither is fixed here. A change that lands the harness and rewrites two images
is one nobody can review, and the matrix exists precisely so a gap can be
recorded red with a pointer instead of blocking the kit.
This document generalises a class of defect the emulator kept shipping: contracts
that real Fabric *frameworks* depend on, which no amount of reading Microsoft's
REST reference reveals, because they are not in the REST surface at all. They
live in the notebook runtime — what is importable, what is in scope, what a
signature looks like, what `runtime.context` answers, and where a write actually
lands.

Every defect in this class was found the same way: by driving a real data
product end to end and watching it fail. None was found by the parity map, the
witness system, or the test suite, all of which were green throughout. That is
the fact this document exists to fix.

## Why the REST reference does not cover this

The emulator's fidelity work has mostly been control-plane: an API surface with
a published schema, which can be read, implemented, and witnessed. Notebook
frameworks depend on something else — the **runtime contract**, which Microsoft
documents thinly and which frameworks probe rather than read:

- they resolve identity through a **fallback chain**, so satisfying one path is
  not enough;
- they **introspect signatures** to decide whether a runtime supports a feature,
  before calling anything;
- they assume the session is **theirs alone**, because on Fabric it is;
- they assume a successful write **landed where Fabric puts it**.

An emulator can satisfy every documented endpoint and fail all four.

## The seven contracts

### 1. Session context is a control-plane contract, not an environment variable

**What a framework does.** It resolves the workspace and lakehouse it is running
in by probing in order: `mssparkutils.env.getWorkspaceId()`, then
`mssparkutils.runtime.context`, then an environment fallback, then it raises.
Every published Fabric framework has some version of this chain. Satisfying only
the last link means a framework that never reaches it fails on its first call.

**Where the emulator stands.** `/mount` already sent workspace and lakehouse
ids at bind (the Files mount needs them). `runtime.context` ignored that and
was built **at module import** from `NOTEBOOKUTILS_WORKSPACE_ID` /
`NOTEBOOKUTILS_LAKEHOUSE_ID`, so it was process-global: not per-run, and in a
shared agent not even per-session. A statement request carried `session`,
`code` and `kind` — identity for lineage (`jobId`/`cellIndex`) but not the
notebook's workspace.

**The fix (landed for RunNotebook).** Every notebook `/statements` body now
carries `workspaceId` / `lakehouseId` / `notebookId` / `isForPipeline`. The
agent remembers them per session (including from `/mount`) and binds
`notebookutils.runtime.context` around the statement via a `ContextVar`, so
two concurrent notebooks cannot see each other's workspace.
`mssparkutils.env.getWorkspaceId()` reads that same context. The environment
remains the fallback for a kernel that has no agent. Fabric's context also
carries capacity; that field is still absent.

**This was the highest-value single item in this document**, because context
resolution is the first thing a framework does. Setting environment variables
out of band is no longer the only path that works.

### 2. The API shape is the contract, independent of behaviour

**What a framework does.** It inspects a function's signature and declines to run
if a parameter is missing — without ever calling it. A `notebookutils.notebook.
run` lacking `spark_environment` / `attach_lakehouse` is read as "this runtime
does not support notebook activities", and the framework stops.

**The generalisation.** For the `notebookutils` / `mssparkutils` surface,
accepting a parameter and ignoring it is *correct emulation* when there is
nothing to switch — the emulator has one session and attaches the notebook's own
binding. What is not correct is omitting the parameter, because omission is a
signal frameworks read.

**The fix, landed for `notebookutils.notebook`.**
`e2e/conformance/notebookutils-reference.json` pins the documented signatures
the way `test(schema)` pins REST payload fields, and **every entry cites the
Microsoft page it was read from plus that page's own last-updated date** — a
reference assembled from memory would be this same defect one tier up, a claim
about Fabric with nothing behind it. Its scope is declared rather than implied:
`fs`, `credentials`, `env`, `runtime`, `lakehouse`, `session`, `udf` and
`variableLibrary` are listed as not yet covered, so a partial reference cannot
be read as a complete one.

**What the probe found, and what was done about it.** The four orchestration
methods were correct — `run`, `runMultiple`, `validateDAG`, `exit` all carry
their documented parameters in the documented order. **Seven documented methods
were absent entirely**: `create`, `get`, `getDefinition`, `update`,
`updateDefinition`, `delete`, `list` — the whole notebook-management surface a
CI/CD framework introspects before it will run.

They are now implemented, and implementing them surfaced a third defect one
layer down. Microsoft's `create(content=…)` takes **`.ipynb`**; this emulator
executes from `notebook-content.py`. Nothing derived one from the other outside
the VS Code route, so a notebook created the documented way stored happily,
returned 201/202, and its `RunNotebook` job then failed with
`notebook-content.py is missing` — **a create that reports success and produces
something unrunnable**, which is §4's shape arriving one API call later.

The derivation is **server-side** (`notebookExecutableParts`, called from
`createItem` and `updateDefinition`), reusing the existing converter, so there
is one definition of it in the package that owns the parser and a Python client,
the VS Code route and a raw REST caller cannot drift apart. Doing it in the shim
would have been a second definition. Two refusals in it: an author who sends
both parts keeps theirs, and an undecodable payload is stored as sent rather
than rejected there.

**Missing fails, extra passes, order counts.** A framework declines on an
absent parameter without calling anything, so omission is the signal. Accepting
one and ignoring it is correct emulation when there is nothing to switch — this
shim already carries `spark_environment` and `attach_lakehouse`, which
Microsoft's current page does not document, and that is fine. Order is part of
the contract because Fabric's own examples are positional
(`run("Sample1", 90, {"input": 20})`): right names in the wrong order accept
that call and do something else with it.

### 3. The runtime is a versioned product, not "some Spark"

**What Fabric does.** A Fabric Runtime pins Spark, Delta, Python, and a
preinstalled library set together as one versioned unit. A framework declares
which runtime it targets and assumes that floor.

**Where the emulator stood, and what changed.** Two images with different
Python versions and no statement about which Fabric runtime either claims to be.
The JVM overlay was built on a Spark image shipping **Python 3.8.10**, and
[Fabric Runtime 1.3 is **Python 3.11**](https://learn.microsoft.com/en-us/fabric/data-engineering/runtime-1-3)
— everything else in that image already matched the runtime it claims to be
(Spark 3.5, Delta 3.2, Java 11, Scala 2.12) and the interpreter did not.

Not cosmetic: `notebookutils` requires `>= 3.9`, so a notebook on that overlay
could not import the surface **at all**, which is what held contracts 1 and 2 red
there. The overlay now carries Python 3.11 in a virtualenv with the shim
installed, and `PYSPARK_PYTHON` points at it. PySpark itself needed no change —
Spark ships it as `pyspark.zip` on `PYTHONPATH`, so it is interpreter-agnostic.

**The assertion now exists.** Both images carry `ENV FABRIC_RUNTIME=1.3`, and
`e2e/conformance/fabric-runtimes.json` holds that runtime's floor with the
Microsoft page and its last-updated date. Two failures are distinguished
because they are different problems: an image that declares NOTHING cannot be
asked the question at all, and an image that declares a runtime and ships below
its floor answers it wrongly — which is worse, and is what happened here.

**Only Python is asserted.** It is the floor that actually broke. Whether the
engine behaves like Spark 3.5 is the engine matrix's question, and it answers
that row by row rather than by trusting a version string; Spark's reported
version is recorded in the findings for drift, not asserted.

The comparison is numeric, not textual, and that is not fussiness: `3.8` sorts
above `3.11` as a string, so a string comparison would have passed the exact
image that failed.

**The fix.** Declare the Fabric Runtime version each image targets, make the
Python floor match it, and have the engine matrix assert it. A runtime that
cannot meet the floor should say so at startup, not at the first import.

### 4. A success claim must be witnessed by the artifact

**The recurring defect.** The single most repeated finding, in four unrelated
places:

| Reported | Actual |
|---|---|
| CTAS built the gold tables | Engine wrote to its own warehouse; the lakehouse stayed empty |
| `saveAsTable("schema.table")` succeeded | Schema folder unmodelled; the write vanished into the engine warehouse |
| RunNotebook job `Completed` | Every cell still Pending |
| A concurrent fan-out reported success | Exit values landed in another session's globals |

Each is a *false green*, and each was invisible to the caller. This is the
failure class the emulator exists to prevent, because a consumer meets it for
the first time in production.

**The generalisation.** A success claim must be checkable against the artifact
existing where Fabric would have put it. The emulator already has the OneLake
listing needed to check. This is the principle `bindDefaultLakehouse` applies to
unqualified table names, applied everywhere a write can be redirected.

### 5. Concurrency is the default case, not the edge case

**What a framework does.** A pipeline-driven orchestrator fans out to tens of
child notebooks at once. On Fabric each gets its own container.

**Where the emulator stands.** One long-lived agent with many session namespaces,
so everything process-global leaks across runs: the prelude's exit-value global
(fixed), `/opt/wheels` installs (Environments now refuse a second bind), and
`runtime.context` from item 1. The Files mount is two-way at statement
boundaries and refuses a second lakehouse rather than switching; the single
`/lakehouse/default` path remains, which is 2c′ in
[37-runtime-fidelity-gaps.md](37-runtime-fidelity-gaps.md).

**The rule.** Every piece of agent state is session-scoped unless it is proven
shared. The shared-agent model is a legitimate emulator choice; letting it leak
is not.

**Now proven, on both engines.** Three notebooks are published, **all submitted
before any is polled** — serial submission would prove nothing, because the leak
only exists while two sessions are live in the same agent at once — and each
writes its own file under `Files/conformance/fanout/`. The probe compares each
child's reported notebook id against the id **the control plane issued that
child**, not against what another child said: two children that had leaked into
each other would agree with each other and disagree with the harness.

Markers and ids both, because they answer different questions. A marker proves a
child ran; the id proves it knew WHICH child it was. A child reporting another
child's identity is the leak, and it is invisible to any assertion that only
counts successes.

### 6. Engine gaps need a bounded-rewrite escape hatch, with a stated contract

**The pattern, now used three times.** A strict grammar recognises a bounded
statement form, routes it to a correct implementation (delta-rs), and lets
anything it does not understand fall through to the engine untouched. It covers
`OPTIMIZE` / `VACUUM`, `MERGE`, and CTAS (`python/spark_agent/delta_ops.py`).

The reason it keeps recurring is that engine gaps are not evenly distributed:
they cluster on the statements a medallion pipeline writes constantly. An upsert
into a table carrying audit timestamps is not an exotic query; it is the normal
shape, and an engine that cannot plan it makes practically every silver notebook
unrunnable.

**The generalisation.** Name this as a mechanism with a published contract —
strict grammar, honest fall-through, no silent approximation — rather than three
ad-hoc interceptors. It is the Spark-side sibling of what `internal/tsql` does
for T-SQL on the wire, and it deserves the same treatment: a documented grammar,
and a test that an unrecognised shape reaches the engine unmodified.

**Asserted on sail, and NOT RUN on jvm — which is a finding, not a shortcut.**
The statements address the table by `abfss://`, and hadoop's ABFS driver forces
TLS for that scheme: `fs.azure.always.use.https=false` downgrades `abfs://` and
nothing else. Against a plaintext stack every request therefore fails at the
socket with status `0`, hadoop-azure treats that as retryable, and the notebook
**hangs** — measured as five threads parked in
`AbfsRestOperation.completeExecute` for 324s to 864s, one per statement,
accumulating and never exiting.

A hang is worse than a red. Contract 6's statements share a notebook with 1, 2,
3 and 4, so one hang takes five cells down and reports the harness's own timeout
as five separate defects — which is exactly what happened twice before this was
gated. The jvm cell now records the measurement instead.

**A TLS terminator in front of the OneLake alias fixes the statements** — 4.6s /
2.8s / 1.5s / 2.5s for write, read, OPTIMIZE and MERGE, where they had hung
indefinitely — and is deliberately not in the tree: it also regressed cell 0's
write into the local read-only Spark warehouse
(`Mkdirs failed to create file:/opt/spark/work-dir/spark-warehouse/events`) for
a reason not yet understood. Trading one red for another is not a fix. **So the
JVM overlay cannot reach OneLake by path today**, and that limitation had never
surfaced because the engine matrix probes local paths — its own text says
credentials are out of scope there — and no notebook had addressed OneLake by
`abfss://` on that engine.

**`OPTIMIZE <name>` cannot resolve a table a notebook wrote.** The delta-rs
interception resolves a NAME through the emulator's registration, and
`saveAsTable` inside a notebook does not register it that way: `OPTIMIZE events`
fails with `cannot resolve 'events' to a table location: it was not registered
through the emulator`. Measured twice. The probe uses the path form, which is
what `e2e/livy` proves against a real OneLake table — but the name is what a
notebook author would type, so this is a gap rather than a preference.

### 7. Credentials must outlive the run

A token minted at container start expired an hour into a run, and every OneLake
read then failed `401` until someone restarted by hand — which reads as a storage
outage. Fixed for one engine by keeping the launcher resident and re-minting.

**The generalisation.** Any credential the emulator hands to an engine needs a
refresh path, because real runs outlive token lifetimes. This is a property of
the compute surface, not of one launcher.

**Asserted by making the clock cheap rather than the test slow.**
`TOKEN_LIFETIME_ACCESS_SECONDS=60` on the conformance entra-emulator, and the
notebook writes to OneLake through the engine 75 seconds after the session
started — past a lifetime the original defect took an hour to cross.

**The wait must actually exceed the lifetime, and the probe checks that.** A
probe that slept less than the token lived would pass on a runtime that never
re-mints, which is the exact defect. The session reports both numbers and the
harness refuses to grade a run where the gap was never opened. The read *before*
the wait is the control: without a working baseline the second operation says
nothing either way.

**One setting covers every audience**, so shortening it also shortens the
harness's own token — which is why `live.py` now caches and re-mints at two
thirds of advertised life. A client that minted once and then polled a job for
minutes would 401 partway through and report a broken pipeline instead of a
short token. That is the same argument this contract makes about the engine, one
tier up.

**An open question this contract surfaced and did not answer.** The second
operation was originally a re-read of the table cell 0 wrote. It failed with
`AnalysisException: Table not found: [TABLE_OR_VIEW_NOT_FOUND]` while the read
*before* the wait succeeded — the catalog entry a `saveAsTable` created was gone
75 seconds later, in the same session and the same cell, on **sail**. Whether a
session is being recycled underneath the cell or a catalog entry is expiring is
not established. It is not what §7 is about — a credential dying, not a catalog
forgetting a name — so the probe writes to a fresh table instead, which needs
the engine's credential and nothing that must survive the wait. **The
observation is recorded here rather than routed around silently**, because a
notebook that cannot see its own table after a minute would matter to anyone
running something long.

---

## The conformance kit

**Status: built; contracts 4 and 1 are live.** The harness, the committed
matrix, and the offline checker are in tree. Sail, JVM, and warehouse each
write through the emulator path and an out-of-band reader confirms the
artifact. Items 3 and 5–7 are still individually tractable. The reason
they existed for months is that nothing exercised them, and that is the
gap worth closing first — a new framework will find a new one next week
otherwise. Contract 1 found two on its first run, and contract 2 a third.

**Contracts share a run but must not share a failure.** Contract 1 rides the
same notebook as contract 4, which costs nothing and describes one session
rather than two. Twice while wiring it, a contract-1 failure failed the *job*
and turned contract 4 red — with the table landed and the out-of-band reader
seeing it. Its cell is guarded whole, imports included, and the absent
artifact is the failure signal. The rule generalises to every contract that
joins this run.

**A red must point at its cause.** A missing artifact says only `404`. The
notebook prints why it could not write one, and the harness quotes that line
into the cell, so the jvm red reads `ModuleNotFoundError: No module named
'notebookutils'` rather than leaving a reader to guess.

### What it is

A suite that asserts the **runtime contracts a framework depends on**, expressed
as a Fabric product would exercise them, not as unit tests of the emulator's
internals. It is deliberately *not* another medallion example: the examples prove
a pipeline runs, which is a different claim from proving the runtime answers
correctly when probed.

### What it asserts

| # | Contract | Assertion |
|---|---|---|
| 1 | Context chain | Each link resolves independently: `env.getWorkspaceId()`, `runtime.context`, and the env fallback each return the *running* notebook's identity, with no variable set out of band |
| 2 | Signature shape | Every parameter real Fabric accepts is present on the `notebookutils`/`mssparkutils` surface, pinned against the reference |
| 3 | Runtime floor | The image declares a Fabric Runtime version, and its Python meets that runtime's floor |
| 4 | Write landing | Every write path (`saveAsTable` qualified and unqualified, CTAS, MERGE, mount write-back) is followed by a OneLake listing proving the artifact is where Fabric puts it |
| 5 | Concurrent isolation | A fan-out of N children each reports its own exit value, binds its own lakehouse, and sees its own context |
| 6 | Fall-through | A statement the rewrite grammar does not recognise reaches the engine unmodified, and fails honestly if the engine cannot plan it |
| 7 | Credential lifetime | A run that outlives the token lifetime keeps reading |

### Every contract proves real execution, on a real backend

An assertion that passes against a stub proves nothing, so each contract is
proven by *executing* on the backend it concerns:

- **Warehouse** — a real SQL Server, reached over TDS.
- **Lakehouse** — Parquet/Delta in OneLake, written by **Sail** and by **JVM
  PySpark**, because the two engines fail differently and one is the other's
  control.

**The rule that makes it real: the engine that wrote must not be the one that
confirms.** Every false green in [the table above](#4-a-success-claim-must-be-witnessed-by-the-artifact)
happened because success was reported by the component doing the work — the
engine's own catalog said the table existed, in the engine's own warehouse. So
each assertion has two halves: execute through the emulator's real path
(RunNotebook for Lakehouse, TDS for Warehouse), then **verify out of band** —
read the Delta back through delta-rs / the OneLake DFS API, and read the
Warehouse table through a fresh TDS connection. Both readers already exist as CI
jobs (`e2e/delta-rs`, `e2e/warehouse-tds`); this composes them rather than
inventing anything.

<!-- APPLICABILITY:BEGIN (scripts/check_conformance.py parses this table) -->

| # | Contract | sail | jvm | warehouse |
|---|---|---|---|---|
| 1 | Context chain | required | required | n/a |
| 2 | Signature shape | required | required | n/a |
| 3 | Runtime floor | required | required | n/a |
| 4 | Write landing | required | required | required |
| 5 | Concurrent isolation | required | required | required |
| 6 | Rewrite fall-through | required | control | required |
| 7 | Credential lifetime | required | required | required |

<!-- APPLICABILITY:END -->

`n/a` is honest, not a hole: contracts 1–3 are properties of a notebook session,
and the Warehouse surface has none. `control` is the interesting verdict —
**the JVM column is what makes contract 6 provable at all.** The delta-rs
rewrites exist because Sail cannot plan `MERGE` against a temporal-columned
target; on JVM the engine *can*, so the grammar must be proven to stay out of the
way. The same holds for `input_file_name()`: shimmed on Sail, native on JVM. A
single-engine suite cannot tell "the rewrite worked" from "the rewrite was not
needed and fired anyway".

### CI realisation

One job, one matrix, three backends:

```yaml
conformance:
  name: Framework conformance (${{ matrix.backend }})
  strategy:
    fail-fast: false
    matrix:
      backend: [sail, jvm, warehouse]
  runs-on: ubuntu-latest
  timeout-minutes: 45
```

Three things keep this from being a new tax:

- **The JVM image is already built on every push.** `engine-matrix` runs all
  three engine profiles per push. Reusing the same `actions/cache` key on
  `docker/spark-runtime/jars` means the Maven fetch costs once per `jars.txt`
  change, not once per run. (Worth correcting a nuance: JVM *probes* do run per
  push. What has never run per push is the emulator composed **over** the JVM
  overlay — which is exactly what a conformance leg fixes.)
- **SQL Server is about ten seconds** on the ubuntu leg, and `warehouse-tds`
  already uses the `services:` form with a healthcheck.
- The workflow's `concurrency` group already cancels superseded runs.

### A committed matrix, not a pass/fail suite

Model the output on `engine-matrix`, not on an ordinary test job: regenerate
`docs/conformance-matrix.md` from the run and fail if it differs from what is
committed.

```yaml
- run: uv run --frozen --no-sync python e2e/conformance/run.py --backend ${{ matrix.backend }}
- run: git diff --exit-code docs/conformance-matrix.md
```

That buys three things a green/red suite cannot:

1. **The kit can land before every contract passes.** Contract 1 fails today.
   A pass/fail suite could not merge until it is fixed; a matrix lands with the
   cell ❌ and a pointer, which is this document's "record it as a known gap"
   rule made executable.
2. **No cell may regress silently** — the same mechanism that stops a stale
   engine-gap claim surviving an upgrade that closed it.
3. **A cell that starts passing forces the doc to change**, so the map cannot
   drift stale in the optimistic direction either.

### Gating, and what `make check` enforces

Two conventions the repo already uses:

- **Arm on capability, not on platform.** The coverage floor keys on
  `WAREHOUSE_MSSQL_DSN` being present precisely so it self-arms when a leg gains
  an engine. A backend that is not reachable records `gated` and emits a loud
  `::warning::` — never a silent pass. That is also how macOS is handled, where
  a containerised SQL Server is documented as unsolved.
- **Register witnesses.** `ci:conformance-sail`, `ci:conformance-jvm` and
  `ci:conformance-warehouse` belong in [witnesses.json](witnesses.json), so
  `check_witnesses.py --strict` catches a contract being deleted out from under a
  parity claim — the exact failure that let the Environments row claim
  provisioning it never performed.

The regeneration needs live backends, so it stays in CI. `make check` enforces
the half that is checkable offline, via `scripts/check_conformance.py`: the
applicability table above lists every contract this document defines and no
others, and — once the matrix exists — every `required`/`control` cell carries a
verdict, every ❌ carries a pointer, and every witness it names is real. That is
the same division the other invariant scripts use: the expensive proof runs in
CI, the correspondence between doc and artifact is enforced everywhere.

### Write the assertions once

Parameterise by backend rather than shipping three suites that drift, exactly as
`e2e/engine-matrix` runs one `probes.py` under different profiles. A divergence
then surfaces as a differing cell instead of as two suites that quietly stopped
testing the same thing.

### Shape

A notebook-driven suite, run through the emulator's own job API rather than
against the agent directly, because the contracts are about what a *notebook*
sees. It should read as the framework code it stands in for: probe, introspect,
fall back, assert. Each assertion names the contract it covers, so a failure says
which promise broke rather than which line threw.

Where a contract is not yet met, the suite records it as a **known gap with a
pointer** rather than being deleted or skipped silently — the same discipline
[the witness map](witnesses.json) applies to parity claims. A suite that only
tests what already works cannot tell you what a new consumer will hit.

### Size

**M.** No engine work and no research risk: it is notebooks, assertions, and
three CI jobs. The value is entirely in items 1, 4 and 5, which are the three
that produced false greens rather than loud failures.

**Contract 4 (write landing) is wired on all three backends.** It is the one
that produced silent wrong answers rather than loud failures, and it is the only
row where the out-of-band verification pattern has to be got exactly right. 5
and 6 are the same harness with different notebooks, and 1–3 are cheap
assertions inside a session that is already running.

### Why not extend the medallion examples instead

The examples answer "does a pipeline run end to end", and they answer it well
enough that four of them run in CI. They cannot answer "does the runtime respond
correctly to a framework that probes it", because a pipeline that works never
probes: it calls the paths that happen to be implemented. Every defect in this
document sat underneath four passing medallion examples.

## What must NOT be done

- **Do not name a consumer.** This repo is agnostic by design: it emulates
  Fabric, not anyone's product. A finding is stated as the Fabric contract it
  violates, and a fixture is the generic shape of the thing, never a customer's.
  The kit is worth building precisely because it turns "we fixed what one product
  hit" into "we know what any Fabric product needs".
- **Do not satisfy a contract only at the last link of a fallback chain.** A
  framework that stops at link one never reaches it, and the emulator looks
  broken in a way no test reproduces.
- **Do not delete or skip an assertion that fails.** Record it as a known gap
  with a pointer, or the suite becomes a list of things that already work.
- **Do not add a signature parameter that real Fabric rejects.** Being more
  permissive than the thing being emulated is the one direction that actively
  misleads: it passes here and fails there.
