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

## Checklist

Code and its tests are separate rows on purpose: an implementation item that
carries its own proof inside it is how an untested change reads as done.

### Phase 0 — pin the oracle (research, no code)

| # | Action item | Output |
|---|---|---|
| 0.1 | Confirm what real Fabric's `run()` returns (exit value vs status) | Decision recorded here; gates 1.5 |
| 0.2 | Confirm failure reporting shape: `message` vs `error`, `exitVal` on failure | Gates 1.8 |
| 0.3 | Confirm default `concurrency` (~50?) | Gates 3b wording |
| 0.4 | Confirm DAG `timeoutInSeconds` expiry behaviour | Gates 4.3 |
| 0.5 | Confirm children share the parent's Spark session | Confirms Phase 5's divergence wording |
| 0.6 | Cite each source here; unconfirmed items marked "documented divergence" | This document |

### Phase 3a — per-namespace catalog (first: live bug)

| # | Action item | Files | Test proving it |
|---|---|---|---|
| 3a.1 | Remove process-wide current-database state: qualify table names at registration (or per-statement lock) | `python/spark_agent/agent.py` — `register_tables`, `ns` | 3a.2 |
| 3a.2 | Concurrency test: two sessions bound to different lakehouses run simultaneously; each sees only its own tables | agent tests | new |
| 3a.3 | Mutation check: revert the fix, confirm 3a.2 fails | — | one-off, noted in PR |
| 3a.4 | Verify `e2e/livy` + `e2e/notebook-driven` still green | CI | existing suites |

### Phase 1 — wrong answers shipping today

| # | Action item | Files | Test proving it |
|---|---|---|---|
| 1.1 | Extract `_run_detail()` → `(status, exit_value, failure_reason)` fetching `…/notebookRun` after terminal state | `python/notebookutils/notebook.py` | covered via 1.6–1.8 |
| 1.2 | Move test stub from `run` to `_run_detail` | `python/tests/test_notebook_run_multiple.py` | all 14 existing tests still pass |
| 1.3 | `runMultiple` populates `exitVal` from the child's exit value | notebook.py | 1.6 |
| 1.4 | Fix `timeoutPerCellInSeconds`: scale by the run detail's cell count, not whole-notebook | notebook.py | 1.7 |
| 1.5 | Apply the 0.1 decision to `run()`'s return value (breaking if exit value) | notebook.py | e2e assertion updated in same PR |
| 1.6 | Tests: exit value reaches `exitVal`; no `exit()` call → `""` not `None` | tests | new ×2 |
| 1.7 | Test: N-cell notebook with per-cell timeout T gets N×T deadline | tests | new |
| 1.8 | Test: failed activity's reason lands in the field 0.2 says | tests | new |
| 1.9 | E2E: extend `e2e/notebook-driven` — parent `runMultiple` over a child that calls `exit()`; assert the value round-trips through a real engine | `e2e/notebook-driven/` | new witness step |
| 1.10 | If 1.5 breaks `run()`: update `e2e/notebookutils/notebook.py`, release-notes entry, minor bump | e2e, docs/release-notes | existing witness |

### Phase 2 — `useRootDefaultLakehouse`

| # | Action item | Files | Test proving it |
|---|---|---|---|
| 2.1 | `executionData` carries an optional binding override | `internal/api/notebookdrive.go`, `notebooks.go` | 2.4 |
| 2.2 | `driveNotebookRun` prefers the override over the item's binding | notebookdrive.go | 2.4 |
| 2.3 | `runMultiple` passes the root binding when `useRootDefaultLakehouse=True` | notebook.py | 2.5 |
| 2.4 | Go test: job submitted with override registers the override's tables | Go unit | new |
| 2.5 | Python test: flag `True` passes root binding; `False` doesn't | tests | new ×2 |
| 2.6 | E2E asymmetry test: child bound to B reads a table only in A — `True` resolves, `False` fails | e2e/notebook-driven | new, both directions |

### Phase 3b — bounded concurrency (after 3a)

| # | Action item | Files | Test proving it |
|---|---|---|---|
| 3b.1 | Bounded pool per dependency level; default stays sequential | notebook.py | 3b.3–3b.5 |
| 3b.2 | Rewrite order test as the `concurrency=1` contract | tests | rewritten |
| 3b.3 | Test: `concurrency=2` → genuinely overlapping execution (enter/exit recording, no wall-clock) | tests | new |
| 3b.4 | Test: failure during concurrent level still skips dependents correctly | tests | new |
| 3b.5 | Test: pool never exceeds N in-flight | tests | new |
| 3b.6 | Parity row: sequential default recorded as chosen divergence | parity.md | witness check |

### Phase 4 — retry / timeouts

| # | Action item | Files | Test proving it |
|---|---|---|---|
| 4.1 | Per-activity `retry` + `retryIntervalInSeconds` | notebook.py | 4.4–4.5 |
| 4.2 | DAG-level `timeoutInSeconds` wall clock | notebook.py | 4.6 |
| 4.3 | Expiry behaviour per 0.4 | notebook.py | 4.6 |
| 4.4 | Tests: retried-then-succeeded → `Completed`; exhausted retries → *last* error | tests | new ×2 |
| 4.5 | Test: dependents wait out a dependency's retries before deciding skip | tests | new |
| 4.6 | Test: DAG timeout leaves no activity in an undefined state; interval honoured without real sleeps (injectable clock) | tests | new ×2 |

### Phases 5–6 — documentation only

| # | Action item | Files |
|---|---|---|
| 5.1 | Parity row: "children run in isolated sessions" as permanent documented divergence | parity.md |
| 6.1 | Skip recorded above — nothing further | — |

### Cross-cutting (definition of done, every phase)

| # | Action item | Verification |
|---|---|---|
| X.1 | Split the single 🟡 row into 6: ordering (🟢 now), exit values, lakehouse inheritance, concurrency, retry/timeout, session sharing | `docs/parity.md` |
| X.2 | One witness per new row in `docs/witnesses.json` | `check_witnesses.py --strict` exit 0 |
| X.3 | Each phase names its CI job; new e2e steps live in existing suites (no badge-count change) — if a new suite directory is ever added, update the badge count | CI config + badge |
| X.4 | Coverage floor: new Python keeps total ≥70% | coverage job |
| X.5 | README orchestration bullet re-checked after each phase | README.md |
| X.6 | This document updated as phases land (mark done, record the 0.x answers) | doc 39 |
| X.7 | Release-notes entries for behaviour changes (1.5 especially) | docs/release-notes |

**Totals:** ~45 items — 6 research, 13 implementation, 19 new/rewritten tests,
7 doc/CI. The test-heavy ratio is deliberate: 1.6–1.8 and 3a.2 are the rows
that make the parity claims mean anything.
