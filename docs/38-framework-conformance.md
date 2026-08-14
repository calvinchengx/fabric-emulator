# 38 — Framework conformance: what a Fabric product assumes, and how to test it

**Status: the findings are landed, the kit that would have found them is not.**
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

**The fix.** A signature-pinning test over the whole surface, the way
`test(schema)` pins REST payload fields against the reference. A parameter that
real Fabric accepts must appear here, whether or not it does anything.

### 3. The runtime is a versioned product, not "some Spark"

**What Fabric does.** A Fabric Runtime pins Spark, Delta, Python, and a
preinstalled library set together as one versioned unit. A framework declares
which runtime it targets and assumes that floor.

**Where the emulator stands.** Two images with different Python versions and no
statement about which Fabric runtime either claims to be. The JVM overlay is
built on a Spark image shipping Python 3.8, which is below the floor of current
frameworks; the first failure is a missing stdlib module, arriving long after the
agent reported ready, which reads as a notebook fault rather than a runtime that
was never eligible.

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

### 7. Credentials must outlive the run

A token minted at container start expired an hour into a run, and every OneLake
read then failed `401` until someone restarted by hand — which reads as a storage
outage. Fixed for one engine by keeping the launcher resident and re-minting.

**The generalisation.** Any credential the emulator hands to an engine needs a
refresh path, because real runs outlive token lifetimes. This is a property of
the compute surface, not of one launcher.

---

## The conformance kit

**Status: not built.** Items 1–7 are individually tractable. The reason they
existed for months is that nothing exercised them, and that is the gap worth
closing first — a new framework will find a new one next week otherwise.

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

**M.** No engine work and no research risk: it is notebooks, assertions, and one
CI job. The value is entirely in items 1, 4 and 5, which are the three that
produced false greens rather than loud failures.

**Start with contract 4 (write landing) on all three backends.** It is the one
that produced silent wrong answers rather than loud failures, and it is the only
row where the out-of-band verification pattern has to be got exactly right. Once
that harness exists, 5 and 6 are the same harness with different notebooks, and
1–3 are cheap assertions inside a session that is already running.

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
