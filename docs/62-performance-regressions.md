# 62 — Performance regressions: what could be measured, what could not, and what now guards it

**Status: no performance regression in this repository is detectable by
measurement, and that is a finding rather than a gap in this document.
`func Benchmark` returns 0 hits across 552 `*_test.go` files and no stored
timing baseline exists anywhere in the tree. What was gateable instead — per-request
work whose size the caller chooses and nothing bounds — was measured, repaired,
and is now held by `scripts/check_perf_regressions.py` with a ledger that ships
empty.**

**The headline number: 70 sites consumed an inbound request body against 0 uses
of `http.MaxBytesReader` in the entire repository.** The tree was not unguarded,
which is what made this hard to see — it was *half*-guarded, in a way that read
as fully guarded.

This is the fourth document on this architecture, after
[60-test-flakiness.md](60-test-flakiness.md)'s three language surfaces. It
follows the same shape: measure first, repair what is real, record what is
accepted, and gate the class rather than the instances.

## 1. Why there is no measurement to regress against

| Probe | Command | Result |
| --- | --- | --- |
| Benchmarks | `grep -rn '^func Benchmark' --include='*_test.go'` | **0** across 552 test files |
| Stored timings | any committed baseline, profile or budget | **none** |
| `http.MaxBytesReader` | `grep -rn 'MaxBytesReader'` | **0** in the whole tree |

A regression spotter needs a baseline and a comparison. Neither exists here, and
building both — a benchmark corpus plus a harness that compares runs across
commits, with the noise discipline that makes such a comparison mean anything on
a shared CI runner — is a substantially larger piece of work than this one. It is
not attempted here, and this document does not claim it.

So the question was narrowed to one that can be answered offline, deterministically,
from source: **what work is unbounded by construction?** That needs no stopwatch.
A handler whose allocation is chosen by whoever sent the request is a regression
waiting for a caller, whether or not any benchmark exists to catch it.

## 2. The measured sweep

Over 207 non-test Go files under `internal/` (395 repo-wide excluding `vendor/`):

| Shape | Count | Bounded? |
| --- | --- | --- |
| `json.NewDecoder(r.Body)` | **68** | **no ceiling at all** |
| `io.ReadAll(r.Body)` (bare) | **2** | **no ceiling at all** |
| `httpx.ReadBounded(...)` | 23 sites, 16 files | yes — one of 8 named ceilings |
| `http.MaxBytesReader` | **0** | — |
| `regexp.MustCompile` / `Compile` | 9 | all 9 at package scope |
| `func Benchmark` | 0 | — |

**The half-guarded shape is the finding.** `internal/httpx` exists because this
project got body-bounding wrong in eight places at once, and it fixed them
thoroughly: every site that reads a body *as bytes* goes through `ReadBounded`
with a ceiling chosen by the handler that knows what it is reading. That work was
done, documented, and guarded by `internal/httpx/guard_test.go`.

It left the decoders untouched. A `json.NewDecoder(r.Body)` allocates as it
*reads* — it never calls `ReadAll`, so no ceiling was ever in its path. Before
this change any client could make the emulator allocate whatever it chose to
send, on any of 68 control-plane entry points, and a 69th added the next morning
would have inherited that with nothing to notice it.

Two further things nothing covered:

- **`internal/httpx/guard_test.go` does not catch a missing bound.** It bans the
  `io.ReadAll(io.LimitReader(...))` idiom — a bound that *truncates silently* —
  and is blind to the absence of a bound altogether. So the two bare
  `io.ReadAll(r.Body)` sites passed it, correctly, having no bound at all.
- **`resp.Body` reads are a different question.** There are 3 `io.ReadAll(resp.Body)`
  and 6 `json.NewDecoder(resp.Body)` sites. An engine the emulator relayed *to* is
  not an untrusted caller choosing a size, and the ones that matter are already
  bounded on their own terms (`mlflow.go` and `kql.go` both read through
  `MaxProxyBody`). The checker deliberately keys on the *receiver* name, not on
  `.Body`, so it does not report these nine as findings.

## 3. The repair

A ledger of 70 accepted entries would document the problem instead of closing it,
so the same change landed the fix.

**One outer bound at the root handler** (`boundBodies`, in
`internal/server/server.go`),
which is the only ceiling those 68 decoder sites ever needed. One bound closes the
class; 68 edits would have closed the instances and left the shape intact.

It wraps the **data plane too**, unlike the response recorder beside it. The
recorder is diagnostics and has no business on a Delta file's response path; this
is a memory bound, and the data plane is the surface most *able* to exhaust
memory. Excluding it — as an earlier draft of this work did — would have left the
largest reads in the system as the only unbounded ones.

**Two paths, because `http.MaxBytesReader` alone cannot answer 413.** It makes
reads *fail*; the handler that was decoding then reports its own error, so an
oversized body would be refused as "malformed JSON" — telling the caller
something untrue about their own request. So a declared `Content-Length` over the
bound is refused unread with **413**, and anything else (chunked, or no declared
length) is wrapped in `MaxBytesReader`, where the allocation is still capped and
the handler reports in its own voice.

**Both bare reads moved onto `httpx.ReadBounded`** with `MaxControlBody`:
`internal/onelake/principalaccess.go` (a principal id and a path) and
`internal/api/dataaccessmode.go` (`{"dataAccessMode": ...}`).

### 3.1 The ordering, which is the half nobody would check

`FABRIC_MAX_REQUEST_BYTES` defaults to **320 MiB**, and the specific number is
load-bearing:

| Ceiling | Value |
| --- | --- |
| `MaxBlobWrite` (largest inner) | 256 MiB |
| `MaxProxyBody` (MLflow / Kusto relay) | 128 MiB |
| **`DefaultMaxRequestBody` (outer)** | **320 MiB** |

Two designs were rejected by this table, and both looked correct:

- **A 64 MiB control-plane bound** would have silently *halved* two documented
  128 MiB relay paths — `internal/api/mlflow.go` and `internal/api/kql.go` both
  read with `MaxProxyBody`, and both live in the control-plane package.
- **A 256 MiB outer bound** — exactly `MaxBlobWrite` — would have been worse,
  because it would have *worked*. `ReadBounded` detects overflow by probing
  `max+1`, so an outer limiter set at an inner ceiling makes the probe itself
  the thing that trips. The oversized blob write is still refused, but with
  net/http's generic "request body too large" instead of `httpx`'s specific
  fit-vs-truncated message — a silent downgrade of the diagnosis on the one
  ceiling Microsoft's `fab cp` has actually crossed
  ([34-fab-driven-example.md](34-fab-driven-example.md)).

The bound must therefore sit **strictly above** every inner ceiling. That is
asserted twice: statically by the checker's `ceiling-above-outer-bound` kind, and
in Go by `TestTheOuterBoundSitsAboveTheOneLakeCeilings`.

### 3.2 Why it is a knob

It **narrows what a released binary accepts**, and no such narrowing may ship
unremarked — the premise `scripts/check_backward_compat.py` exists on. So
`FABRIC_MAX_REQUEST_BYTES` is documented in
[04-configuration.md](04-configuration.md), promoted into
`docs/compat-surface.json` as a reviewable diff, and **`0` means unlimited**:
anyone posting something larger than 320 MiB today is doing so successfully, and
the honest migration is a documented way to keep doing it rather than a number we
guess is big enough.

`0` is also why the default is applied by `FromEnvPartial` and **not** by
`Finish()`. `0` is both the zero value and the documented escape hatch;
defaulting in `Finish` would make `FABRIC_MAX_REQUEST_BYTES=0` mean 320 MiB,
which is the one thing it must not mean.

## 4. What the checker flags

Four kinds. Two are about the *original shape*; two are about whether the
*repair still holds*, because a bound that exists is not the same as a bound that
still applies.

| Kind | Entries today | Why |
| --- | --- | --- |
| `unbounded-body-read` | 0 | both sites repaired onto `ReadBounded` |
| `missing-outer-bound` | 0 | asserts the root handler still installs `MaxBytesReader` |
| `ceiling-above-outer-bound` | 0 | asserts the ordering in §3.1 |
| `recompiled-regexp` | 0 | all 9 sites are package-level |

`missing-outer-bound` and `ceiling-above-outer-bound` are **absence-shaped**, and
absence is what a per-line sweep cannot report: no line anywhere says "the bound
is gone". Without them the checker would pass in perpetuity on a tree whose one
ceiling had been refactored away, reporting zero unbounded reads — which would be
true, and beside the point.

`recompiled-regexp` ships empty on purpose, the precedent `long-sleep` set in
[`docs/vitest-test-flakiness.json`](60-test-flakiness.md): the kind is defined so
a future per-call compile lands as a recorded decision rather than an unnoticed
cost.

## 5. What is deliberately NOT flagged, and the measurements that ruled it out

The loop-shaped heuristics one reaches for first were each probed over this tree
and rejected. They are recorded here so the next person does not re-litigate them.

| Heuristic | Hits | Verdict |
| --- | --- | --- |
| `marshal-in-loop` | 45 | rejected — nearly all legitimate per-item work |
| `string-concat-in-loop` | 6 | rejected — too noisy to gate on |
| `sort-in-loop` | 4 | rejected — too noisy to gate on |
| `defer-in-loop` | 1 | rejected — **the one hit is a false positive** |

The `defer-in-loop` hit is `internal/tds/server.go:100`, and it is worth naming
because it is the clearest case: the `defer` is inside a **goroutine closure**,
not the loop body, so it runs once per connection exactly as intended. A
brace-depth walk cannot tell those two apart — which is the whole problem with
the class, and the same limitation
[60-test-flakiness.md](60-test-flakiness.md) records for the Go flakiness
checker's loop-extent counting.

A list response that marshals each of its elements is not a defect. Gating on 45
such sites would mean a ledger of dozens of accepted entries nobody reads, which
is how a guard becomes decoration.

## 6. What this cannot see

Stated plainly, because a document that lists only what it covers reads as
complete:

- **Algorithmic complexity.** An O(n²) join over stored items is a real
  regression and is invisible here.
- **Query and storage cost.** Nothing inspects SQLite access patterns, N+1
  reads, or missing indices.
- **Anything needing a baseline.** Wall time, allocation counts, p99 latency —
  §1's gap is unclosed and this checker does not pretend otherwise.
- **The cost of a bounded read.** A 320 MiB body is now refused rather than
  unbounded; it is still 320 MiB of legitimate allocation if a caller sends it.

Closing §1 properly means benchmarks and a comparison harness. This document is
the smaller, honest half: the regressions that are decidable from source, decided.

## 7. Guards

| Guard | Where | Holds |
| --- | --- | --- |
| `scripts/check_perf_regressions.py --strict` | `make check` + the `witnesses` CI job | all four kinds, ledger both directions |
| `python/tests/test_check_perf_regressions.py` | pytest | 26 tests, each driving a violation the checker must catch |
| `internal/server/maxbody_test.go` | `go test` | 413 fires, traffic under the bound is untouched, `0` means unlimited, ordering holds, the data plane is included |
| `internal/config/config_test.go` | `go test` | unset / `0` / explicit / garbage read three different ways |
| `internal/httpx/guard_test.go` | `go test` | the pre-existing truncation ban, unchanged |

The checker was proven non-vacuous by driving it against a real violation of each
of its four kinds and against a stale ledger entry, confirming all five failure
directions fire before reverting. Writing its test module found **a real defect in
the checker itself**: the ceiling regex anchored at `^\s*<name>`, which matches a
name indented inside a `const (...)` block and *not* the standalone
`const DefaultMaxRequestBody =` at column zero — so every inner ceiling was read,
the outer one was not, and the ordering check reported the bound as *absent* on a
correct tree. It was found by running the checker against the tree it ships with,
which is the only way it would have been found at all.
