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

### What actually caught them

Not review, and not more assertions. In every case it was **looking at what the
thing produced** rather than at whether it ran without complaint:

- reading dbt's compiled SQL instead of the Jinja that generated it;
- listing the edges instead of trusting the edge count;
- printing the parsed DSN (`Host "np:"`, `Protocols [tcp]`, `err = nil`);
- `docker ps`, which settled a day-long misdiagnosis in one line.

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
