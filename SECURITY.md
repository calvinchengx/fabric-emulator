# Security

## Reporting a vulnerability

Report privately through GitHub, on this repository:
**[Security → Report a vulnerability](https://github.com/calvinchengx/fabric-emulator/security/advisories/new)**.

That opens a draft advisory visible only to you and the maintainer. Please do
not open a public issue for a security report, and please give the project a
chance to ship a fix before disclosing.

Include what you would want if you were fixing it:

- the component (control plane, OneLake, TDS/warehouse, Livy/Spark agent,
  portal, the Entra handshake);
- how to reproduce, ideally as a failing test or a `curl` against
  `docker compose up`;
- what an attacker gains, and from what starting position.

Expect an acknowledgement within a few days. This is a personal open-source
project, not a staffed security team, so please be patient with timelines.

## What this project is, and what that means for scope

**fabric-emulator is a local development tool that is deliberately insecure in
documented ways.** It ships seeded identities and publicly known secrets, uses
self-signed TLS by default, and exposes admin surfaces without authentication so
tests can drive them. It is meant to run on `localhost`. It is not a security
boundary and must never hold real data or face a network.

So the usual framing does not transfer. "The seeded client secret is public" or
"the admin API needs no token" are **design**, described in the docs, not
findings.

### In scope

Reports that matter here are ones where the emulator betrays the developer
running it, or teaches code a lesson that is wrong in production:

- **Escape from the emulator to the host.** Path traversal out of the OneLake
  store, command injection through a pipeline expression, notebook or T-SQL
  input reaching the host beyond the documented execution surface.
- **Real credentials leaking.** The paths that touch genuine cloud material are
  the sharp ones: the real-Fabric toggle
  ([docs/21-real-fabric-toggle.md](docs/21-real-fabric-toggle.md)) and real
  compute ([docs/14-real-compute.md](docs/14-real-compute.md)). A real token,
  connection string, or storage key written to a log, an event, an error body,
  or a committed fixture is in scope.
- **Authorization logic that is wrong rather than absent.** Where the emulator
  *does* enforce RBAC, workspace scoping, or token validation, a bypass is a
  finding, because consumers write and test authorization code against it. A
  cross-workspace read that should have been refused counts; the unauthenticated
  admin API does not.
- **Accepting what real Fabric rejects.** Being more permissive than the thing
  being emulated certifies code that will fail in production. Treated as a
  parity defect, and worth reporting as one.
- **Supply chain.** A compromised or typosquatted dependency, or anything in the
  release pipeline that could ship a binary we did not build.

### Not in scope

- Seeded users, secrets, keys, and certificates. They are published on purpose.
- Unauthenticated admin and management endpoints, by design for testing.
- Self-signed or locally trusted TLS, and the local CA the docs tell you to
  install.
- Anything that requires exposing the emulator to a hostile network. Do not do
  that; it is out of scope by construction.
- Denial of service against a single-tenant local process.
- Missing hardening headers, cookie flags, or rate limits on a localhost tool.

If you are unsure which side a report falls on, send it. A misfiled report costs
little; a silent one costs more.

## What scans the dependencies, and what does not

Stated rather than implied, because the gap between "we run scanners" and "this
manifest is scanned" is where the two failures below lived.

**What runs** (`.github/workflows/security.yml`, on every push and pull request
and on a weekly cron — a scanner nobody runs is a scanner that finds nothing):

- **gitleaks** over the working tree *and* the full history. A secret that was
  committed and then removed is still in the pack, still cloneable, and still
  leaked, so scanning only the tip would report clean on the case that matters
  most.
- **govulncheck** over the Go module, with reachability filtering: it reports a
  vulnerable symbol this code can actually call, not merely a vulnerable
  version in the graph.
- **`scripts/check_dismissed_advisories.py`**, which re-asks whether each
  dismissed alert's justification has expired. GitHub never re-raises a
  dismissal when upstream ships a fix.
- **Dependabot** across five ecosystems — `gomod`, `npm`, `uv`, `docker`,
  `github-actions` — which is *version currency plus GitHub's advisory graph*,
  not reachability analysis.

**What does not run**, named here rather than left to be assumed:

- **Python and npm dependencies get no reachability-filtered advisory scan.**
  Go has govulncheck; the other two have Dependabot alerts alone, which flag a
  vulnerable version whether or not anything here calls the affected code.
- **Nothing produces an SBOM, and nothing checks licences.** There is no
  inventory artifact for a downstream consumer to ingest.

**What keeps the Dependabot half honest.** A scanner configuration that has
quietly stopped matching the repository reports clean on precisely the
manifests nobody is watching — worse than no scanner, because it produces a
green check. That has happened here twice, and both times it surfaced sideways
rather than by anyone looking: seven example `uv.lock` files watched by
nothing while their pins drifted, and eleven of twelve Dockerfiles unwatched
including two published to GHCR and pulled family-wide. Both were found through
a stale alert naming a directory deleted months earlier.

So `scripts/check_dependency_risk.py` runs offline in `make check` and in CI,
and enforces three things: every tracked manifest is covered by some Dependabot
entry (with deliberate exclusions written out as data with their reason, never
as silence), every watched directory still exists and still holds a manifest,
and every `ignore:` hold declares its exit condition — either the upstream
change that retires it, or an explicit statement that it is policy rather than
delay.

## Supported versions

Fixes land on `main` and ship in the next release. There are no long-lived
maintenance branches, so please confirm against `main` before reporting.
