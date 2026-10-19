# 64 — Logging quality: the one knob the surface ledger could not see, and the 214 failures that logged nothing

**Status: round 2.** Round 1 registered the vocabulary — every operational log
line names its subsystem, no library exits the process, every log-gating knob is
`FABRIC_*` — and [`scripts/check_logging_quality.py`](../scripts/check_logging_quality.py)
has held that shape against [`docs/logging-subsystems.json`](logging-subsystems.json)
ever since. Round 2 is not a second audit of the same dimension. It is the two
things round 1 explicitly left open, plus one dimension it never measured.

**The round-2 headline: 214 non-test call sites write a 500 and not one of them
logged anything.** 268 write some 5xx, across 47 files and seven error writers —
`writeErr` alone is 150 of them. Every `log.*` line in the tree belonged to a
background driver, a trace or a lifecycle path, so the single status that always
means *the emulator is broken, not the request* was the one class of failure with
no server-side record at all. The detail was not lost; it went the wrong way.
`writeErr(w, 500, "InternalError", err.Error())` echoes the error into the
response body, so the **client** learned what happened and the operator reading
the compose log learned nothing — not even that a 500 had occurred.

**The round-2 second finding: the trace printed a credential.** The
`FABRIC_ONELAKE_TRACE` lines printed `r.URL.RawQuery` and the `x-ms-copy-source`
and `x-ms-rename-source` headers verbatim, and all three carry an Azure SAS in
ordinary use. The asymmetry is inside this one repository:
[`internal/server/record.go`](../internal/server/record.go) refuses to record
response headers **at all**, and says why — "the Authorization header carries a
bearer token, and a recording written to a file that CI may upload as an artifact
is exactly the place a token should never be" — while the sibling diagnostic one
package over printed the same class of credential to stderr, which compose
captures and CI uploads by the identical route.

This is the sixth document on this architecture, after
[60-test-flakiness.md](60-test-flakiness.md)'s three language surfaces,
[62-performance-regressions.md](62-performance-regressions.md)'s unbounded
request bodies and [63-security-footguns.md](63-security-footguns.md)'s unread
source. The pattern is the same each time and it is the point: a dimension nobody
was measuring, measured; what is decidable from the source text alone turned into
a gate with a ledger checked in both directions; and what is not, written down
here as deferred rather than smuggled in.

### What round 1 said, kept as history

> **Status: 30 `log.*` call sites carry the whole of this emulator's account of
> its own failure paths, and nothing registered them. Four tag conventions
> coexisted, so no single grep selected one subsystem out of a compose log; one
> test in a tree of 522 Go files read a log line back; and `ONELAKE_TRACE` — the
> only log-gating environment knob not named `FABRIC_*` — was therefore invisible
> to `check_backward_compat.py`'s literal scan and sat outside the surface ledger
> entirely.**

That remains the right account of what round 1 found. It had also become the
document's stale present tense, which is its own small finding: see the drift
table below.

## The measured baseline

Every number below is reproducible from a clean checkout at `671bec12`, before
anything in this round changed. The commands are the evidence, not the prose.

```
# (a) every log call site in the compiled trees, as the guard counts them
python3 scripts/check_logging_quality.py --strict
    35 log call sites across 13 non-test files, naming 14 subsystems

# (b) the tree they sit in
git ls-files '*.go' | grep -v _test.go | wc -l     # 224 non-test sources
git ls-files '*.go' | wc -l                        # 563 total

# (c) every test that asserts a log line
grep -rln --include='*_test.go' 'log.SetOutput' internal cmd pkg
    internal/api/livy_catalog_test.go
    internal/onelake/blob_test.go              # added by round 1
    internal/server/record_test.go             # added by round 1
    internal/server/warehouseversioning_test.go

# (d) every non-test site that writes a 500, by the writer it goes through
grep -rn --include='*.go' StatusInternalServerError internal cmd pkg \
  | grep -v '_test\.go:' \
  | sed -E 's/^[^:]+:[0-9]+:[[:space:]]*//;s/\(.*//' | sort | uniq -c | sort -rn
    150 writeErr          10 writeDFSErr        3 writeJSONError
     24 writeJSON          7 writeBlobErr       1 writeKustoErr
     15 writeAtlasErr      4 &dfsError{…}
    = 214 sites across 47 files

# (e) ...and the other 5xx statuses, which the same fix covers
for s in StatusNotImplemented StatusBadGateway StatusServiceUnavailable; do …
    16 + 34 + 4  =  268 non-test 5xx-writing sites in total
```

### The drift the headline had acquired

Round 1's status line was written in the present tense and stopped being true the
moment anything landed. Recorded, because a document that states a stale number
confidently is worse than one that states none:

| Round 1 claimed | Measured at `671bec12` |
|---|---|
| 30 log call sites | **35** |
| across 12 non-test files | **13** |
| 13 subsystem tags | **14** |
| 1 test asserting a log line | **4** |
| 212 non-test Go sources / 522 total | **224 / 563** |

After this round: **36 sites across 14 files, naming 15 tags.** The one added
site is the fault log; the one added tag is `fault`.

### A correction to this round's own first measurement

The tally in (d) was first run as `grep -rh … | grep -v _test`, which is wrong and
not subtly: `grep -rh` suppresses filenames, so `grep -v _test` had nothing to
filter on and matched only lines whose *text* contained `_test`. That counted
every assertion in every test file and reported ~252 sites, including rows for
`http.Error` and `w.WriteHeader` that exist only in tests. The corrected form
above keeps filenames (`-rn`) and filters on them. **214, not 252.** Noted for
the same reason round 1 recorded its 31-versus-30 grep: the next person to
re-derive these numbers will reach for the same pipeline.

## Findings

| # | Finding | Evidence | Status |
|---|---------|----------|--------|
| **F1** | `ONELAKE_TRACE` is the only log-gating knob not named `FABRIC_*`, so `check_backward_compat.py`'s `_ENV_LITERAL = re.compile(r'"(FABRIC_[A-Z0-9_]+)"')` cannot see it. No `envVars` row, no `docsUndocumented` reason, no mention in `docs/` — unlike `FABRIC_TDS_TRACE`, which has both. | `internal/onelake/onelake.go:476`, `internal/onelake/blob.go:175` | **Fixed.** Renamed `FABRIC_ONELAKE_TRACE`; it is now row 39 of `compat-surface.json`'s `envVars` with a `docsUndocumented` reason. The legacy spelling is still read and is recorded in `logging-subsystems.json`. |
| **F2** | Four tag conventions coexisted, so no single grep selected one subsystem's lines out of a compose log. Three lines carried no tag at all. | bracketed: `onelake.go:480`, `blob.go:179` · space-delimited: `notebookdrive.go:186,193,199,369`, `sparkjobdrive.go:38,46,52,120` · untagged: `server.go:232,347`, `record.go:80` | **Fixed.** All normalised to the dominant `subsystem: ` form — 13 tags, now a closed set the guard enforces. |
| **F3** | No line carries a severity, so a benign notice and a broken session read identically — to a reader and to a grep. | `eventstream.go:63` (`topic will auto-create on first produce`) against `livy_catalog.go:98` (`lakehouse %s Files did NOT mount`) | **Recorded, not fixed.** See *What this audit did not do*. |
| **F4** | The set was unregistered: nothing asserted which subsystems log at all, so a handler that stops logging its failure path is indistinguishable from one that has no failures. | the absence of any ledger or test over the 30 sites | **Fixed.** `logging-subsystems.json` registers the vocabulary and is checked in **both** directions. |
| **F5** | Exactly one test in the tree asserted a log line; 29 of 30 sites had none. | `internal/api/livy_catalog_test.go:111` | **Partly fixed.** Three assertions added (below). The remaining sites are listed rather than claimed. |
| **F6** | **214 non-test sites write a 500 and none of them logs anything** (268 write some 5xx). All 35 log lines in the tree belong to background drivers, traces and lifecycle paths, so the one status that always means the emulator is broken was the one class of failure with no server-side record. | `internal/api/api.go:369` (`writeErr`, 150 call sites) · `internal/server/portal.go:162` · `internal/purview/datamap.go:149` · `internal/onelake/onelake.go` `writeDFSErr` · `internal/onelake/blob.go` `writeBlobErr` · `internal/api/kql.go:367` | **Fixed** at the root chokepoint: [`internal/server/fault.go`](../internal/server/fault.go), installed in `server.go:310` as `boundBodies(logFaults(routed))`. |
| **F7** | The `FABRIC_ONELAKE_TRACE` lines print `r.URL.RawQuery` and the `x-ms-copy-source` / `x-ms-rename-source` headers **verbatim**, and all three carry an Azure SAS. Meanwhile `record.go` refuses to record headers at all, for exactly this reason, two directories away. | `internal/onelake/onelake.go:480` (query + rename source) · `internal/onelake/blob.go:178` (query + copy source) · against `internal/server/record.go:37` | **Fixed.** [`internal/onelake/trace.go`](../internal/onelake/trace.go) redacts SAS parameter *values* and keeps their names; **R5** now gates the class. |
| **F8** | The `requestId` handed to the caller in every error envelope is never logged, so a user reporting "InternalError, requestId abc123" has nothing to correlate against. | `internal/api/api.go:374` and `:383` mint `store.NewID()` into the body; no log site anywhere reads it | **Recorded, not fixed.** See *What this audit did not do* — the ID is minted below the chokepoint, so fixing it is a different diff. |

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

**What round 2 buys F3, and what it does not.** A 5xx is the one severity
distinction decidable with no editorial judgement at all — it is a number the
protocol defines — so F6's middleware supplies the error/notice split *for the
responses*, and `grep 'fault: '` is now a severity filter in everything but
name. It buys nothing for the 35 pre-existing lines, and does not try to: the
retrofit stays deferred for the reason given below, and because F6's fix touches
none of them, adding severity later remains additive rather than a fight with
this gate.

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

### F6, in full, because the number is the argument

Seven error writers, 214 call sites, 47 files, zero log lines. The distribution
matters more than the total: `writeErr` alone accounts for 150, which is what
makes this a *class* rather than 214 oversights. Every one of those sites already
had the information — most of them pass `err.Error()` straight through — and
every one of them sent it to the client and nowhere else.

**The fix follows `boundBodies`'s own stated precedent, one function down in the
same file:**

> One bound at the root closes the class instead of the instances, which is why
> it is not 68 edits.
> — `internal/server/server.go:321`

The argument holds harder here, because a status is observable at the root
without knowing anything about which handler produced it. One middleware wraps
the control plane, the portal, the Purview Data Map and both OneLake dialects,
and covers the 215th site added tomorrow without an edit. It is **unconditional**
rather than env-gated: a 5xx is always abnormal, and gating the record of it
behind a knob reproduces exactly the silence this finding is about — the operator
who needs the line is the one who did not set the variable before the failure
they are now trying to explain. Unconditional also means no new row in
`docs/compat-surface.json`.

The line is deliberately thin:

```
fault: POST /v1/workspaces/ws-1/items -> 500 InternalError
```

Method, path, status, and the error code when the handler set one. **The path and
never the query** — F7's rule applied to F6's fix, and the stricter case of it,
because this line is unconditional where the trace at least requires somebody to
turn it on.

**The one real risk in this change is the ResponseWriter wrapper, not the
logging — and it cut both ways.** Two surfaces reach past the interface by *type
assertion*, which does not follow `Unwrap`: `terminal.go:157` asserts
`http.Hijacker` for the WebSocket upgrade, and `events.go:106` asserts
`http.Flusher` for SSE.

- A wrapper that merely **embedded** the writer makes both assertions *fail* — a
  dead terminal and a dead flow stream.
- A wrapper that declares `Flush` and `Hijack` **unconditionally** makes them
  both *succeed*, including over a connection that can do neither. `events.go`'s
  up-front check exists precisely so an unstreamable connection is refused with a
  clean 500 instead of hanging on a stream nobody can read, and a wrapper that
  always satisfies the interface deletes that check for every caller.

The second is the one that is easy to miss, and it is the one that happened:
`TestFlowStreamRefusesAConnectionItCannotStream` failed on the first attempt. So
`wrapFault` inspects the connection and returns the variant with the matching
method set — four types, which is the shape this problem has in Go, since a
method cannot be declared conditionally. `capture` in `record.go` gets away with
one unconditional `Flush` and no `Hijack` at all only because it is installed on
almost nothing: `s.rec` is nil on every ordinary run, and `recordable()` excludes
`/_emulator/`, which is where both of these endpoints live. This wrapper is
unconditional and wraps everything, so it can borrow neither excuse.

A hijacked connection writes its status straight to the socket, so nothing passes
through `WriteHeader`. That is treated as **not a fault** rather than defaulting
to 200: inventing a status either way would be reporting a number nobody wrote.

### F7, in full, and the asymmetry is the finding

Three values, all of which carry an Azure SAS in ordinary use:

| Printed verbatim | Why it is a credential |
|---|---|
| `r.URL.RawQuery` | **A SAS *is* a query string** — `sv=…&se=…&sp=…&sig=…`. `sig` is the HMAC; possession of it is possession of the access it grants. |
| `x-ms-copy-source` | By convention a **full SAS URL**. This is how `object_store` — and therefore delta-rs — implements rename, so it is the value most likely to be real in a real stack. |
| `x-ms-rename-source` | `renameSource`'s own comment says it "may carry a sas/query suffix". |

And the asymmetry, inside one repository, which is what makes this a finding
rather than a preference:

> No request bodies and **NO HEADERS** — the Authorization header carries a
> bearer token, and a recording written to a file that CI may upload as an
> artifact is exactly the place a token should never be.
> — `internal/server/record.go:37`

A log line goes to stderr; compose captures stderr; CI uploads compose logs. Same
destination, same risk, opposite decision, two directories apart.

**Redact the value, keep the name,** so the trace stays worth having. A reader
still sees that a SAS was presented and which parameters it carried — which is
most of what the trace is for, since a missing `se` or a wrong `sp` is the usual
reason a storage client is refused:

```
onelake-blob: PUT /ws/item/Files/dst.txt?sv=REDACTED&se=REDACTED&sig=REDACTED
    inm="" copy="https://acct.blob.core.windows.net/…/src.txt?sig=REDACTED" -> 201 (0B)
```

The **whole** SAS parameter set is redacted, not just `sig`: `sig` signs the other
parameters, so printing its signed inputs beside a redacted signature would be a
redaction in name only. Ordinary parameters — `resource=filesystem`,
`action=flush&position=4096`, `comp=list&prefix=…` — survive byte-for-byte, which
is the half that decides whether this is useful or merely safe. The tag and
format string are unchanged, so `grep 'onelake-dfs: '` still selects the same
lines.

### F8, in full, and why it is deferred rather than fixed

`writeErr` and `writeRetriableErr` mint `store.NewID()` as the `requestId` in
every error envelope (`internal/api/api.go:374`, `:383`). That ID is handed to the
caller and **logged nowhere**. A user reporting "InternalError, requestId abc123"
has nothing on the server side to correlate it against — including now, with the
fault log, which records the route but not the identifier the caller was given.

The fix is not free, and that is the whole reason it is deferred. The ID is
minted *inside* the helper, which is below the root chokepoint, so correlating it
means one of:

- **Six package-crossing helper edits** — threading the ID out of `writeErr`,
  `writeRetriableErr`, `writeJSONError`, `writeAtlasErr`, `writeDFSErr` and
  `writeBlobErr` into something the middleware can read, across `internal/api`,
  `internal/server`, `internal/purview` and `internal/onelake`.
- **Or a new response header** carrying the ID, which the middleware could read
  the way it already reads `x-ms-public-api-error-code` — but that changes the
  wire shape, and the wire shape is what the conformance recorder validates
  against Microsoft's swagger.

Both are defensible; neither belongs in a diff whose subject is "log the
failures". It is named here so it is a decision rather than an omission.

## The convention the guard now enforces

Five invariants, each measured against the tree before it was written. The
guard's own docstring carries the reasoning; this is the summary. R1–R4 are round
1's; **R5 is new.**

**R1 — every line names its subsystem.** `<tag>: ` prefixes the format string,
where `<tag>` is one of the 13 in `logging-subsystems.json`. Bare `<tag>:` is
accepted for `log.Println("tds:", line)`, because `Println` supplies the space
and the output a reader greps for is identical. A call whose first argument is
not a string literal has no format string to prefix and must be recorded with
its reason — there is exactly one, `log.Fatal(err)` in `main()`.

The 15 tags: `arm-capacities`, `eventstream`, **`fault`**, `lineage`, `livy`,
`notebook`, `notebook-drive`, `onelake-blob`, `onelake-dfs`, `record`,
`spark-job-drive`, `store`, `tds`, `versioning`, `warehouse`. Lower-kebab, so
`grep 'livy: '` is one subsystem and needs no shell quoting. (Round 1 listed 13;
`versioning` landed between rounds, and `fault` is F6's.)

**`fault` names an event class rather than a component,** which every other tag
does. A 5xx observed at the root has not yet been attributed to a subsystem —
the same argument the `main.go` exemption already makes about `log.Fatal`, and
the same one `boundBodies` makes about answering 413 in the control plane's
error shape ("a request refused before routing has not yet been attributed to a
surface"). Attributing it would mean editing the 214 sites or guessing from the
path, and a guessed tag is worse than an honest one: `grep 'fault: '` would then
select an incomplete set while looking complete.

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

**R4 — `--update` rewrites only the derived `subsystems` list.** `exemptions`,
`legacyEnvNames` and `redactionExemptions` are decisions somebody wrote down, so
regenerating must not be able to launder one away — the refusal
`check_backward_compat.py --update` makes explicit for the same reason. It
matters most for `redactionExemptions`, where a laundered entry does not merely
excuse an untagged line: it excuses a credential.

**R5 — no log argument reaches a credential un-redacted.** A `log.*` call whose
argument text reaches a credential-bearing source must pass it through a
redactor, or be recorded in `redactionExemptions` with the reason. Detected two
ways, as R3 already is, because each catches what the other cannot:

- **By source** — the argument names a field or header known to carry one:
  `.RawQuery`, `Header.Get("Authorization")`, `x-ms-copy-source`,
  `x-ms-rename-source`, `x-ms-encryption-key`, `Cookie`.
- **By name** — an identifier reads as a secret: `token`, `secret`, `password`,
  `sas`, `credential`, `bearer`, `signature`, and the joined pairs `apiKey`,
  `accessKey`, `sharedKey`, `privateKey`, `secretKey`.

Matched against *words*, not substrings — identifiers are split on non-letters
and at camelCase boundaries, because a substring scan for `sas` fires on any
format string containing those three letters (`assassination` is the one that
turned up in testing) and one for `name` fires on `Username`. Adjacent words are
also joined, which is how `apiKey` is caught without listing `key`, a word that
appears in `primaryKey` and `keyspace` and half the store package.

**`sig` is deliberately absent from the name list,** and that is a trade recorded
rather than hidden: it is a SAS parameter, but as an identifier it collides with
`sigChan`, `sigint` and `signal`, and a gate that fires on a signal handler is a
gate people switch off. The SAS `sig` is reached through `.RawQuery` or a
copy-source header, which the by-source rule already catches.

**Both directions.** A tag in the tree and not in the ledger is a stale ledger,
fixed with `--update`. An entry in the ledger and not in the tree **fails**: a
tag nobody emits goes on blessing a spelling, an exemption whose call site is
gone goes on excusing a file, a legacy env name nobody reads goes on excusing a
knob that is not there, and a **redaction** exemption whose call site is gone
goes on excusing a credential in a diagnostic — and would silently re-excuse it
if the call came back. A one-directional ledger only ever grows.

## Turning the logging up

Three knobs change what is printed. None changes what the emulator accepts,
which is why none has a row in
[04-configuration.md](04-configuration.md)'s table and all three carry a
`docsUndocumented` reason instead.

| Knob | What it prints | Tag to grep |
|---|---|---|
| `FABRIC_ONELAKE_TRACE=1` | every OneLake request, both dialects: method, path, query, range headers, status, bytes. SAS parameter **values** are redacted and their names kept (F7, [`internal/onelake/trace.go`](../internal/onelake/trace.go)). | `onelake-dfs:`, `onelake-blob:` |
| `FABRIC_TDS_TRACE=1` | every client→server TDS message, so a reader can see which message type carries a statement ([29-tsql-parity.md](29-tsql-parity.md), T6a) | `tds:` |
| `FABRIC_RECORD_RESPONSES=<file>` | not a log knob — the conformance recorder. It is here because its *failure* is logged, and that line is now asserted. | `record:` |

**The fault log has no knob, deliberately.** `fault: ` lines are emitted
unconditionally, because a 5xx is always abnormal and gating the record of it
behind a variable reproduces the silence F6 is about — the operator who needs the
line is the one who did not set the variable before the failure they are now
trying to explain. It is also why this round adds no row to
`docs/compat-surface.json`.

`ONELAKE_TRACE` (no prefix) is still read, so an existing shell export or
compose override keeps working. It is recorded in
[`logging-subsystems.json`](logging-subsystems.json) with the reason and goes
away in a release whose notes say so.

## What this audit did not do

Named here rather than left for a reader to discover, because each is a change
with its own diff and its own review. **F6 has moved out of this list** — it was
not in it, and now it is fixed; what follows is what remains open after two
rounds.

- **No severity retrofit (F3, deferred twice).** Adding a level token to the 35
  pre-existing sites means making 35 editorial judgements about which failures
  are warnings and which are errors, in one diff, with no test able to check any
  of them. It belongs in a change that can be argued with per line. F6's
  middleware supplies the one severity distinction that needs no judgement — a
  5xx is a number the protocol defines — and touches none of the 35, so the
  retrofit stays additive rather than a fight with this gate.
- **No `requestId` correlation (F8).** The ID is minted inside `writeErr`, below
  the root chokepoint, so correlating it means either six package-crossing helper
  edits or a new response header that changes the wire shape the conformance
  recorder validates. Argued in full above; it belongs in its own diff.
- **No `log/slog` migration, and no structured logging.** 36 call sites is not a
  library problem. The cost of a migration is paid in every one of them plus a
  handler, a format decision and a compose-log convention, to buy machine-
  parseable output that nothing in this repository currently parses. What the
  sites actually lacked was a *tag a human can grep*, and that is a convention.
  Revisit when something consumes the log programmatically — that is the
  trigger, and it has not happened.
- **27 of the 36 sites remain unasserted.** Listed above by file. The *set* is
  registered, which is the property that makes a subsystem going quiet a failing
  build; the individual lines are not. The fault log is asserted (14 Go tests in
  `fault_test.go` plus two through the real terminal and events routes), and so
  are both OneLake trace lines.
- **R5 cannot see a credential reached by an innocent name, or emitted from a
  helper one call away.** `conn.Password` is caught; `conn.Username` carrying a
  key is not, and neither is a secret composed into a local variable a call or
  two earlier and then logged under a plain name. It is a gate on the
  *convention* — a log argument that looks like a credential must show a
  redactor — and what makes it worth having is that the convention is checkable
  at all. **This is the same class of limit R3 already documents about itself,**
  and it is stated for the same reason: a gate whose blind spots are written down
  is a gate a reader can calibrate against.
- **The redactor knows Azure SAS parameter names and nothing else.** It is not a
  general secret scrubber and does not try to be. A credential arriving under
  some other name in some other field goes through untouched.
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
| `python/tests/test_check_logging_quality.py` | **55 tests** (41 before this round) driving the guard against every violation it must catch, plus this repository's own tree in **both** directions — a false positive fails `make check` for everyone, and a false negative is invisible because the real tree is clean. R5's pair matters most: one asserts the real tree has no leaky argument, the other asserts R5 still fires on the two trace lines **as they stood before the fix**, so "clean" is a statement about the tree and not about the rule having stopped matching. |
| `internal/server/fault_test.go` | 14 tests: the line's content, the 4xx/5xx boundary, the query-string refusal, and the four capability combinations of the wrapper. `terminal_test.go` and `events_transport_test.go` each add one driving the real route through it. |
| `internal/onelake/trace_test.go` | the redactor, and both trace call sites driven end to end — a redactor applied in `trace.go` and forgotten at a call site is the failure the unit tests cannot see |

**No CI change was needed.** The `witnesses` job at `ci.yml:1011` already runs
this guard with `--strict`, and this round adds no new script, so
`test_make_check_runs_in_ci.py`'s list agreement still holds.

Writing that test module found a live defect in the guard: `relkey` called
`Path.relative_to` without a fallback, so the error **reporter** crashed on a
path outside the repository root — which is exactly what a `tmp_path` fixture
is. The malformed-ledger cases it exists to report were the cases that killed
it. `check_backward_compat._rel` carries the same guard with the same comment,
measured the same way. Three tests found it; reading the function would not
have.
