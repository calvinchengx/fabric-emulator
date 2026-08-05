# 10 — Testing with the emulator

The emulator's reason to exist is **determinism real Fabric cannot offer**.
Two levers do the work, both on the `/_emulator` control surface (control-plane
origin, unauthenticated, not part of the Fabric contract).

## The clock — LROs on demand

Every async operation gets a `completeAt` on a **virtual clock**. Nothing
sleeps; nothing is flaky.

```bash
GET  /_emulator/clock                 # { offset, frozen, now }
POST /_emulator/clock                 # any of:
     { "freeze": true }               #   stop time
     { "advance": 3600 }              #   jump forward (seconds)
     { "offset": 0, "freeze": false } #   reset / resume
```

The pattern for testing a polling loop:

```text
POST /_emulator/clock {"freeze": true}
start the emulator with -lro-delay 600      # operations stay Running 600 virtual seconds
POST /v1/workspaces/{id}/items {…}          # → 202, poll → Running forever
POST /_emulator/clock {"advance": 601}      # time passes instantly
poll again                                  # → Succeeded
```

With the default `-lro-delay 0`, operations complete on the next poll — fast
CI without giving up the `202` contract.

## Fault injection — the unhappy paths

```bash
POST /_emulator/faults
     { "failNextOperations": 1 }   # next N async operations end Failed (Fabric-shaped error body)
     { "rejectNextRequests": 2 }   # next N API requests get a 5xx
     { "lroDelaySeconds": 30 }     # override the delay at runtime
```

This is how retry logic, poll-until-failed branches, and error surfaces get
tested without patching the client under test.

## `/health`

`GET /health` → `{ "status": "ok", "now": … }` — what the Docker
`HEALTHCHECK` (the `healthcheck` subcommand) and compose `depends_on` gates
use.

## Testing your own code against the emulator

- **In-process (Go):** `server.New(cfg, …)` + `httptest` — the emulator's own
  integration tests run this way, including with a real in-process
  entra-emulator minting tokens. No network, no fixtures.
- **Over HTTP (any language):** start the family with docker-compose, mint
  seeded tokens ([quickstart](01-quickstart.md)), drive the API. `-data-dir`
  empty means each run starts clean.
- **Real tools unmodified:** see
  [testing with fabric-cicd](11-testing-with-fabric-cicd.md).

## How the emulator tests itself

Every package covers itself (90% floor cross-package, currently ~90%), on
Linux, macOS, and Windows. The full matrix of what CI verifies — including
the real-tool e2e — is in [12-e2e-matrix.md](12-e2e-matrix.md).

## The failure this codebase keeps producing

Seven times in one day, across two people working in parallel, a bug took the
same shape: **something was missing or invented, and every available check
reported success.** They are collected here because each was expensive, each
looked different at the time, and the pattern is what makes the next one cheap.

Two directions. The first is an absence reported as a presence:

| what was absent | what every check said |
|---|---|
| a `DESCRIBE` returning zero rows for a table that has columns | correct schema, no error — dbt read "this table has no columns" |
| a `USING delta` clause dbt never emitted, because `delta` is the one value its macro omits | the config was demonstrably applied; that same value suppressed the clause |
| the `np` protocol, unregistered, so a pipe DSN parsed as TCP | `msdsn.Parse` returned **no error** and a usable-looking config |
| the lakehouse tables a reflection never loaded | reflection reported success; the fingerprint said "already done" |
| an `entra` process that never bound its port | the health check passed — against a different service on that port |
| a `keep-alive` scoped to a step's shell, dead before it was needed | the step ran green |
| lineage edges a whole warehouse build never recorded | a clean run, no error, an empty graph |

The second direction is worse and rarer: **a fabrication reported as
evidence.** A lineage endpoint that paired reads against writes as a cross
product recorded six well-formed bronze-to-silver edges where three of the
movements never happened. Nothing was missing, the count went *up*, and the
result looked more complete than the truth. No schema check can catch that;
only comparing against what actually moved.

Two more, added after the first seven, because their mechanism is different
enough to be worth separating.

**Eight — the check that never ran, with silence where the signal would be.**
A README was added to one half of a pair that a check, landed forty minutes
earlier, required to differ only in how silver is built. Every path the README
referenced existed; the author verified the contents and not the context. A
local run was silent because that check lived only in CI, so there was nothing
to distinguish "no invariant applies here" from "the invariant lives somewhere
I did not look". The other seven are a check running and reporting wrongly;
this is a check not running at all.

That is why `make check` exists and why `make test` depends on it. Both repo
invariants — `scripts/check_witnesses.py` and `scripts/check_example_parity.py`
— are stdlib-only and run in under a second, so there was never a reason for
them to be reachable only from a workflow file. **A check that exists only in
CI will keep catching people after the fact.**

**Nine — a measurement at the wrong granularity, reported as the answer.**
Asked whether CI was green, one person queried the RUN and saw `queued`;
another queried the JOB inside it and saw `success`. Both were reading the API
correctly. The run was `queued` because one of its 55 jobs had not started; the
job in question had run for seven seconds and passed. Two true measurements,
in conflict, each reported as "the CI result" — and nothing in either output
says which question it answers.

This is nastier than evidence that cannot discriminate, because the evidence
discriminates perfectly; it just answers a question adjacent to the one asked.
The same shape recurred twice more within the hour: `git log --format='%an'`
used to attribute a commit, in a repo where every session commits under one
author, so the signal had no discriminating power at all; and a parity check
"verified" by moving a file on disk, when the check reads `git ls-files` and
had not noticed. **Before believing a measurement, say out loud which question
it answers, and check that it is the one you asked.**

**Ten — a comment asserting a contract the code does not hold.** Above the
`Status` field on the event bus stood: *"Field names match the job-instance wire
shape, so a consumer reading the stream and a consumer polling the API see the
same words."* The names do match. The VALUES do not: the bus publishes
`Started`, which `StatusAt` never returns — polling the same job says
`InProgress`. A consumer written by reading that comment classified every job
start as an unknown status, and its report was wall-to-wall false positives with
real failures buried inside them.

The code was right; an event log and a state query legitimately differ. The
prose was wrong, and prose is what a downstream author reads. **A comment that
states an invariant is a claim, and an unenforced claim decays into a lie
without anything going red.**

Its twin is a DEFAULT that converts an omission into an assertion.
`CreateLineageEdge` fills an empty producer with `Copy` — and `Copy` means *the
emulator watched the bytes move*. Five of seven call sites named their producer;
two relied on the default. A new caller that simply forgot would publish an edge
claiming evidence nobody ever had, into the one structure whose entire purpose
is telling evidence from claim. Both copy sites now say `Copy` out loud, and
`TestEveryLineageEdgeStatesItsProducer` keeps the default a backstop rather than
a mechanism.

The tell for both: **ask what would go red if the sentence were false.** For the
comment, nothing did — until a consumer believed it. For the default, nothing
did — because being wrong and being defaulted are indistinguishable downstream.

**Eleven: a BOUND that truncates instead of refusing.**
`io.ReadAll(io.LimitReader(body, max))` is the obvious way to cap a read, and on
a write path it is data corruption. LimitReader reports clean EOF at the
ceiling: the excess is discarded, `err` is nil, and the handler cannot tell a
body that fitted from one that was cut. It stores the fragment and answers
success.

Nine sites had it. The one that mattered was OneLake's append, and it took
Microsoft's `fab cp` to reach it — every client this project drives chunks its
uploads, so none had ever crossed the ceiling. `fab` sends a whole file in one
append, and a 71 MiB upload was stored as 64 MiB with a 202. It surfaced ONLY
because `fab` then flushed at the real length and a position check disagreed. A
client that never flushed would have had a short file and no signal.

Several sites looked safe because they parse the body afterwards, so a truncated
JSON document 400s. That is luck wearing the costume of a design: the caller is
told their input is malformed when what happened is that it was too big, and the
first site that stops parsing inherits the silent version.

The tell is the same as ever — **ask what would go red if the sentence were
false.** "This read is bounded" was true. "This read is *safely* bounded" was
not, and nothing anywhere could tell the difference.

What closed it was not nine fixes. It was `internal/httpx`, one `ReadBounded`
that reads `max+1` so "too big" is detectable, plus a test that walks the source
and fails on the banned idiom — with an explicit `bounded-read-exempt:` marker
for the two places that genuinely discard what they read, and a second test
pinning the exemption list so the escape hatch cannot quietly widen. Fixing nine
sites is a day's work that lasts until someone writes the tenth.

### What actually caught them

Not review, and not more assertions. In every case it was **looking at what the
thing produced** rather than at whether it ran without complaint:

- reading dbt's compiled SQL instead of the Jinja that generated it;
- listing the edges instead of trusting the edge count;
- printing the parsed DSN (`Host "np:"`, `Protocols [tcp]`, `err = nil`);
- `docker ps`, which settled a day-long misdiagnosis in one line;
- a `log.Printf` compiled into the binary, which settled whether a fix was
  running at all after `docker compose ps` and `docker inspect` had both
  reported the tag and image id expected — for three runs against a stale
  image. **Verify the running code, not the label on it.**

### Three habits that follow

**A test that cannot fail is worse than no test**, because it certifies the area
as covered. A parse-only test sat green next to a dial that could not connect;
the handler tests injected path values and would have passed with every route
mis-registered. Check a new test fails when you break the thing it guards —
several here did not, and the guard for the port collision above passed its own
review while sailing straight past the collision it was written for.

**Prefer a loud failure to a plausible answer.** Endpoints in this repo refuse
rather than return an empty list where empty is indistinguishable from missing
(`GET /v1.0/myorg/datasets` for a personal workspace), refuse a refresh that
would re-read nothing, and refuse `modifiedSince` rather than silently doing a
full pass. Where empty IS the truth — a model with no datasources — it is
returned, and the difference is the point.

**A skip is only honest while the assertion still runs somewhere.** The
`medallion-compare` job exists because `compare.py` skips when its counterpart
is absent; delete the job and the comparison stops being made with nothing going
red. Both carry a comment saying to remove them together or not at all.

## Coverage: what the number covers, and what it cannot

Three measurements, because one would misrepresent the others:

| | What it measures | Gate |
|---|---|---|
| Go | unit + in-process server tests, merged with instrumented e2e runs | 90% floor, armed only where a real SQL Server was reachable |
| Python | the unit suite's own scope (checkers, delta-ops, storage) | 70% floor |
| Witnesses | every supported parity claim names a test that exists | `check_witnesses.py --strict` |

The witness count is the integration measure. "Every claim of support is backed
by something that ran" is a statement no percentage can make, which is why it is
published beside the percentages rather than folded into them.

### Instrumented e2e runs

An e2e suite can contribute real coverage, because the emulator can be built
with Go's `-cover` and the counters merged with the unit ones:

```bash
scripts/coverage_prepare.sh          # chmod 777, or the container writes nothing
FABRIC_COVERAGE=1 uv run --frozen --no-sync python e2e/sail/run.py
go test -cover -coverpkg=./... ./... -args -test.gocoverdir=$PWD/covdata/unit
scripts/coverage_merge.sh
```

The Python floor is passed **explicitly**, not through `addopts`:

```bash
uv run --frozen --group test pytest python/tests -q \
  --cov --cov-report=term-missing --cov-fail-under=70
```

`addopts` would apply to every pytest in the repo, including the ones e2e
harnesses run inside containers that have pytest but not pytest-cov — where the
flags are unrecognised arguments and the suite dies with exit 4 for a reason
that has nothing to do with what it was testing.

`FABRIC_COVERAGE=1` makes the harness layer `e2e/docker-compose.coverage.yml`,
which builds the emulator with `COVER=1` and mounts one repository-level
`covdata/`. Every wired suite writes into that same directory, so the merge sees
the whole fleet without needing a list of which suites ran.

**Two things must both hold, and neither is obvious:**

1. **The binary must be built instrumented.** `COVER=1` does that; the published
   image is never built this way.
2. **`covdata/` must be writable by uid 65532.** The image is distroless
   nonroot and a bind mount keeps the HOST's ownership, so a directory created
   by the checkout user is not writable by the container — and Go's coverage
   runtime does not complain, it just writes nothing. Docker Desktop on macOS
   ignores the uid mismatch entirely, so this passes on a laptop and fails only
   on Linux CI. `scripts/coverage_prepare.sh` does the chmod;
   `e2e/engine-matrix` hit the identical trap first.
3. **The process must exit CLEANLY.** A `-cover` binary writes its counters when
   `main` returns, and a SIGKILLed one writes *nothing* — an empty `covdata/`
   reads as "the e2e exercised nothing" rather than "nobody asked it to say".
   `cmd/fabric-emulator` handles SIGTERM for exactly this reason, and a stop it
   was asked for returns `nil` rather than the closed-listener error that would
   send `main` through `log.Fatal` and `os.Exit` — which skips the write anyway.

Measured contribution: `cmd/fabric-emulator` goes from **0%** on `signalStop` to
100% once an instrumented stack has been started and stopped, and the package
reaches ~70% from e2e alone. The merged total moves less than the per-package
figures do, because the unit suites already cover most paths — the e2e legs
prove the *wiring* (compose, TLS, the Entra handshake, the engines) that unit
tests deliberately stub.

**Wiring a suite that is not yet instrumented** is one conditional in its
`run.py`: append `-f e2e/docker-compose.coverage.yml` when `FABRIC_COVERAGE` is
set. A suite whose stack contains no `fabric-emulator` service has nothing to
instrument and is deliberately left alone.

## JavaScript: always through `pnpm`

Every JS entry point — installs, scripts, CI — goes through **pnpm**, never npm
or yarn:

```bash
pnpm install --frozen-lockfile
pnpm --filter fabric-emulator-portal test
pnpm --filter fabric-emulator-docs build
```

The workspace pins `packageManager: pnpm@10.29.3` and every `package.json`
carries `preinstall: npx only-allow pnpm`. **Every one, not just the root** —
the root guard does nothing for `cd portal && npm install`, which reads that
directory's own manifest, finds no guard, resolves its own tree, and writes a
`package-lock.json`. Two lockfiles is worse than the wrong one: each tool reads
its own, both report success, and the divergence surfaces later as a version
that is somehow different in CI with nothing in the diff to explain it.

`npx only-allow pnpm`, and that `npx` is not an oversight to tidy into
`pnpm dlx`: the script has to run under the package manager being refused.
Someone typing `npm install` has npx and may not have pnpm at all.

Enforced by three guards in `internal/repo`, because a convention with nothing
checking it is the subject of most of this document: every manifest carries the
guard, no rival lockfile is committed, and no workflow or script shells out to
`npm`/`yarn`. The last one has a leading word boundary — `npm install` is a
substring of `pnpm install`, and the first version failed on the two CI lines
that were already correct.

## Running Python: always through `uv`

Every Python entry point — e2e runners, `scripts/`, Makefile targets, CI steps —
goes through `uv`, never a bare `python3`:

```bash
uv run --frozen --group <group> python e2e/<suite>/run.py   # suite needs deps
uv run --frozen --no-sync python e2e/<suite>/run.py         # stdlib-only driver
```

Most host-side runners only drive `docker compose` and are stdlib-only, so they
take `--no-sync`; the nine that import real client libraries name their group.

This is not style. A bare `python3` resolves to whatever is on `PATH`, which on
a developer machine is often a pyenv build with none of the project's
dependencies — `e2e/adls-sdk` fails with `ModuleNotFoundError: No module named
'azure'` that way, a harness fault that reads exactly like a test failure.

`.python-version` pins **3.12**, matching `requires-python`, the `python:3.12-slim`
images, and CI's `setup-python`. Without it `uv` satisfies `>=3.12` with the
newest interpreter it can find (3.13 at the time of writing), so the host would
quietly run a different Python from the containers under test. Note that
`uv run --no-sync` *reuses* a mismatched existing `.venv` with only a warning
rather than rebuilding it — if the warning appears, `rm -rf .venv` and re-sync.
