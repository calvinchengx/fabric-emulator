# `runMultiple` full-parity plan

`notebookutils.notebook.runMultiple` ships real DAG semantics — dependency
order, skip cascade, cycle refusal — but the parity row is 🟡 for reasons this
document turns into a worked plan. Phases are independent; each lands with its
own parity row, witness, and CI line.

The direction that matters throughout is **passes here, fails there**. A gap
that fails loudly on the emulator gets noticed; a gap the emulator papers over
ships a defect to real Fabric with a green local run.

## Phase 0 — pin the oracle

Several semantics below are inferred, not quoted. Before building to them,
confirm against current Microsoft docs and cite the source in the parity row.

| Question | Working assumption | Confidence |
|---|---|---|
| What `run()` returns | The child's exit value, not the status string | High that it's the exit value — which makes our status-string return itself a parity bug |
| `exitVal` in `runMultiple` results | The child's `notebookutils.notebook.exit()` value | High |
| Default `concurrency` | ~50 | Low |
| `message` vs `error` on failure | Failure reason lands in `message` | Low |
| DAG `timeoutInSeconds` expiry | Remaining activities marked, call returns | Low |
| Children share the parent's Spark session | Yes — the headline difference vs `run` | Medium-high |

Anything still unconfirmed after this ships as a **documented divergence**,
never a silent guess.

## Phase 1 — wrong answers shipping today (Python only)

Three fixes, one branch:

1. **`exitVal` is hardcoded `""`.** The value already flows end to end — the
   engine posts it, `finalizeNotebookRun` stores it, and
   `GET …/jobs/instances/{jid}/notebookRun` serves it
   (`internal/api/notebooks.go`). `run()` just never asks. Split the internals:
   a private `_run_detail()` returning `(status, exit_value, failure_reason)`,
   with `run()` and `runMultiple()` each projecting what they need.
2. **`timeoutPerCellInSeconds` is passed as a whole-notebook deadline**
   (`python/notebookutils/notebook.py`, the `runMultiple` → `run` call).
   Fabric's field is per cell; a 10-cell notebook given 90s/cell gets 900s
   there and 90s total here, so a legitimately slow DAG fails locally that
   passes in production. Scale by the run detail's cell count.
3. **`run()`'s return value**, contingent on Phase 0: if real Fabric returns
   the exit value, ours returning `"Completed"` is a parity bug. Fixing it is a
   breaking change — `e2e/notebookutils/notebook.py` asserts
   `status == "Completed"`, and consumers may too. Decision: follow the oracle,
   bump minor, fix our own witnesses in the same PR, release-notes entry.

Tests: move the stub in `python/tests/test_notebook_run_multiple.py` from
`run` down to `_run_detail` so exit values are expressible. Add: exit value
reaches `results[name]["exitVal"]`; no `exit()` call yields `""` not `None`;
a failed activity's failure-reason field matches the Phase 0 answer.

E2E: the notebookutils witness's `child-nb` is markdown-only on purpose (it
completes without an engine). Proving `exitVal` end to end needs a child with
a real cell calling `exit()` and an agent attached — extend
`e2e/notebook-driven` (which already asserts `exitValue == "3"`) rather than
weaken the engineless witness.

CI: `notebookutils` job (unit) + `notebook-driven` suite (e2e). No badge-count
change unless a new suite directory is added.

## Phase 2 — `useRootDefaultLakehouse` (Python + Go)

Accepted and ignored today; children attach their own binding. When `True`, a
child's unqualified `spark.table("x")` must resolve against the **root**
notebook's lakehouse. Crosses into Go: `driveNotebookRun` reads `run.Binding`
off the parsed item (`internal/api/notebookdrive.go`); the RunNotebook
`executionData` must be able to carry a binding override that
`parseNotebookRun`/`driveNotebookRun` prefer over the item's own.

Test the asymmetry or the test proves nothing: parent bound to lakehouse A,
child bound to B, child reads a table only A has. `True` → resolves,
`False` → fails.

CI: Go unit tests + an `e2e/notebook-driven` extension.

## Phase 3a — per-namespace catalog resolution (agent; a latent bug today)

Not a `runMultiple` feature — a standing defect. The agent holds **one**
SparkSession with per-Livy-session REPL namespaces
(`python/spark_agent/agent.py`), so two pieces of state are process-wide:
`spark.conf.set(...)` and `spark.catalog.setCurrentDatabase(schema)`. Two
notebooks bound to different lakehouses running concurrently fight over the
current database, and the loser silently reads the wrong tables.
`ThreadingHTTPServer` already permits this — no `runMultiple` involvement
needed. Same defect class as the `__nb_exit__` prelude race documented in
`internal/api/notebookdrive.go`.

Fix: qualify table names at registration so no current-database state is
needed, or set the current database per statement under a lock. Test: two
differently-bound sessions running concurrently, each asserting it sees its
own tables.

Independent of every other phase. Ship first.

CI: agent unit tests + `e2e/livy`.

## Phase 3b — bounded concurrency (Python; after 3a)

Sequential execution hides a real class of user bug: two independent
activities writing the same table collide on real Fabric and never collide
here. Concurrency is worth having as a **race-exposure** feature, not a
performance one. Cost is small — a concurrent notebook is a dict and a thread
on the agent, not a second SparkSession.

Decision, stated rather than implied: **default stays sequential** even if
Fabric's default is ~50, because reproducibility-by-default is the right
property for a test harness. Explicit `concurrency: N` is honoured with a
bounded pool per dependency level. The sequential default is recorded in the
parity row as a chosen, documented divergence.

Tests: rewrite "order within a level is the order given" as the
`concurrency=1` contract; add a `concurrency=2` test asserting genuinely
overlapping execution via enter/exit recording in the stub — never wall-clock
timing.

CI: `notebookutils` unit job.

## Phase 4 — `retry`, `retryIntervalInSeconds`, DAG `timeoutInSeconds` (Python)

Straightforward once Phase 0 pins expiry semantics. Tests:
retried-then-succeeded reports `Completed`; exhausted retries report the
*last* error; a DAG timeout leaves no activity in an undefined state;
dependents wait for a dependency's retries before deciding skip.

CI: `notebookutils` unit job.

## Phase 5 — shared parent Spark session: default **no**

Real Fabric runs children in the parent's session, so temp views and session
config carry across activities. Ours gives each child its own session,
`/close`d by a `defer` in `driveNotebookRun` — a child reusing the parent's
session must not close it, and shared-session plus Phase 3b concurrency
reintroduces exactly the prelude-race class of defect (two children in one
namespace racing on `__nb_exit__` and the `exit` patch).

Largest change on this list, makes two other phases harder, and the benefit is
narrow (sibling temp views). **Position: documented permanent divergence** —
"children run in isolated sessions" in the parity row — implemented only if a
real consumer demonstrates need.

## Phase 6 — progress table / Graphviz rendering: skip

Interactive-only. A text summary of final statuses is cheap if ever wanted;
Graphviz is a dependency for zero harness value.

## Sequencing

| Phase | Scope | Effort | Verdict |
|---|---|---|---|
| 3a per-namespace catalog | agent | M | **First — fixes a live bug** |
| 0 oracle | docs research | S | Before 1's return-type decision |
| 1 exitVal + per-cell timeout + return type | Python | S | **Now — wrong answers shipping** |
| 2 lakehouse inheritance | Python + Go | M | Do |
| 3b concurrency | Python | M | After 3a |
| 4 retry / timeouts | Python | S | Do |
| 5 shared session | Go + agent | L | No — document divergence |
| 6 progress UI | Python | S | Skip |

## Cross-cutting

The single 🟡 parity row cannot track six independently-landing phases —
split it: DAG ordering (🟢 today), exit values, lakehouse inheritance,
concurrency, retry/timeout, session sharing. Each row gets its own witness in
`docs/witnesses.json`; `scripts/check_witnesses.py --strict` stays clean.

Every phase's definition of done: parity row updated, witness naming a test
that exists, CI job named, README orchestration bullet still accurate.
