# 64 — Logging quality: the one knob the surface ledger could not see, and the log line nothing asserted

**Status: 30 `log.*` call sites carry the whole of this emulator's account of
its own failure paths, and nothing registered them. Four tag conventions
coexisted, so no single grep selected one subsystem out of a compose log; one
test in a tree of 522 Go files read a log line back; and `ONELAKE_TRACE` — the
only log-gating environment knob not named `FABRIC_*` — was therefore invisible
to [`check_backward_compat.py`](../scripts/check_backward_compat.py)'s literal
scan and sat outside the surface ledger entirely.
[`scripts/check_logging_quality.py`](../scripts/check_logging_quality.py) now
holds the shape, against
[`docs/logging-subsystems.json`](logging-subsystems.json).**

**The headline: a released binary honours a knob that appears in no
`envVars` list, carries no `docsUndocumented` reason, and is named nowhere in
`docs/` — so it could have been renamed or deleted with every gate in this
repository reporting green.** Its sibling `FABRIC_TDS_TRACE`, which does the
same job one package over, carries both. The difference between them was eight
characters of prefix.

This is the sixth document on this architecture, after
[60-test-flakiness.md](60-test-flakiness.md)'s three language surfaces,
[62-performance-regressions.md](62-performance-regressions.md)'s unbounded
request bodies and [63-security-footguns.md](63-security-footguns.md)'s unread
source. The pattern is the same each time and it is the point: a dimension
nobody was measuring, measured; what is decidable from the source text alone
turned into a gate with a ledger; and what is not, written down here as
deferred rather than smuggled in.

## The measured baseline

Every number below is reproducible from a clean checkout. The commands are the
evidence, not the prose.

```
# (a) every log call site in the compiled trees
grep -rnE '\blog\.(Printf|Println|Print|Fatal|Panic)' internal cmd pkg
    31 lines  ->  30 call sites across 12 non-test files

# (b) the tree they sit in
git ls-files '*.go' | grep -v _test.go | wc -l     # 212 non-test sources
git ls-files '*.go' | wc -l                        # 522 total

# (c) every environment variable that gates log output
grep -rnoE '"[A-Z][A-Z0-9_]*TRACE"|"FABRIC_[A-Z0-9_]*"' internal cmd pkg
    ONELAKE_TRACE, FABRIC_TDS_TRACE

# (d) every test that asserts a log line
grep -rn --include='*_test.go' 'log.SetOutput' internal cmd pkg
    internal/api/livy_catalog_test.go:111     # one, in the whole tree
```

**(a) is 31 lines and 30 sites, and the one-line difference is itself a
finding.** The thirty-first match is a *comment* in
`cmd/fabric-emulator/main.go`:

```go
// clean shutdown — and, because main would then log.Fatal, os.Exit would
```

A grep counts a sentence explaining `log.Fatal` as a call to it. The guard
therefore scans Go with a small state machine that treats comments, interpreted
strings, raw strings and rune literals as text rather than matching around them
— and reports 30. This is recorded because it is the first thing anyone
re-deriving these numbers will hit.

## Findings

| # | Finding | Evidence | Status |
|---|---------|----------|--------|
| **F1** | `ONELAKE_TRACE` is the only log-gating knob not named `FABRIC_*`, so `check_backward_compat.py`'s `_ENV_LITERAL = re.compile(r'"(FABRIC_[A-Z0-9_]+)"')` cannot see it. No `envVars` row, no `docsUndocumented` reason, no mention in `docs/` — unlike `FABRIC_TDS_TRACE`, which has both. | `internal/onelake/onelake.go:476`, `internal/onelake/blob.go:175` | **Fixed.** Renamed `FABRIC_ONELAKE_TRACE`; it is now row 39 of `compat-surface.json`'s `envVars` with a `docsUndocumented` reason. The legacy spelling is still read and is recorded in `logging-subsystems.json`. |
| **F2** | Four tag conventions coexisted, so no single grep selected one subsystem's lines out of a compose log. Three lines carried no tag at all. | bracketed: `onelake.go:480`, `blob.go:179` · space-delimited: `notebookdrive.go:186,193,199,369`, `sparkjobdrive.go:38,46,52,120` · untagged: `server.go:232,347`, `record.go:80` | **Fixed.** All normalised to the dominant `subsystem: ` form — 13 tags, now a closed set the guard enforces. |
| **F3** | No line carries a severity, so a benign notice and a broken session read identically — to a reader and to a grep. | `eventstream.go:63` (`topic will auto-create on first produce`) against `livy_catalog.go:98` (`lakehouse %s Files did NOT mount`) | **Recorded, not fixed.** See *What this audit did not do*. |
| **F4** | The set was unregistered: nothing asserted which subsystems log at all, so a handler that stops logging its failure path is indistinguishable from one that has no failures. | the absence of any ledger or test over the 30 sites | **Fixed.** `logging-subsystems.json` registers the vocabulary and is checked in **both** directions. |
| **F5** | Exactly one test in the tree asserted a log line; 29 of 30 sites had none. | `internal/api/livy_catalog_test.go:111` | **Partly fixed.** Three assertions added (below). The remaining sites are listed rather than claimed. |

### F3, in full, because it is the one a reader should be able to judge

These two lines are the same shape, the same severity to every tool, and
mean opposite things:

```
eventstream: CreateTopics orders: ... (topic will auto-create on first produce)
livy: lakehouse 8f3c… Files did NOT mount for session abc: ...
```

The first is routine — the topic appears on first produce and nothing is wrong.
The second means the session is broken and every path read in it will fail.
`grep -i error` finds neither. `grep DID NOT` finds the second only because
somebody shouted in capitals, which is a convention held by one line.

### F5, in full, because the fix is three assertions and not thirty

The three added here were chosen because each one pins a claim that was
asserted by nothing:

- **`internal/server/record_test.go` — `TestAnUnwritableRecordingPathSaysSo`.**
  `record_test.go:73` already drove the unopenable-path branch and asserted only
  that the recorder came back `nil` — which a branch that disabled recording
  *silently* would satisfy exactly. `newRecorder`'s own comment records what
  that silence cost: a medallion stack ran the emulator as a non-root user
  against a runner-owned bind mount, the open failed, the suite went **green**
  having recorded nothing, and the failure surfaced a job later in the
  aggregate conformance gate as seven routes missing traffic — naming the routes
  rather than the suite that had stopped recording. The log line is the entire
  fix for that, and it was asserted by nothing.
- **`internal/onelake/blob_test.go` —
  `TestBothTraceKnobNamesEnableTheTrace`.** Both knob names, because a
  compatibility read that nothing exercises is one that has already stopped
  working; and the tag, because the bracketed `[onelake-dfs]` spelling would
  pass every other test in that file.
- **`internal/onelake/blob_test.go` — `TestTheTraceIsOffWithNeitherKnob`.** The
  negative half. Without it, a trace that had become unconditional would pass
  the test above and nothing would say so.

**Still unasserted: 27 of the 30 sites.** They are listed here rather than
covered, because thirty log-capture tests would be thirty tests asserting the
emulator's prose:

| File | Sites |
|---|---|
| `internal/api/livy_catalog.go` | 8 (one asserted — `livy_catalog_test.go:111`) |
| `internal/api/notebookdrive.go` | 5 |
| `internal/api/sparkjobdrive.go` | 4 |
| `internal/server/warehouselineage.go` | 3 |
| `internal/server/server.go` | 3 |
| `internal/onelake/{onelake,blob}.go` | 2 (both now asserted) |
| `internal/api/eventstream.go`, `internal/warehouse/reflect.go`, `internal/store/db.go`, `cmd/fabric-emulator/main.go` | 1 each |

What the guard adds instead is the property a per-line test cannot: the *set* is
registered, so a subsystem that stops logging is a failing build rather than a
quiet absence.

### The two `recover()` handlers do log, and the third correctly does not

Checked, because a panic handler that swallows its panic is the worst case of
F4 — a crash that leaves no trace at all:

- `internal/api/notebookdrive.go:185` and `internal/api/sparkjobdrive.go:37`
  both log the panic **and** finalise the job as failed. Correct.
- `internal/pipeline/expr.go:164` recovers and converts the panic to a returned
  **error** rather than logging it. Also correct, and better: the caller gets a
  value it can act on. It is not a finding and is recorded here so nobody
  re-files it as one.

So F5 is a test-coverage finding, not a silent-swallow one.

## The convention the guard now enforces

Four invariants, each measured against the tree before it was written. The
guard's own docstring carries the reasoning; this is the summary.

**R1 — every line names its subsystem.** `<tag>: ` prefixes the format string,
where `<tag>` is one of the 13 in `logging-subsystems.json`. Bare `<tag>:` is
accepted for `log.Println("tds:", line)`, because `Println` supplies the space
and the output a reader greps for is identical. A call whose first argument is
not a string literal has no format string to prefix and must be recorded with
its reason — there is exactly one, `log.Fatal(err)` in `main()`.

The 13 tags: `arm-capacities`, `eventstream`, `lineage`, `livy`, `notebook`,
`notebook-drive`, `onelake-blob`, `onelake-dfs`, `record`, `spark-job-drive`,
`store`, `tds`, `warehouse`. Lower-kebab, so `grep 'livy: '` is one subsystem
and needs no shell quoting.

**R2 — no `log.Fatal*`/`log.Panic*` outside `cmd/`.** A library that exits the
process cannot be tested, cannot be recovered from, and takes its caller's
deferred cleanup with it — here that includes the SQLite close in `store`, and
the coverage counters a `go build -cover` binary writes only when `main`
*returns* (see `signalStop` in `main.go` for what that cost once). **Ships at
one hit, in `cmd/`, recorded: a pure regression guard.**

**R3 — every knob that gates log output is named `FABRIC_*`,** so it lands in
`compat-surface.json`'s `envVars` and cannot escape the surface ledger. A knob
is identified two ways — by its name (`TRACE`, `DEBUG`, `VERBOSE`, `LOG`) and by
whether the `if` it guards emits log output — because each catches what the
other cannot. Neither can see a knob with an innocent name whose output is
emitted somewhere else; that limit is written into the guard rather than papered
over.

**R4 — `--update` rewrites only the derived `subsystems` list.** `exemptions`
and `legacyEnvNames` are decisions somebody wrote down, so regenerating must not
be able to launder one away — the refusal `check_backward_compat.py --update`
makes explicit for the same reason.

**Both directions.** A tag in the tree and not in the ledger is a stale ledger,
fixed with `--update`. An entry in the ledger and not in the tree **fails**: a
tag nobody emits goes on blessing a spelling, an exemption whose call site is
gone goes on excusing a file, and a legacy env name nobody reads goes on
excusing a knob that is not there. A one-directional ledger only ever grows.

## Turning the logging up

Three knobs change what is printed. None changes what the emulator accepts,
which is why none has a row in
[04-configuration.md](04-configuration.md)'s table and all three carry a
`docsUndocumented` reason instead.

| Knob | What it prints | Tag to grep |
|---|---|---|
| `FABRIC_ONELAKE_TRACE=1` | every OneLake request, both dialects: method, path, query, range headers, status, bytes | `onelake-dfs:`, `onelake-blob:` |
| `FABRIC_TDS_TRACE=1` | every client→server TDS message, so a reader can see which message type carries a statement ([29-tsql-parity.md](29-tsql-parity.md), T6a) | `tds:` |
| `FABRIC_RECORD_RESPONSES=<file>` | not a log knob — the conformance recorder. It is here because its *failure* is logged, and that line is now asserted. | `record:` |

`ONELAKE_TRACE` (no prefix) is still read, so an existing shell export or
compose override keeps working. It is recorded in
[`logging-subsystems.json`](logging-subsystems.json) with the reason and goes
away in a release whose notes say so.

## What this audit did not do

Named here rather than left for a reader to discover, because each is a change
with its own diff and its own review:

- **No `log/slog` migration, and no structured logging.** 30 call sites is not a
  library problem. The cost of a migration is paid in every one of them plus a
  handler, a format decision and a compose-log convention, to buy machine-
  parseable output that nothing in this repository currently parses. What the 30
  sites actually lacked was a *tag a human can grep*, and that is a convention.
  Revisit when something consumes the log programmatically — that is the
  trigger, and it has not happened.
- **No severity retrofit (F3).** Adding a level token to all 30 sites means
  making 30 editorial judgements about which failures are warnings and which are
  errors, in one diff, with no test able to check any of them. It belongs in a
  change that can be argued with per line. The guard does not enforce severity
  and the ledger does not record it, so adding one later is additive rather than
  a fight with this gate.
- **27 of 30 sites remain unasserted.** Listed above by file. The *set* is
  registered, which is the property that makes a subsystem going quiet a failing
  build; the individual lines are not.
- **R3 cannot see a knob with an innocent name whose log output is emitted
  elsewhere.** The two detection rules and their limit are in the guard's
  docstring. R3 is a gate on the naming convention; what makes it worth having
  is that the convention is checkable at all.
- **Out of scope by design:** the Python harnesses (`python/`, `e2e/`,
  `scripts/`) and the portal. They have their own conventions, their own
  toolchains and — for the e2e suites — their own output contracts with CI. This
  audit is about the **emulator's own** operational logging, which is what a
  user of the published image reads.

## Where this is enforced

| Where | What |
|---|---|
| `make check` | `scripts/check_logging_quality.py --strict` |
| `.github/workflows/ci.yml`, `witnesses` job | the same script, the same flag — required, because [`test_make_check_runs_in_ci.py`](../python/tests/test_make_check_runs_in_ci.py) asserts the two lists agree and `LOCAL_ONLY` is empty |
| `python/tests/test_check_logging_quality.py` | 41 tests driving the guard against every violation it must catch, plus this repository's own tree in **both** directions — a false positive fails `make check` for everyone, and a false negative is invisible because the real tree is clean |

Writing that test module found a live defect in the guard: `relkey` called
`Path.relative_to` without a fallback, so the error **reporter** crashed on a
path outside the repository root — which is exactly what a `tmp_path` fixture
is. The malformed-ledger cases it exists to report were the cases that killed
it. `check_backward_compat._rel` carries the same guard with the same comment,
measured the same way. Three tests found it; reading the function would not
have.
