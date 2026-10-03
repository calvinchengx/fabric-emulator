# 60 — Test flakiness: what was measured, what was fixed, what now guards it

**Status: the Go suite is clean under the race detector and under randomised
test order. Three load-dependent Go tests were rewritten (§3b), three Python
sites were (§7.2), and the portal's one was (§8.2). Three guards became four: a
CI job running all four, and one static checker with a ledger per surface.**

**Finding: this suite's flakiness exposure was LATENT, not active. The timing
discipline was already good; what was missing was any mechanism that would have
told us if it were not. That held for the Go suite (§1–5), it held again,
independently, for the Python surface (§7) — where nothing had inspected 105
current `time.sleep` sites and three of the five originally unbounded ones
turned out to be real —
and it held a third time for the portal's vitest suite (§8), whose one
real-clock site was the same shape as those three: a fixed sleep before a
negative assertion.**

Sections 1–5 are scoped to the Go suite — **328** `*_test.go` files as of this
revision (285 when they were written; the figure is refreshed rather than
reworded, because a count that drifts is how a measured document turns into an
approximate one). §7 covers the Python surface and §8 the portal's vitest
suite, both added later on the same architecture. Nothing under this document's
scope remains unanalysed; §6 records what a timing sweep cannot see at all.

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

A third was found by CI rather than by that test, and it is the one worth
reading. The checker built its ledger key with `str(path.relative_to(ROOT))`,
which spells a nested path with the HOST's separator — forward slashes on
POSIX, backslashes on Windows. The ledger is a checked-in JSON file, so it can
only be written one way. On the Windows legs nothing matched, and **both
directions of the both-directions check fired at once**: all seven recorded
sites read as unrecorded, and all six ledger entries read as stale. It took the
windows leg of `.github/workflows/make-targets.yml` red, and the windows leg of
the pytest job with it, while every Linux and macOS leg stayed green.

The bug is ordinary; what it says about the test is not. Every test in
`python/tests/test_check_test_flakiness.py` passed on a POSIX runner whether or
not the defect was present, so a suite written specifically to drive a checker
in both directions could not see a defect that broke both directions. A test
that only exercises the host's own path flavour is testing the host. The guard
now drives `pathlib.PureWindowsPath` explicitly, so it fails on Linux and macOS
too: reintroducing `str()` in place of `as_posix()` fails three tests on darwin,
where before it failed none. The normalisation lives in one function
(`relkey`), and writing it surfaced the same mistake one layer in —
`pathlib.PurePath(PureWindowsPath(...))` re-parses with the *host's* flavour
and silently discards the Windows one, so the root is never stripped. The new
test caught that on its first run.

The checker was also mutation-tested: disabling the comment-stripping, the
stale-ledger check, the path normalisation, and the sleep detection each fail
the suite.

## 6. What is not covered

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

## 7. The Python surface — the same sweep, one language over

§6's first bullet used to read "the pytest, e2e and portal suites. Out of scope,
as stated above." This section is what replaced most of it. The measurement and
the two guards are the same architecture as §5; what is different is the parser,
the ledger key, and the fact that three of the findings were real.

### 7.1 What was measured

Measured on the checker's own scan roots — the two pytest `testpaths`
(`python/tests`, `python/fabric-target/tests`) plus every `e2e/**/*.py` harness —
and refreshed against this revision:

| Quantity | Measured |
| --- | --- |
| Python files scanned | 247 |
| `time.sleep` call sites | **105** (all in `e2e/`) |
| …of those, sleeping ≥ 1s | **58** |
| Current checker findings | **58**: 56 bounded `long-sleep`, 1 accepted `unbounded-sleep`, 1 accepted `unbounded-poll` |
| Current strict result | **pass**: 58 accepted, 0 unrecorded, 0 stale |
| `while True` loops containing a sleep | 3 |
| Originally unbounded sites | **5**: 3 fixed, 2 intentionally accepted |
| Inspected by anything, before this | **0** |

The last row is the finding, exactly as it was in §1 for the race detector. 105
sleeps is not a large number for a fleet of e2e harnesses and most of them are
correct; the point is that nothing could have said so.

### 7.2 The five unbounded sites, and why three were fixed rather than recorded

They are not all the same thing, which is why they are listed individually
rather than waved through as a bucket.

| Site | What it is | Outcome |
| --- | --- | --- |
| `e2e/adls-sdk/driver.py:193` | `time.sleep(2)` then `assert not pipeline_runs()` | **fixed** |
| `python/tests/test_notebookutils_shim.py:292,301` | a 0.05s/0.01s pair ordering two threads | **fixed** |
| `e2e/engine-matrix/probes.py:196` | console-sink liveness probe | accepted, `liveness-probe` |
| `e2e/sail/driver.py:63` | infinite watchdog heartbeat | accepted, `watchdog-daemon` |

**The adls-sdk site was bucket (b) — a bare sleep before a NEGATIVE.** It is the
same shape as the three Go sites in §3(b), and the file's own comment says what
it is for: "The NEGATIVE half first: a write outside the watched prefix must
start nothing. Without it, a trigger that fires on every write would pass."
Sleeping two seconds and finding no run is consistent with two worlds — there is
no trigger, which is the claim, or there is one that has not been scheduled yet.
On a laptop it is the first; on a loaded CI runner it can be the second, and then
the assertion passes for a reason unrelated to prefix matching. Rewritten onto
`stays_empty`, the assertion is unchanged — it still claims no run started from a
write outside the watched prefix — but is now made across the whole window, so a
trigger that fires 10ms in fails instead of being missed.

**The notebookutils site was a sleep pair standing in for a happens-before.** The
worker thread bound a context, slept 0.05s and read; the main thread slept 0.01s
and read. That only orders the two while 0.01 reliably elapses before 0.05, and
under load it need not: a thread descheduled at the wrong moment inverts the
pair, both reads land on the same side of the other thread's bind, and the
isolation assertion stops meaning anything. It does not fail — it stops testing
anything. Rewritten onto `threading.Barrier` and `threading.Event`, so both
threads are provably bound before either reads and the worker provably holds its
binding until the main thread has read. Confirmed still load-bearing by mutation:
replacing `runtime`'s `ContextVar` with a process-wide global fails it.

**The two accepted sites are accepted for opposite reasons.** The console-sink
probe has no stronger assertion available — a console sink writes to the *server's*
stdout, so the client has nothing to read and nothing to poll, and polling
`isActive` until true would pass the instant the query started and stop
witnessing that it stayed up. The sail watchdog is an intentionally infinite
daemon heartbeat; bounding it would defeat it, since a watchdog that returns
stops watching and the run it was set to kill would hang to the CI job's own
timeout with no traceback.

### 7.3 The 56 bounded long sleeps

Not rewritten, and none asked to change: every one sits inside a loop the checker
already considers bounded — 44 in a finite `for` loop and 12 behind a
clock-checked `while` deadline (42 unique `file:symbol:kind` keys, several
covering more than one site). They are recorded for the reason §5 gives for the
seven container retries: the aggregate cost is real and invisible at any one call
site, so writing them down makes a fifty-seventh bounded long sleep a deliberate
decision rather than an unnoticed one. Three buckets, by what is actually being
waited on:

| Bucket | Keys | What it waits for |
| --- | --- | --- |
| `service-cold-start` | 16 | a container or binary this suite started that is not yet listening |
| `job-poll` | 25 | an async Fabric job, LRO or Livy statement reaching a terminal state |
| `engine-settle` | 1 | rows landing in a table an engine is writing asynchronously |

### 7.4 What the rewrites added

`e2e/waiting.py` — the Python analogue of `internal/testsupport`, placed at the
existing shared e2e import root (`e2e/entra_install.py` is already reached that
way by nine harnesses):

- `wait_for(timeout, cond, msg)` — poll until truthy, raise at the deadline.
- `stays_empty(window, probe, msg)` — assert `probe()` stays empty across the
  whole window, failing at the first instant it does not.

`window` is passed by name at the call site for the same reason `StaysFalse`'s
is: the guess about how long is "long enough for the wrong thing to have
happened" is not engineered away, it is made *visible* where the claim is made.
`wait_for` raises rather than asserts because an `assert` is stripped under
`python -O`, and a deadline guard that vanishes under `-O` is a real bug.

### 7.5 The guard

`scripts/check_python_test_flakiness.py --strict`, in `make check` and in the
`witnesses` CI job — the pairing that
`python/tests/test_make_check_runs_in_ci.py` enforces. It flags the same three
kinds as its Go sibling (`unbounded-sleep`, `unbounded-poll`, `long-sleep`), with
the same split: the first two are what `docs/python-test-flakiness.json` exists
to keep **empty**, the third is what it exists to **hold**, and the ledger is
checked in **both** directions.

**That split is enforced by the key, and the first version of this checker did
not enforce it.** The key is `file:symbol:kind`; review found it spelled inline in
three places with the kind missing from two, so a finding matched an entry on
`file:symbol` alone. 42 of the 44 entries are `long-sleep`, so those 42 symbols
were exempt from the unbounded-sleep and unbounded-poll bans entirely — a bare
`time.sleep(3)` before an assertion, added to a recorded symbol, printed
`accepted` and passed `--strict`. The compound case was worse: nine entries use
`<module>`, which by design covers a file's whole top level, and module level is
exactly where the `adls-sdk` defect §7.2 fixed lived — so the guard would not
have caught a recurrence of its own motivating bug in nine sibling drivers. The
key is now built by one function (`ledger_key`) that reads findings and entries
alike, because three inline spellings are what let two drift from the third, and
an entry carrying no kind is refused by name rather than silently accepting every
kind. **The Go sibling matches kind-blind for the same reason**; the impact there
is far smaller only because its ledger holds 6 entries against this one's 44, and
it is left alone here rather than changed in a Python-surface commit.

Three differences from the Go checker, each forced by the language rather than
chosen:

**It parses.** The Go checker must count braces to find a loop's extent, and has
the scar to show for it — a correctly bounded `for i := 0; i < 60; i++ {` read as
unbounded because the line carried a trailing comment. Python ships `ast`, so
extents here are exact and that whole class of false positive cannot occur.

**The ledger key has a `<module>` fallback.** The e2e harnesses are top-level
scripts, so `e2e/adls-sdk/driver.py:193` has no enclosing function — a position
Go has no equivalent of. One `<module>` entry covers every module-level site in
that file *of one kind*, the same many-to-one the Go ledger already carries for
`tds_reflect_test.go`. The symbol is the *outermost* enclosing function, so a
sleep in a nested helper is named by the test containing it.

**Body-deadline detection is a required feature, not a refinement.**
`e2e/fabric-cicd/driver.py` has two `while True` loops whose only bound is
`assert time.time() < end` in the loop *body* — correct code that a
header-reading check reports as violations, in the suite that publishes through
Microsoft's own fabric-cicd tool. A checker whose false positives land on correct
code gets argued with rather than fixed. Shape (c) requires the guard to **call a
clock in its own test** *and* to **leave the loop** (`assert`/`raise`/`break`/
`return`); that pair of conditions is what keeps the sail watchdog flagged
(`elapsed` is precomputed, and `os._exit` is neither) so it must be declared
rather than silently blessed.

Three further holes were found by driving the checker rather than reading it, and
all three are the same shape — a guard that was true of the tree as it stood and
silently false of the tree as anyone might next write it:

- **A deadline must bound *this* loop.** Shape (c) searched the loop's whole
  subtree with `ast.walk`, which crosses into nested loops and nested `def`s. A
  deadline in either place bounds something else, so a `while True` that genuinely
  spins forever read as correctly bounded. The search is now scoped to what the
  loop's own iteration governs, and a `break` inside a nested loop no longer
  counts as leaving the outer one — it does not. The counter search is
  deliberately *not* scoped the same way: an increment inside an inner loop still
  advances the outer loop's test, so pruning both would have flagged correct code.
- **A sleep imported by name is still a sleep.** All 123 sleep sites in this tree
  write the qualified `time.sleep(...)`, so matching the attribute alone passed —
  and `from time import sleep` is ordinary Python, so the next bare `sleep(5)`
  before an assertion would have been an *accident* the checker said nothing
  about. Resolved per file, so a local helper named `sleep` is not flagged.
- **A scan root that has been renamed away fails.** The vacuity guard is
  all-or-nothing and the sweep is not: `rglob` on a missing path yields nothing
  and raises nothing, so a moved root subtracts its whole share in silence. `e2e/`
  held almost all sleep sites when the checker landed; rename it and the checker
  would have walked only the pytest roots and printed success. A root that
  *exists* and holds no Python is still fine.

Report mode now prints the stale direction too. It had shown only unrecorded
sites, so the half of the contract a "flag what is new" reader would never think
to ask about was the half missing from the output someone actually reads.

It also skips `build/` alongside `.claude/`, for the same reason and a real one:
`python/fabric-target/build/lib/fabric_target/` is a **checked-in** setuptools
staging copy of a package that also exists at
`python/fabric-target/fabric_target/`, so a sweep that walked it would report the
same site under two paths, only one of which anyone edits.

### 7.6 On this checker also guarding a line already held

Same awkwardness as §5, same answer. `python/tests/test_check_python_test_flakiness.py`
drives it with violations it must catch (a module-level sleep, an unbounded
`while True`, a bounded ≥ 1s sleep) **and** with correct-looking near-misses it
must not: `for _ in range(60)`, a monotonic-deadline `while`, a counter-bounded
`while`, and both body-deadline spellings — plus the real
`e2e/fabric-cicd/driver.py` read from disk, so a transcription cannot drift away
from the file it claims to represent. Two near-misses assert the *limits* of the
bound rules: `for _ in itertools.count()` is a `for` loop that never ends, and
`while len(rows) < 3` is a comparison that is not a bound.

`pathlib.PureWindowsPath` is driven explicitly rather than left to the Windows
leg, because the Go sibling's one escaped defect was exactly there: a ledger key
built with `str()` instead of `as_posix()` made **both** directions of the
both-directions check fire at once, on Windows only, while every POSIX leg stayed
green. Reverting `as_posix()` here fails three tests on darwin.

Mutation-tested, sixteen ways. The original nine: disabling the body-deadline
bound, the stale-ledger check, the path normalisation, the sleep detection, the
vacuity guard, the `build/` skip, and the outermost-symbol rule each fail the
suite, as do over-broadening the counter bound to any comparison and dropping the
infinite-generator exclusion. Seven more cover the fixes above: dropping the kind
from the ledger key, accepting a kind-less entry, disabling the per-root guard,
not detecting a bare imported sleep, reverting the deadline scoping to
`ast.walk`, counting a nested-loop `break` as an exit, and pruning the counter
search at nested loops. A mutation that leaves the suite green means the
corresponding test is not testing anything.

## 8. The portal's vitest suite — the third surface, out of scope no longer

§6 used to carry a first bullet reading "the portal's vitest suite ... out of
scope as a separate toolchain." This section is what closed it, on the same
architecture as §7: a static sweep, a ledger, a guard in `make check` and in
CI. What is different here is smaller than either sibling — 24 test files
rather than hundreds, and a regex scan rather than a real parser, because
TypeScript has no `ast` module in Python's stdlib and this repository's
checkers do not import third-party packages (`docs/60` §5's own reasoning,
carried over unchanged).

### 8.1 What was measured

Every `*.test.ts` under `portal/src/` (`vite.config.ts`'s own `testpaths`:
`include: ['src/**/*.test.ts']`) — 24 files, and exactly one real-clock
`setTimeout` among them. Nine further sites across five files drive a *fake*
clock instead (`vi.useFakeTimers()` + `vi.advanceTimersByTimeAsync`), which is
not flagged at all: advancing a virtual clock is deterministic and races
nothing, the same reason a `for _ in range(60)` in the Python suite is not a
violation merely for containing a `time.sleep`.

### 8.2 The one real-clock site, and the fix

`Flow.test.ts`, "counts a dropped notice that does not say how many": a bare
`await new Promise((r) => setTimeout(r, 20))`, then `expect(...).not
.toBeInTheDocument()`. The same shape as the three Go sites (§3b) and the one
real Python site (§7.2) — a fixed sleep gating a **negative** assertion, which
is exactly the shape a single sleep-then-check cannot prove: it shows the chip
was absent *at the instant checked*, not that it stayed absent for the window.

Rewritten onto `staysAbsent` (`portal/src/testing.ts`), the vitest-side sibling
of `internal/testsupport.StaysFalse` and `e2e/waiting.py stays_empty`: it polls
the probe every 5ms across the window and fails at the first instant it stops
being empty, naming what was found. `docs/vitest-test-flakiness.json` therefore
ships **empty** rather than recording the site — the same choice the Python
ledger's own comment states as the standard to hold to: "an escape hatch that
is easier to reach than a fix is how a checker stops mattering."

`staysAbsent` is not itself in a `*.test.ts` file, so `check_vitest_test_flakiness.py`
does not see its own `setTimeout` — the same exemption `internal/testsupport/wait.go`
and `e2e/waiting.py` get from their siblings, for the same reason: a helper is
not a test, and bounding it correctly there is what a caller is trusting instead
of writing its own sleep.

### 8.3 The guard

`scripts/check_vitest_test_flakiness.py --strict`, in `make check` and in the
`witnesses` CI job. It flags every `setTimeout(` inside a `*.test.ts` file that
is not under `vi.useFakeTimers()` control at that point, in two kinds:

- `bare-sleep` — the shape the ledger exists to keep **empty**. `staysAbsent`
  covers the one legitimate use this suite has found (asserting an absence);
  anything else wanting a fixed real-clock wait belongs on the fake clock.
- `long-sleep` — a `bare-sleep` of a second or more. None exist today; the kind
  is defined so a slow one lands as a recorded decision rather than an
  unnoticed cost, exactly as the Go and Python ledgers use theirs.

**Fake-timer state is tracked in file order, top to bottom** — `useFakeTimers()`
turns it on, `useRealTimers()` turns it off, real timers are the default at the
top of every file. That is a straight-line reading of a file that is not itself
straight-line control flow, the same simplification the Go checker's
brace-counter makes about a loop's extent, and it is exactly right for every
file in this tree today: each block that switches to fake timers switches back
before the next real-clock wait (`Flow.test.ts` does this four times). It would
misread a file that toggled the clock from a shared helper called by several
tests rather than inline — there is no such helper here.

**The symbol is the enclosing `it`/`test` block's title**, with a `<module>`
fallback for a site outside any of them (a `beforeEach`, say) — the vitest
analogue of the Python ledger's `<module>` entries for e2e drivers with no
enclosing function. Matching the title needs a title-preserving comment strip
that is *not* also a string strip: an earlier draft ran both passes together,
which blanked `'flakes here'` into `''` along with every other quoted literal
and named every finding by the empty string — caught by
`python/tests/test_check_vitest_test_flakiness.py` failing on the very first
synthetic violation it was driven against, in this document's own first
category: found by driving the checker, not by reading it. `its_of` now takes
two passes — comments-only for the title match, comments-and-strings for the
brace-depth extent — so a title survives and a mock JSON payload's braces still
cannot shift a block's boundary.

Same three-part ledger key as the Python checker (`file:symbol:kind`), refused
by name when an entry carries no kind, and checked in **both** directions —
`docs/vitest-test-flakiness.json`'s single stale-entry test exists for the same
reason the other two ledgers' do.

### 8.4 On this checker also guarding a line already held

Same awkwardness as §5 and §7.6: the one real violation was fixed in the same
change that added the checker, so running it against the real tree proves only
that it did not crash. `python/tests/test_check_vitest_test_flakiness.py`
therefore drives it with synthetic violations it must catch — a bare sleep
before a positive assertion, a ≥1s sleep, a site outside any `it` block — and
with the fake-clock idiom it must *not* catch, including a test that switches
back to real timers partway through and must be judged on the real clock again
from that point on. Two tests pin the noise-stripping directly: a `setTimeout`
named inside a `//` comment or a string literal must not be flagged, and a
multi-line JSDoc block above a finding must not shift its reported line number.
The real tree is read from disk in a closing pair of tests — zero unrecorded
findings under `--strict`, and `findings_for` on the actual `Flow.test.ts`
returns nothing — so a passing synthetic suite cannot drift away from the file
it claims to describe.
