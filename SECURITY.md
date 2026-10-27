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
Python shell and dynamic-execution misuse, world-writable file modes, unchecked
archive extraction, and SQL assembled by interpolating a name. Its first run
found one real defect —
`internal/akv` disabled certificate verification for its whole transport, on the
one client whose allowlist deliberately admits a real `*.vault.azure.net`.

The TLS rule then found the same defect class **six more times** once it was
taught the spelling it had been missing. It matched `requests`' `verify=False`
and not the standard library's `verify_mode = ssl.CERT_NONE` /
`check_hostname = False`, which is the pair this repository's own Python
actually writes — so 39 sites were unexamined, and six of them sent a client
secret or a bearer token over the unverified connection. One sent a bearer token
to a `FABRIC_API_URL` **defaulting to `api.fabric.microsoft.com`**, so the
default configuration reached a real Microsoft endpoint with the trust check
off. All six are now scoped per host, as `internal/akv` was. The same change
added the archive rule and fixed its two findings — an archive downloaded over
the network and unpacked without checking where its members land.

That is worth stating plainly rather than as a win: **a rule whose scope
sentence claimed to cover every verification skip in this tree covered one
library's spelling of it for two releases.** Each rule here covers the
spellings someone thought of, which is the limit most worth distrusting.

**And the guard that replaced those six skips shipped with a hole of the same
kind.** It decided whether a host was local from the *shape of the name* and
ended on "no dot, so it cannot be public" — but no IPv6 address contains a
dot, so every IPv6 literal was read as local and authenticated nothing, and
`127.0.0.1.evil.com` is a public name with a loopback-shaped prefix. Both
halves were reachable from the same configuration that carries the client
secret, and nothing in either test suite asserted the boundary at all. The
repair decides an address literal on the parsed address, and the behaviour is
now pinned by a host table over both copies of the helper. See
[docs/63-security-footguns.md](docs/63-security-footguns.md) §4.

**The newest rule found the same kind of asymmetry one layer down, and this
time the Go half was the one that was right.** A name spliced into SQL text is
SQL *syntax*, not a value — backticks around it look like quoting until the
name contains one. Measured: every Go site in this tree already escaped
correctly (`internal/tds.principalName` doubles `]`, `quotedID` doubles `'`,
`internal/server` has the same pair, and `enterDelegated` leans on SQL Server's
own `QUOTENAME`), while **not one backtick was doubled anywhere in
`python/spark_agent`** — including `onelake_security.apply`, which builds the
temp view that *enforces* OneLake row- and column-level security. A lakehouse
table is a folder under `Tables/` created by whoever can write to OneLake and
nothing rejects a backtick in a path segment, so a name that broke out of that
statement would choose the body of the view deciding which rows a principal
sees. Eight symbols were repaired onto one escape helper; the six correct Go
symbols are recorded as regression guards, because that half of the rule reads
the enclosing function and cannot see an escape in the argument list.

Three of the eight the rule could not see, and **two it still cannot** — one
returns SQL text instead of executing it, one binds its statement to a variable
first. Both were fixed anyway, since they are the column-only and the
table-withholding paths of the same enforcement. That is the honest shape of
this gate: it is a floor, and where the floor does not reach, a test table does.

**And the third of those three was the rule's own blind spot, in the package
its scope sentence named.** It required a *method* call, on the sound reasoning
that `execute` is a name anything may have — which does not reach a bare name
that **is** `spark.sql`, and `delta_ops` writes exactly that and passes it as a
parameter. The site hidden behind it spliced a catalog-supplied storage
location into a quoted SQL literal with nothing doubling an apostrophe, while
the same value was escaped one module over. The rule now recognises a bare call
to a name the file itself binds to `.sql`, so that repair fails the build if it
is removed. Three times now in this document a rule's confident scope sentence
has outrun its implementation, and each time a person re-reading the claim
against the tree is what found it.

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
- **`osv-scanner.toml`**, next to `go.mod`, lists the advisories that
  osv-scanner and OpenSSF Scorecard's version-level scan are told to ignore.
  Each entry is a reachability claim that govulncheck re-checks on every push.
  Today the only entry is GO-2026-5932, for `x/crypto/openpgp`: it affects every
  `x/crypto` version, and nothing here imports that package.
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
- **Nothing checks licences.** `make sbom` (`scripts/gen_sbom.py`) writes a
  deterministic CycloneDX 1.5 inventory of every locked Go, Python and npm
  dependency, but lockfiles carry no licence data, so the SBOM has none, and
  Dockerfile and GitHub Actions pins are not resolved sets and are not in it.

**What keeps the Dependabot half honest.** A scanner configuration that has
quietly stopped matching the repository reports clean on precisely the
manifests nobody is watching — worse than no scanner, because it produces a
green check. That has happened here twice, and both times it surfaced sideways
rather than by anyone looking: seven example `uv.lock` files watched by
nothing while their pins drifted, and eleven of twelve Dockerfiles unwatched
including two published to GHCR and pulled family-wide. Both were found through
a stale alert naming a directory deleted months earlier.

So `scripts/check_dependency_risk.py` runs offline in `make check` and in CI,
and enforces seven things. The first three are about Dependabot still matching
the tree: every tracked manifest is covered by some entry (with deliberate
exclusions written out as data with their reason, never as silence), every
watched directory still exists and still holds a manifest, and every `ignore:`
hold declares its exit condition — either the upstream change that retires it,
or an explicit statement that it is policy rather than delay.

The next two are about whether anything *scans* what Dependabot watches, which
is a different question with the same symptom when the answer is no: every
ecosystem with tracked manifests is reached by a named scanner job in
`security.yml` (or is recorded as deliberately on Dependabot alone, with the
reason), and every hold in `docs/advisory-holds.json` declares its exit
condition under the same rule.

**The last two are about the manifests themselves, and they close a gap the
first five could not see.** This repository can decide to live with a known
risk in four places, and until recently only three of them had any
bookkeeping: a Dependabot `ignore:` entry is checked by the third invariant, a
dismissed alert by `check_dismissed_advisories.py`, and an `advisory-holds.json`
record by the fifth. **The fourth is an `==` pin in a `pyproject.toml`, and it
is both the strongest hold available and the only one with none.** It is an
`ignore:` with no entry, no comment and no review: the package receives no
bump, security or otherwise, until a human edits the file, and not one of the
three checks above can see it. A grep for `LIFT THIS` or `PERMANENT HOLD`
across all eleven tracked `pyproject.toml` files returned **zero** against
eight exact pins. Seven turned out to be policy — the `pyspark-client` and
`pysail` versions are one number, a fidelity claim about which Spark the
shipped engine *is*, the same class as `apache/spark` in `dependabot.yml` — and
one, `dbt-databricks`, was not. Being made to write the marker down is what
separated them.

The seventh asks whether a constraint an example shares with the root agrees
with it, or records why not. That is a maintenance risk precisely **because
examples are what people copy out** — the same reason seven example `uv.lock`
files going unwatched mattered more than the root one would have.

**The honest gap that invariant found, and is now data rather than silence.**
The two advanced medallion examples pin `great-expectations>=0.18,<1` under a
comment that claimed the pin was there *"to match the emulator's own
great-expectations extra, so the two demonstrations cannot drift onto different
major versions of the API"* — while the root declares `>=0.18,<2` and its
lockfile resolves **1.23.2** against the examples' **0.18.22**. The comment
predicted the failure and was wrong about it being prevented. The divergence is
real and, for now, correct: `contract_gate.py` is written against the 0.18
fluent API that 1.0 removed, so raising the ceiling breaks both examples at
runtime rather than bumping a version. **The port of those gates to the 1.x
API is outstanding work, not a closed question**, and it is now recorded with a
`LIFT THIS` naming it rather than asserted away by a comment that had already
stopped being true.

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
