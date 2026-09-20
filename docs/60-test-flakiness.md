# 60 — Test flakiness: what was measured, what was fixed, what now guards it

**Status: the Go suite is clean under the race detector and under randomised
test order. Three load-dependent tests were rewritten. Two guards were added —
a CI job that runs both flags, and a static checker with a ledger.**

**Finding: this suite's flakiness exposure was LATENT, not active. The timing
discipline was already good; what was missing was any mechanism that would have
told us if it were not.**

Scoped to the Go suite — 285 `*_test.go` files. The pytest suite under
`python/tests/`, the sixteen `e2e/*/run.py` harnesses and the portal's vitest
suite are out of scope here and are not claimed to have been analysed.

No historical CI failure data is reachable from a checkout, so flakiness was
identified two ways rather than by mining past runs: an empirical sweep, and a
static classification of every timing-coupled construct in the tree.

## 1. The measured sweep

Every run below is `-count=1` (the test cache defeated, so each is a verdict on
this commit rather than a replay), on an 8-core darwin/arm64 machine, against
the tree at `0578c042`.

| Sweep | Command | Result | Wall time |
| --- | --- | --- | --- |
| Race detector | `go test -race -count=1 ./...` | **pass**, 28 packages, 0 races | 2m09s |
| Randomised order | `go test -shuffle=on -count=1 ./...` | **pass** | 27s |
| Both | `go test -race -shuffle=on -count=1 ./...` | **pass** | 2m04s |
| Repeat | `go test -count=2` over `store`, `api`, `server`, `tds`, `cmd/...` | **pass** | 49s |
| Baseline | `go test ./...` | pass | ~25s |

The slowest packages under `-race` are `internal/server` (117s) and
`internal/api` (84s); everything else is under 15s.

**The race detector had never run against this repository.** Before this change,
`grep -rn -- '-race' .github/workflows/ Makefile scripts/` returned nothing, and
the same grep for `-shuffle` returned nothing. That is the actual finding. The
suite contains 68 `go func` sites in test files, an event bus that dispatches to
subscribers on a goroutine of its own, and job drives deliberately built to
outlive the POST that starts them — and no instrumented run had ever been made.

The distinction matters: an unrun detector reports exactly the same silence
whether or not there is anything to find. The green above is the first evidence
that the answer is *no* rather than *unknown*.

Test order had the same shape. It was fixed, so it never varied, so the class of
bug where one test passes only because an earlier one left state behind was
unobservable by construction — not absent, unobservable.

## 2. Why the suite was already clean

Two decisions, both predating this work, account for most of it.

**A fake clock.** `internal/store` injects `store.Clock`, and tests advance it
(`st.Clock.Advance(120)`) rather than waiting on wall time. A job whose
completion is a function of the clock is therefore decided by an explicit call,
not by whether the scheduler got round to it. `advanceUntilTerminal` in
`internal/api/validationactivity_test.go:87` is the pattern: it advances the
clock *inside* a deadline-guarded poll, so an advance landing early simply moves
the deadline with it.

**Deadline-guarded polling as the house style.** 26 of the 29 sleeps in the tree
were already inside a loop carrying a bound. The comment on `awaitJob`
(`internal/api/notebookdrive_test.go:143`) is the reasoning written down: the
deadline is generous *on purpose*, because nothing there measures timing, so it
bounds only the failure case — a windows runner under load once took 106s for
that package, and a 5s ceiling had produced exactly the runner-starvation flake
the 30s ceiling removes.

## 3. The three-bucket inventory

All 29 `time.Sleep` sites in `*_test.go` at `0578c042`, classified.

### (a) BOUNDED POLL — 19 sites — correct, unchanged

A sleep inside a loop guarded by a deadline (`time.Now().Add(N)` or
`time.After(N)`). Fast machines return immediately; slow ones are bounded.

`cmd/fabric-emulator/main_test.go:124` · `internal/server/pipeline_test.go:123` ·
`internal/server/pipeline_sql_test.go:107` ·
`internal/server/forcelro_test.go:127,194,291` ·
`internal/server/variablelibrary_test.go:92,329` ·
`internal/server/events_transport_test.go:280` ·
`internal/tds/splice_integration_test.go:452` ·
`internal/api/armcapacities_test.go:163` ·
`internal/api/webhookactivity_test.go:60,122` ·
`internal/api/triggers_test.go:266` ·
`internal/api/validationactivity_test.go:87` ·
`internal/api/airflow_test.go:166,275` ·
`internal/api/notebookdrive_test.go:159` · `internal/store/bus_test.go:322`

### (b) BARE SLEEP-THEN-ASSERT — 3 sites — **rewritten**

A fixed sleep whose verdict is read immediately after. This is the only shape
whose outcome is a function of machine load, and all three had the same tell:
each asserts a **negative**.

| Site | The negative it asserts |
| --- | --- |
| `internal/api/sparkjobdrive_test.go:188` | with no agent, nothing drove the job |
| `internal/api/notebookdrive_test.go:284` | no engine reached the notebook |
| `internal/store/bus_test.go:356` | nothing was delivered after `Close` |

Why a negative is the dangerous case. Sleeping 100ms and then finding the job
still open is consistent with two different worlds: there is no drive, or there
is a drive that has not been scheduled yet. On a laptop it is the first; on a
loaded CI runner it can be the second, and then the assertion passes for a
reason unrelated to the code under test. The test does not fail — it stops
meaning anything, silently, on exactly the machine where it matters most.

The `bus_test.go` case was the sharpest: sleep 200ms, then take **one**
non-blocking read. Dispatch is asynchronous, so that read can land before the
dispatcher has run at all.

### (c) CONTAINER RETRY — 7 sites — accepted and recorded

One-second sleeps in the TDS suites, each inside `for i := 0; i < 60; i++`,
backing off against a real SQL Server that is still booting. Bounded by
iteration count rather than by a deadline, which is equally sound: the failure
case has a ceiling and the success case returns immediately. A shorter interval
would only poll a closed port faster.

The plan for this work expected six; there are **seven** —
`internal/server/tds_reflect_test.go` has two (the `GROUP BY` and its
`rows.Err` retry). All seven are recorded in `docs/test-flakiness.json`.

## 4. What the rewrites changed

`internal/testsupport/wait.go` adds two helpers:

- `WaitFor(t, timeout, msg, cond)` — poll until true, fail at the deadline.
- `StaysFalse(t, window, msg, cond)` — assert `cond` never becomes true across
  the whole window, failing at the first instant it does.

`StaysFalse` is what the three bucket-(b) sites wanted. The assertion is
unchanged — each still claims the same negative — but the claim is now made
across an explicit window instead of at whichever instant one sleep happened to
land on. A drive that fires 10ms in now fails the test; before, it could be
missed entirely.

The window is still a guess about how long is "long enough for the wrong thing
to have happened", and that has not been engineered away. It is now *visible*,
at the call site, in a parameter named after what it bounds — rather than a bare
`time.Sleep` with a comment explaining that it is not really a sleep.

`internal/testsupport` imports no other `internal/` package, so `internal/store`
and `internal/api` can both use it with no import cycle.

## 5. The guards

**`make test-race` and the `race` CI job** run
`go test -race -shuffle=on -count=1 ./...` on every push and pull request. One
ubuntu leg, not the three-OS matrix `test` carries: the detector instruments the
Go memory model, which does not vary by platform. Deliberately **not** folded
into `make test`, which is the target people run in a tight edit loop — a gate
that costs 5x the wall time gets run less, not more.

**`scripts/check_test_flakiness.py --strict`**, in `make check` and in the
`witnesses` CI job, is the static half. It flags three things:

1. an unbounded sleep — a `time.Sleep` not inside a loop carrying a bound;
2. an unbounded polling loop — a sleep in `for { }` with no ceiling, which does
   not fail when the awaited thing never happens, it hangs;
3. a long sleep — a single sleep of ≥ 1s even when bounded, surfaced because the
   aggregate cost is real and invisible at any one call site.

Categories 1 and 2 are what the ledger exists to keep **empty**; category 3 is
what it exists to **hold**. `docs/test-flakiness.json` records each accepted
site with its bucket and reason, keyed on file + enclosing symbol rather than
line number — a ledger that must be renumbered after every edit above it is a
ledger people delete entries from.

The ledger is checked in **both** directions. An entry naming a site that is no
longer flagged also fails, because a stale allowance is how a ban quietly stops
applying, and it is the direction a "flag what is new" checker would never look
at on its own.

### On the checker guarding a line that is already held

This checker passes against the tree today, which is the awkward case: a check
that passes now passes whether or not it works. Its test
(`python/tests/test_check_test_flakiness.py`) therefore drives it with
violations it must catch **and** with correct-looking near-misses it must not,
with the real repository as a single control at the end.

Two false positives were found and fixed by writing that test, both of which
would have made the checker something people argue with rather than fix:

- it walked `.claude/worktrees/`, a **nested git worktree** — a second checkout
  of this same repository — and reported findings against another branch's copy
  of a file, with a path that looks local and a line number matching nothing
  here. That worktree holds 247 more `*_test.go` files carrying 27 more sleeps,
  so any sweep that walks it sees 532 files instead of 285; the checker's first
  run reported three phantom findings out of it. (`golangci-lint` has been
  caught by the same nesting in this repo.)
- it missed `for i := 0; i < 60; i++ { // SQL Server may still be starting`,
  because the `for`-header pattern anchored on `{` at end of line and this one
  has a trailing comment — so a correctly bounded retry loop was reported as an
  unbounded sleep.

The checker was also mutation-tested: disabling the comment-stripping, the
stale-ledger check, and the sleep detection each fail the suite.

## 6. What is not covered

- **The pytest, e2e and portal suites.** Out of scope, as stated above.
- **Flakiness with no timing tell.** A test depending on map iteration order, on
  a port being free, or on the network would not be found by either the sweep or
  the checker. `-shuffle=on` covers cross-test order dependence; nothing here
  covers within-test nondeterminism that the race detector cannot see.
- **The windows and macOS legs under `-race`.** One ubuntu leg, by the reasoning
  in §5.
- **Rare races.** A clean `-race` run is evidence, not proof: the detector only
  reports races on memory accesses that actually happened, so a window a few
  instructions wide may simply not have been hit. `TestPublishDuringCloseDoesNotPanic`
  (`internal/store/bus_test.go`) is the suite's own acknowledgement of this — it
  loops 50 times precisely because one attempt would usually miss.
