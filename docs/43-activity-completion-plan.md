# 43 — Every pipeline activity, and what "every" honestly means

**Goal, as directed: all 34 activities Microsoft documents for Fabric data
pipelines, implemented with full test coverage and a witness per claim.** This
plan exists because "implement all" has two failure modes this repo has already
paid for once each: inventing wire shapes nobody captured (the fabricated
nested columns), and counting a stub as done (the WebHook that silently aliased
Web). Every phase below names its oracle before its code.

Current state. The line here used to read "18 of 34 execute for real" and went
stale across three merged phases. It is **not simply re-counted**, because
Phase 7 below showed the denominator was the wrong frame: 34 was Microsoft's
documented activity *gallery*, while the wire accepts **41 discriminators** —
nine of which this plan never listed, and every one of which the dispatch was
reporting as `Succeeded`. A fraction measured against a product surface cannot
express "and the ones the surface does not show", which is precisely where the
defects were.

What is verifiable, stated so anyone can re-derive it rather than trust it:

- **17 leaf activity types execute for real** in `internal/api/pipelines.go`'s
  dispatch: notebook, invoke-pipeline, Copy, Lookup, GetMetadata, Delete,
  Script, stored procedure, Web, WebHook, Functions, HDInsight Spark,
  Databricks notebook, Databricks python, Azure Batch (opt-in), Validation,
  Data Explorer command. (Several accept more than one wire spelling —
  `RunNotebook`/`TridentNotebook`/`SynapseNotebook` are one behaviour, as are
  `SqlServerStoredProcedure`/`SqlPoolStoredProcedure` and `Web`/`WebActivity` —
  so the *case-label* count is higher than the behaviour count.)
- **9 control-flow types** are interpreted in `internal/pipeline/activities.go`
  (SetVariable, AppendVariable, ForEach, IfCondition, Switch, Until, Filter,
  Wait, Fail), plus the `Inactive` activity *state*.
- **13 types refuse by name with a cause**: Dataflow Gen2 (3 spellings), the
  three Azure ML types, the Databricks JAR task, and the six in Phase 7.
- **7 remain blocked on a wire-name capture** from a real tenant (Phase 0):
  Refresh SQL Endpoint, Lakehouse maintenance, KQL, Spark Job Definition,
  Teams, Copy job, Approval.

**To re-derive:** list the `case` labels in the dispatch switch and in
`activities.go`, and diff them against the `x-ms-discriminator-value` set in
ADF's published `Pipeline.json`. Anything in neither list falls to the
dispatch default — see Phase 7 for why that matters.

## The two rules every phase inherits

1. **Capture before code.** No activity lands under a guessed `type` string or
   an assumed `typeProperties` shape. The oracle is a pipeline JSON exported
   from a real tenant (the portal's JSON view, or the secret-gated
   `real-fabric` workflow fetching a definition via REST). A shape the docs
   describe but no capture confirms is recorded as such in the code comment.
2. **A stub must say so.** Where the emulator cannot reach the real service,
   the activity either terminates the real *protocol* against a local stand-in
   (the `kustainer`/Airflow/SeaweedFS precedent) or refuses by name. "Succeeded"
   without an observable effect is the one output no phase may produce — the
   Web activity's stub mode (`TestWebActivityStubModeSaysItDidNotCall`) is the
   pattern.

## Phase 0 — unblock (external input, no code)

| Item | Needs | Unblocks |
|---|---|---|
| Wire-name capture | One throwaway pipeline in a real tenant containing Refresh SQL Endpoint, Lakehouse maintenance, KQL, Spark Job Definition, Teams, Copy job, Approval and Refresh Materialized Lake View activities. **Now a button rather than a chore:** run the `Real Fabric conformance` workflow with `capture_item_type: dataPipelines` and the pipeline's display name, and it prints the SHAPE — every `type` discriminator and every property name, with all values redacted, because this repository's logs are public. See `scripts/capture_definition_shape.py` | Phase 2, and the five other blocked activities |
| Copy job activity case | #78 to settle; owned by the CopyJob session, or claimed by notice after | The 24th real activity — wiring the existing `copyjob` executor into the `pipelines.go` dispatch |

## Phase 1 — async pipelines (the prerequisite, doc 37 §4)

Pipelines execute inline in the job POST. Two activities are *defined* by
parking (WebHook's callback, Approval's human decision), so this lands first.
Scope is doc 37's: the job goes async through the repo's own LRO pattern,
~50 test sites move from synchronous assertion to poll-then-assert. **Size M.**
Witness: existing pipeline suites re-run green under async, plus a test that a
long pipeline's job POST returns 202 before the pipeline finishes — the
behaviour inline execution cannot produce.

## Phase 2 — the captured four (each S once Phase 0 lands)

| Activity | Engine already present | Behaviour to honour | Witness |
|---|---|---|---|
| Refresh SQL Endpoint | warehouse reflection | the documented `Success` / **`NotRun`** (nothing unsynced) / `Failure` output statuses; lock-contention failure stays representable | Go: stale reflection → activity → tables current; `NotRun` on a second run with no new data |
| Lakehouse maintenance | delta_ops OPTIMIZE/VACUUM via the Livy agent | table-scoped maintenance; v-order accepted where documented, refused-by-name where Sail cannot | Go + assertion in `e2e/livy` (compaction observed, not just exit 0) |
| KQL | kustainer under `--profile rti` | script against a KQL DB; honest 501 without the profile | e2e under the rti job; grade ceiling 🟠 (AVX2/amd64) |
| SJD wrapper | SJD item execution | activity → item job, both `jobs.go` switches if a job type appears, bus-subscribe-then-drain test per the CopyJob lesson | Go: SJD activity runs the item's job to terminal |

## Phase 3 — HTTP-real externals (no park needed)

- **Functions** — an Azure Function call is an HTTP request with a key. Execute
  it for real like Web does; witness against a local stand-in function that
  rejects a missing/wrong key (negative control first). **S.**
- **Teams** — an incoming-webhook message is an HTTP POST of a documented card
  payload. Execute for real against the connection's URL; witness with a
  stand-in receiver asserting the card shape, plus refusal-by-name for the
  Graph-API delivery modes the emulator does not model. **S–M.**

## Phase 4 — the park pair (after Phase 1)

- **WebHook, for real this time** — call with a generated `callBackUri` on the
  emulator's own surface, park on the virtual clock (documented default 10m),
  resume on the callback, fail on timeout; the refusal from #79 comes out the
  same PR its replacement goes in. Witness: an e2e/Go test whose stand-in
  receiver calls back *late* but in time, plus a timeout case.
- **Approval** — park until a decision arrives on an approval surface
  (`/_emulator` control plane, since Fabric's approval UI has no public REST);
  approve and reject both witnessed, plus the timeout. Boundary stated like
  the Reflex binding: the *decision surface* is emulator-native, everything
  downstream is faithful. **M each.**

## Phase 5 — compute externals (protocol termination, the Livy precedent)

The emulator already terminates Livy itself and lets a real engine compute.
Same shape here — terminate each service's job protocol locally, compute with
what exists, refuse what cannot be honoured by name:

| Activity | Protocol terminated | Compute | Size |
|---|---|---|---|
| HDInsight | the activity's Spark-job submission | Sail (JVM overlay for JAR types) | M |
| Azure Databricks | Jobs API stand-in (runs/submit → poll) | notebook/python via the agent; JAR → JVM overlay or refusal-by-name | L |
| Azure Batch | task submission | the script in a sandboxed container sidecar | L |
| Azure ML | ~~job submission~~ **nothing — refused by name** | ~~python entry via the agent~~ **there is no entry point to run** | S |

**The Azure ML row did not survive its oracle, and the correction is recorded
rather than quietly edited.** This table sketched "python entry via the agent"
by analogy with the three rows above it. The ADF schema does not support the
analogy: `AzureMLExecutePipeline` names only `mlPipelineId`, an opaque handle on
a pipeline **published in an Azure ML workspace**, and neither of the ML Studio
(classic) activities names a code artifact either. The neighbours run because
each one points at *a thing to execute* that the emulator can read; this one
points at steps that live in a service the emulator does not host. So all three
refuse by name, and the value delivered is that they no longer fall through the
dispatch default and report `Succeeded` — which is what they did before. See the
parity map's Azure ML row and `internal/api/azuremlactivity.go`.

Grades here say what they are: 🟢 "Real (protocol terminated, local compute)"
— the Livy row's wording, which a reader already knows how to trust. Each needs
its stand-in to reject bad auth (the SeaweedFS negative-control rule: a pass
must not be satisfiable by a server that lets anyone in).

## Phase 6 — Refresh Materialized Lake View (build, L)

**The model is built.** A view is a named query against a lakehouse; refresh
runs it on the engine and writes a real Delta table under `Tables/<name>`, and
staleness is measured against the declared sources' Delta versions. See the
parity map's *Materialized lake views* row and `internal/api/mlv.go`.

The **definition surface is emulator-native**, on the Reflex-binding precedent:
Fabric creates these with Spark SQL DDL that no capture here has observed, and
inventing a syntax is what the oracle rule forbids. When a capture arrives the
DDL becomes a second front door onto the same model rather than a rewrite.

**The activity itself is still blocked**, and on the same thing as the other
seven: nobody has captured its wire `type` or `typeProperties`. It is now a
Phase-2-shaped wrapper over `RefreshMaterializedLakeView` — small, once the
capture lands.

## Decision gates — named, not buried

1. **Dataflow Gen2 execution.** The engine is Power Query M; no open
   implementation exists to attach. Choices: (a) keep the honest
   `DataflowEngineNotImplemented` refusal and state that "all" excludes it;
   (b) an M-subset interpreter — **XL, research-grade risk**, and a partial M
   that silently mis-evaluates would be worse than the refusal. This plan
   recommends (a) until a consumer produces a concrete dataflow the subset
   would have to run.
2. **Deactivate** is an activity *state*, not an activity — already honoured
   (placeholder status, mark-driven branching). Counted done, not planned.

## Coverage and witness accounting

Every phase lands: Go tests proven able to fail (mutation-checked where the
guard is a branch), e2e where the claim crosses a process boundary, a parity
row whose grade matches its witness, and a `witnesses.json` entry — the strict
checker's claim count is the plan's progress meter. Coverage floors (Go 90,
Python `fail_under` in pyproject) must not drop; new e2e suites wire into
`ci.yml` and `docs/12-e2e-matrix.md` in the same PR, so the matrix never lags
a suite. One PR per activity; merge-when-green watchers pin the head SHA *and*
require registered checks. Start-of-work notice per phase through the session
channel — Phase 5 especially, since sidecars touch compose files other
sessions run.

## Order and the finish line

Phase 0 (external) → 1 → 2 (parallel with 3) → 4 → 5 → 6. End state, if both
gates resolve toward "keep the refusal": **26 of 34 executing for real, 7
protocol-terminated against stand-ins with negative controls, Dataflow Gen2
refusing by name** — and the honest sentence in `parity.md` is that every
documented activity either does its work observably or names exactly why not.

## Phase 7: the nine the plan never listed

**The activity list this plan was built from was the portal's, and the portal
is not the wire.** Diffing all 41 discriminators in ADF's published schema
against what the dispatch switch and the pipeline interpreter actually handle
found **nine type strings in neither** — and every one of them fell to the
dispatch default, which returns `{"status":"Succeeded"}`. They had been
counted as "not in the plan"; they were in fact being reported as done.

The default is right for a **connector leaf** — a ServiceNow source really was
reached in `dependsOn` order with its inputs resolved, and the emulator says
so. It is wrong for a **compute activity**, whose whole point is an effect
later steps consume. That distinction is what splits the nine:

| Type | Outcome | Why |
|---|---|---|
| `Validation` | 🟢 real | OneLake paths, real sizes, the virtual clock |
| `SqlPoolStoredProcedure` | 🟢 real | the Synapse spelling of an activity already implemented |
| `AzureDataExplorerCommand` | 🟢 real | the Kusto engine behind Eventhouse already runs |
| `HDInsightHive` / `Pig` / `MapReduce` / `Streaming` | 🔴 refused by name | no Hive/Pig/MapReduce runtime; a main class has no submission path |
| `DataLakeAnalyticsU-SQL` | 🔴 refused by name | U-SQL is its own language; the service is retired |
| `ExecuteSSISPackage` | 🔴 refused by name | no integration runtime, and the work is inside the package |

`Validation` is the one worth naming twice. Its entire purpose is to stop a
pipeline from processing data that has not landed, so a `Validation` that
always passes is **worse than no `Validation` at all**: the pipeline reads an
absent file with the guard's blessing.

**The reusable part is the method, not the list.** A plan derived from a
product surface will miss whatever the wire accepts and the surface does not
show. Diff the schema's discriminators against the dispatch, and check what
the default does with the remainder — a permissive default turns every gap
into a silent success.
