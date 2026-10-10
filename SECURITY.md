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

### Part of that list is now enforced mechanically

The in-scope list above is a judgement about what matters, and until recently
nothing checked this repository's own source against any of it. The dependency
scanners all look elsewhere: govulncheck and osv-scanner read the dependency
graph, gitleaks reads committed strings, Dependabot reads manifests. The two tools that
*do* read this source carry no security analyser — `.golangci.yml` enables no
gosec, and `pyproject.toml` does not select ruff's flake8-bandit family, though
fifteen `# noqa: S###` directives in the tree were written as though it did.

`scripts/check_security_footguns.py` now covers the subset of the list above
that is decidable from source text, offline, on every push and in `make check`:
TLS verification skips, non-cryptographic randomness for security material,
non-constant-time secret comparison, a credential value reaching a log or error,
Python shell and dynamic-execution misuse, and world-writable file modes. Its
first run found one real defect — `internal/akv` disabled certificate
verification for its whole transport, on the one client whose allowlist
deliberately admits a real `*.vault.azure.net`.

It is a floor, not a ceiling: a source-shape checker, not a taint analysis. The
sites where this emulator's documented local-by-design posture legitimately
produces one of those shapes are **recorded** in `docs/security-footguns.json`
with the host set each client can reach and the reason, rather than silently
excluded — so the distinction between "a compose sibling's self-signed
certificate" and "a real cloud endpoint" stays written down and reviewable.
[docs/63-security-footguns.md](docs/63-security-footguns.md) explains each rule,
what it deliberately does not flag, and how to record an accepted site. A report
about something it cannot see is still very much worth sending.

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
- **osv-scanner** over every tracked `uv.lock` — nine of them, 856 locked
  packages, discovered from `git ls-files` rather than a listed set of paths —
  and over the root `pnpm-lock.yaml`, which `pnpm-workspace.yaml` makes cover
  `portal` and `website` too. Two jobs, `python-advisories` and
  `js-advisories`, so a red one names which surface broke. **This is a
  version-level scan against the advisory graph, not reachability analysis:**
  a finding means the locked version is affected, not that this code calls the
  affected symbol. `scripts/check_advisories.py` runs it; the inputs are
  discovered rather than listed, and `check_dependency_risk.py` fails the
  build if a scan job ever grows a literal lockfile path, because a
  hand-maintained list is exactly what drifted before.
- **`scripts/check_dismissed_advisories.py`**, which re-asks whether each
  dismissed alert's justification has expired. GitHub never re-raises a
  dismissal when upstream ships a fix.
- **Dependabot** across five ecosystems — `gomod`, `npm`, `uv`, `docker`,
  `github-actions` — which is *version currency plus GitHub's advisory graph*,
  not reachability analysis.

**What does not run**, named here rather than left to be assumed:

- **Only Go gets reachability filtering.** Python and npm are now scanned
  against the advisory graph on every push (above), but version-level: a
  finding there flags a vulnerable version whether or not anything here calls
  the affected code, and conversely a vulnerable *symbol* nobody calls still
  fails those two jobs where it would not fail govulncheck. The gap narrowed
  from "no advisory scan at all" to "no reachability analysis"; it did not
  close, and the two are not the same claim.
- **`docker` and `github-actions` get no advisory scan**, deliberately and for
  a structural reason rather than a budget one: a base-image tag and a pinned
  action are not resolved dependency sets, so there is no locked version list
  to match against advisories. Dependabot's bumps are the whole of the
  coverage there. The decision, and the reason, are recorded in `SCANNERS` in
  `scripts/check_dependency_risk.py`, where the fourth invariant reads them.
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
and enforces five things. The first three are about Dependabot still matching
the tree: every tracked manifest is covered by some entry (with deliberate
exclusions written out as data with their reason, never as silence), every
watched directory still exists and still holds a manifest, and every `ignore:`
hold declares its exit condition — either the upstream change that retires it,
or an explicit statement that it is policy rather than delay.

The last two are about whether anything *scans* what Dependabot watches, which
is a different question with the same symptom when the answer is no: every
ecosystem with tracked manifests is reached by a named scanner job in
`security.yml` (or is recorded as deliberately on Dependabot alone, with the
reason), and every hold in `docs/advisory-holds.json` declares its exit
condition under the same rule.

**That ledger is the maintenance-risk half, and `cryptography` is why it is
shaped this way.** The hold above was written because mlflow's `requires_dist`
said `cryptography<50,>=43.0.0`, and its comment named the cost in advance:
*"cryptography is security-relevant and 49.x carries no open advisory today,
but that could change while the ceiling holds."* It changed — `PYSEC-2026-3552`
(CVSS 8.2) landed against 49.0.0 with its fix in exactly the 50.0.0 the ceiling
forbade — and nothing in this repository was asking whether the prediction had
come true until the advisory scan above started looking. mlflow 3.16.1 has
since raised its ceiling to `<51`, so the hold's stated `LIFT THIS` condition
was already met and the advisory was fixable after all. A hold with an exit
condition is only worth writing if something re-reads it.

## Supported versions

Fixes land on `main` and ship in the next release. There are no long-lived
maintenance branches, so please confirm against `main` before reporting.
