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
- **Over HTTP (any language):** start the pair with docker-compose, mint
  seeded tokens ([quickstart](01-quickstart.md)), drive the API. `-data-dir`
  empty means each run starts clean.
- **Real tools unmodified:** see
  [testing with fabric-cicd](11-testing-with-fabric-cicd.md).

## How the emulator tests itself

Every package covers itself (91%+ total, CI floor 90% cross-package), on
Linux, macOS, and Windows. The full matrix of what CI verifies — including
the real-tool e2e — is in [12-e2e-matrix.md](12-e2e-matrix.md).
