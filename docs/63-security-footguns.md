# 63 — Security foot-guns: what scanned this source, what did not, and what now guards it

**Status: nothing in this repository read its own source for insecure code
shapes. The scanners that run here all answer a different question —
govulncheck and osv-scanner watch dependencies, gitleaks watches committed
strings, and Dependabot watches versions. `.golangci.yml` enables no gosec and
`pyproject.toml` does not select ruff's flake8-bandit family. The category was
absent rather than incomplete, which is why
[SECURITY.md](../SECURITY.md)'s honest "what does not run" section could not
name it. `scripts/check_security_footguns.py` now holds it, and the first run
found one real defect. A later change taught R1 the stdlib `ssl` dialect it had
been blind to and found six more of the same class — plus two unchecked archive
extractions, under a new rule. All nine were fixed rather than recorded.**

**The headline: `internal/akv` disabled TLS certificate verification for its
whole transport, from a flag `docker-compose.yml` sets to `"true"` and
[04-configuration.md](04-configuration.md) documents as being for
entra-emulator's self-signed certificate — on the one client in this tree whose
allowlist deliberately admits a real `*.vault.azure.net`.**

This is the fifth document on this architecture, after
[60-test-flakiness.md](60-test-flakiness.md)'s three language surfaces and
[62-performance-regressions.md](62-performance-regressions.md). It follows the
same shape: measure first, repair what is real, record what is accepted, and
gate the class rather than the instances.

## 1. What was already scanning, and what it could not see

| Scanner | Where | What it answers | Reads this source? |
| --- | --- | --- | --- |
| govulncheck | `security.yml` | a vulnerable dependency symbol this code can reach | no — the module graph |
| gitleaks | `security.yml`, tree + full history | a secret string that was committed | no — string contents |
| Dependabot | `dependabot.yml`, five ecosystems | version currency plus the advisory graph | no — manifests |
| osv-scanner (`python-advisories`) | `security.yml`, 9 tracked `uv.lock` | a vulnerable locked *version* — **not** reachability-filtered | no — lockfiles |
| osv-scanner (`js-advisories`) | `security.yml`, root `pnpm-lock.yaml` | the same question for the pnpm workspace | no — lockfiles |
| `check_dismissed_advisories.py` | `security.yml` | a dismissal whose justification expired | no — alert state |
| `check_dependency_risk.py` | `make check` + `ci.yml` | is every manifest watched, is every ecosystem *scanned*, and does any manifest hold a dependency back silently — an `==` pin or a root-diverging range with no declared exit | no — config and manifests vs tree |
| golangci-lint | `ci.yml` | errcheck, govet, ineffassign, staticcheck, unused | yes, but **no gosec** |
| ruff | `ci.yml` | E, W, F, I, UP, B, SIM, RUF | yes, but **no `S` (flake8-bandit)** |

The two rightmost rows are the gap. Every scanner above them is pointed at
somebody else's code or at a string, and the two pointed at this source carry no
security analyser.

**The tree had already noticed.** Fifteen `# noqa: S###` directives sit in the
Python sources — `S102` beside an `exec`, `S501` beside a `verify=False` — marking
sites as security-relevant for a rule family `pyproject.toml` never selected. They
were suppressing a check nobody ran. `RUF100` is deliberately off (see the ruff
config's own comment), so they survived as documentation of intent with nothing
reading them.

## 2. Why the rule set is SECURITY.md's list and not a scanner's defaults

This is the constraint that shapes everything below. **fabric-emulator is
deliberately insecure in documented ways** — seeded identities with published
secrets, self-signed TLS by default, admin surfaces with no authentication so
tests can drive them. SECURITY.md says so and calls those *design*, explicitly
not findings.

So a general-purpose security scanner is the wrong tool here, not because it
would be wrong but because it would be *right and unhelpful*: it would report the
seeded client secret, the self-signed certificate and the open admin API on every
run, and output like that trains people to skim. This repository has already
written down that failure mode twice — ruff's config says "a rule that only
produces churn is worse than no rule, because the noise trains people to skim the
output", and the first version of this very checker reported **89 findings**
against **37** now, the difference being entirely near-misses.

Each rule therefore maps onto a bullet SECURITY.md already treats as in scope:

| Rule | SECURITY.md bullet |
| --- | --- |
| `tls-verification-off` | real credentials leaking; authorization logic wrong rather than absent |
| `credential-in-message` | "a real token … written to a log, an event, an error body" |
| `secret-compared-directly` | authorization logic that is wrong rather than absent |
| `weak-random-for-secrets` | real credentials leaking |
| `python-shell-injection` | escape from the emulator to the host |
| `world-writable-mode` | escape from the emulator to the host |
| `archive-extraction-unchecked` | escape from the emulator to the host |

## 3. The rules, and what each deliberately does *not* flag

The exclusions are the substance. Each one below was a real false positive on
this tree, and each is pinned by a test in
`python/tests/test_check_security_footguns.py` that fails if it is lost.

### `tls-verification-off` — ledgered, not banned

`InsecureSkipVerify: true`, `verify=False`, `connection_verify=False`, and the
stdlib `ssl` dialect — `verify_mode = ssl.CERT_NONE`, `check_hostname = False`,
`ssl._create_unverified_context()`, and assignment to
`ssl._create_default_https_context`. A local emulator talking to a sibling's
self-signed certificate is the normal case here, so every site is recorded
rather than forbidden — **and the entry must name the host set that client can
reach.** That field is the whole point: it is precisely what nobody had written
down when `internal/akv`'s skip reached an Azure domain. Forty-seven sites,
forty-nine findings.

**The `ssl` dialect was invisible for this rule's first two releases, and that
was the same defect class the rule had just been written to catch.** R1 claimed
to ledger every certificate-verification skip in this tree and matched only
requests' spelling. The stdlib's spelling is the pair above — and the pair is
what this repository's own Python actually writes, in the spark agent, in
`fabric-target`, in the Sail launcher, and across twenty-eight `e2e/`
harnesses. Teaching R1 the dialect took it from 14 findings to 53 across 39
newly visible sites. **Six of those carried a credential over the unverified
connection**, and they are §4's second finding.

The paired idiom reports **once**: `check_hostname = False` is suppressed when
the same enclosing symbol also sets `verify_mode = ssl.CERT_NONE`, because that
is one context and one trust decision. The suppression is keyed on the symbol
rather than the file, so a second function's lone skip in the same file is still
its own finding — the same reason the ledger key carries a symbol.

### `credential-in-message` — the format string is never scanned

Only the arguments *after* it. This one decision is what makes the rule usable in
this repository, where the dominant shape is:

```go
return fmt.Errorf("%w: signature", ErrBadToken)   // thirteen of these in internal/auth
return fmt.Errorf("unexpected token %q", t.text)  // the pipeline and DAX parsers
```

The word "token" is in the *message*; the argument is an error sentinel or a
parser lexeme. A scan that read format strings would report every one. On top of
that, `Err*`-prefixed and `*Type`/`*Kind`/`*Name`/`*ID`/`*URL`-suffixed arguments
are excluded (a discriminator is not a value — `creds.CredentialType` is what the
arguments-only reading hits), and **a call is judged by its function rather than
its arguments**, so `len(SECRETS)` is a count and
`get_token_seconds_remaining(t)` is a duration.

One finding, recorded: the terminal pane prints its own generated token to
stdout for the operator to copy, the way Jupyter does, and the comment above it
explains why an endpoint would be worse.

### `secret-compared-directly` — a regression guard at zero

`==`, `!=` or `strings.EqualFold` on a secret, signature, MAC or digest instead
of `hmac.Equal` / `subtle.ConstantTimeCompare`. Comparisons against a **literal**
are not flagged, because `if token == ""` is a presence check. Nor is a
credential compared against a **call**, which was this rule's only hit:

```go
token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
if token == "" || token == r.Header.Get("Authorization") {   // NOT a secret comparison
```

TrimPrefix having changed nothing means the `Bearer ` prefix was absent. The
shape the rule exists for survives both exclusions: `mac == expectedMAC` is
identifier-versus-identifier, and `sig != computeMAC(body)` has a
credential-named function on the other side.

### `weak-random-for-secrets` — judged on what the randomness becomes

`math/rand` or Python `random` **bound to** a credential: assigned to a
credential-named target, or returned from a credential-named function. The first
version asked whether the file *mentioned* a token, secret, key, nonce or
password, and reported four `rnd = random.Random(SEED)` calls in
`examples/contoso-fixtures*` — seeded generators producing the rows an example
asserts against, in files that say "key" because they describe a primary key.
Determinism is the point there and `secrets` would break it. Baseline zero.

### `python-shell-injection` — a built command needs a shell behind it

`shell=True`, `os.system`, `os.popen`, `yaml.load` without a safe Loader,
`eval`/`exec` over a non-literal, and a shell command assembled by f-string or
concatenation. That last one only counts **with a shell**:
`subprocess.run(cmd + ["up", "--build"])` is argv, not syntax, and twenty sites
across `e2e/` have exactly that shape.

Thirteen findings, all recorded, all `exec`/`eval`: the spark agent's statement
executor, the notebookutils UDF runner, and the harness and example engines that
run notebook cells. **Running submitted code is what those components are.**
SECURITY.md scopes the in-scope case as escape *beyond the documented execution
surface*, and a notebook cell reaching the interpreter is that surface — real
Fabric runs the cell too. They are recorded rather than excluded so the set of
places this repository executes arbitrary code stays enumerated, and a new `exec`
anywhere else fails the build.

### `world-writable-mode`

`0o777` / `0o666` reaching `os.chmod`, `os.WriteFile` or `os.MkdirAll`. Ten
findings in eight recorded sites, all the same documented workaround: the
published image is distroless **nonroot** (uid 65532) and a bind mount keeps the
host's ownership, so a directory the checkout user created is not writable by the
container. Go's coverage runtime says nothing about that — it writes nothing,
leaving an empty directory that reads as "the e2e exercised nothing" rather than
"the e2e could not say". Docker Desktop on macOS ignores the uid mismatch, so it
passes on a laptop and fails only on Linux CI. See `scripts/coverage_prepare.sh`
and `e2e/engine-matrix/run.py`'s `ensure_out_writable`.

### `archive-extraction-unchecked` — a guard at zero, in two different ways

An archive member's path is chosen by whoever built the archive, so extracting
one without checking where it lands writes outside the destination — zip-slip
and tar-slip, the host-escape class SECURITY.md scopes and R5/R6 already serve.
Python: `extractall` with no `filter=` and no `members=`,
`shutil.unpack_archive`, and `.extract(<non-literal>)` on an archive object.
Go: an archive header's name joined onto a destination with `filepath.Join` and
no containment check in the same function.

**`filepath.Join` is not the bound it looks like.** It *cleans* its result,
which is exactly what makes the shape read as safe:

```go
filepath.Join("/out", "../../etc/passwd")   // "/etc/passwd" — clean, and outside /out
```

Python's default is the same trap from the other direction:
`tarfile.extractall`'s `filter="data"` only became the default in **3.14**, and
this repository's floor is **3.12** — so on the interpreter CI actually runs,
the unsafe default is live and naming the filter is what makes the behaviour
match.

The two halves reached zero differently, which is worth stating rather than
letting "baseline zero" imply one story:

- The **Python** half had two real findings and they were **fixed, not
  recorded** — see §4.
- The **Go** half has never had a finding to report. No non-test Go file in
  this tree imports `archive/zip` or `archive/tar` at all, so it is a pure
  regression guard, there so the first Go-side extractor lands as a finding
  instead of as a review comment.

Two narrowings, both measured against real false positives here. `.extract`
requires an archive module in the file **and** a method call: the
vendored-activity-types parser has `v.extract(page(...))`, which scrapes an
HTML table, and `scripts/vendor_notebookutils_stubs.py` both imports `zipfile`
and defines its own `extract(url)` — so the import gate alone did not save it,
and that function reads members into a dict with `wheel.read(name)` and writes
no member-named path at all. The Go clause needs the archive import for the
same reason: `f.Name` is also how an `fs.DirEntry` is spelled, and those names
come from a filesystem this process is already reading.

## 4. The real findings, and their fixes

`internal/akv` is fabric-emulator's client to a Key Vault data plane. Its
allowlist (`checkVaultURI`) deliberately admits **real** Azure vault domains, and
requires `https` for them specifically — the comment says why: *"a token for
vault.azure.net does not go out over cleartext."* `ErrVaultNotAllowed`'s own
docstring notes this is not theoretical, because the project supports pointing
`--entra-issuer` at a real tenant, "in which case the leaked token is a real
Azure one".

The transport did not hold up that end:

```go
func New(insecure bool, client *http.Client, extraHost string) *Client {
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if insecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}   // every host
		}
```

`insecure` arrives as `cfg.EntraTLSInsecure` — `FABRIC_ENTRA_TLS_INSECURE`, which
`docker-compose.yml:126` sets to `"true"` and `04-configuration.md` documents as
"needed when entra-emulator serves its self-signed cert … Never needed against
real Entra". The same compose file sets `FABRIC_AKV_VAULT_HOST` at line 123. So on
the default local stack, an AKV-reference connection naming
`https://contoso.vault.azure.net` would send a workspace-identity bearer token,
and receive a secret, over a connection nobody authenticated. **The scheme check
was enforced while the trust decision behind it was not** — and requiring https
without verification buys nothing, since whoever terminates the TLS keeps both
the token and the secret.

`internal/server` compounded it by passing `jwksClient` into `akv.New`, so when a
client was supplied the vault path reused the transport built for the Entra JWKS
fetch and inherited a trust decision taken about a different host.

**The fix** scopes the skip to the one host it was justified for, with a small
`http.RoundTripper` that decides per request host:

```go
func (t *hostScopedTLS) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.EqualFold(req.URL.Host, t.skipHost) {
		return t.skip.RoundTrip(req)
	}
	return t.verify.RoundTrip(req)
}
```

An Azure vault domain now verifies whatever the flag says, which is what makes
that flag safe to leave set in a compose file that also resolves real AKV
references. With no `extraHost` configured there is nothing to exempt — every
reachable host is then an Azure one — so verification stays on. AKV builds its own
transport and `server.go` passes `nil`; the injected-client parameter remains for
in-process tests.

Two tests in `internal/akv/akv_test.go` cover it, and the first fails on the old
code. `TestTLSVerificationIsScopedToTheConfiguredVaultHost` serves **one**
untrusted certificate at **one** URL to **two** clients differing only in which
host was configured as the emulator vault — so it cannot pass for a reason to do
with the certificate or the URL, only because the skip is keyed on the host. It
exercises the transport `New` built rather than going through `ResolveSecret`,
because the allowlist would refuse a non-vault host before any TLS decision was
reached. `TestAnAzureVaultHostAlwaysVerifies` then names every domain in
`AzureVaultSuffixes` on the round-tripper directly, including the sovereign
clouds a single network case cannot reach, plus the near-misses: right host wrong
port, the host as a prefix of a longer one, and mixed case.

### The same defect in Python, six times, found by teaching R1 one new spelling

`internal/akv`'s defect was *a TLS skip applied to a client rather than to a
host*. The repair was `hostScopedTLS`. What the rule could not then say is that
the identical shape sat in this repository's Python six times over, because R1
matched `verify=False` and not the stdlib pair. Adding the dialect surfaced all
six at once:

| Site | What it sends | Where it can reach |
| --- | --- | --- |
| `eventstream_kafka._fabric_request` | a bearer token | **`FABRIC_API_URL`, defaulting to `api.fabric.microsoft.com`** |
| `eventstream_kafka._fabric_token` | `ENTRA_CLIENT_SECRET` | `ENTRA_TOKEN_URL` |
| `storage._mint` | `ENTRA_CLIENT_SECRET` | `ENTRA_TOKEN_URL` |
| `storage._forge_attributed` | a client id, returns a token | `ENTRA_FORGE_URL` |
| `usercontext.caller_token` | a principal, returns a token | the issuer's `/admin/api/tokens` |
| `fabric_target._EmulatorCredential.get_token` | a client secret | `ENTRA_EMULATOR_URL` / `ENTRA_URL` |

The first row is the sharp one: **the default configuration talked to a real
Microsoft endpoint with the trust check off.** The other five are
configuration-dependent, and four of them POST a client secret.

Three of these carried a comment justifying the skip, and the justification was
the interesting part. `storage._mint` said the issuer "is named by our own
config, not by anything user-supplied" — and *config is exactly what can name a
real endpoint*. The comment described the compose case correctly; the code did
not restrict itself to the compose case, which is the same gap in reasoning
`akv`'s flag comment had.

**The fix** is `akv.hostScopedTLS` one language over:
`python/spark_agent/tls_scope.py` for the agent's five, and a module-local
`_tls_context` in `fabric-target` (a separate distributable, so it cannot
import the agent's). Both return an unverified context only for a host that
**parses as** a loopback, link-local or private address, or is `.localhost`,
`.internal`, `host.docker.internal`, or a **bare compose service name** — and
`None` otherwise, which `urllib.request.urlopen` reads as the interpreter's
fully-verifying default. `None` rather than a hand-built strict context,
deliberately: a second definition of "the default" is a second thing to drift.

The bare-name rule is the load-bearing one. Compose resolves service names with
no dot in them (`entra-emulator`, `fabric-emulator`), which is how every URL in
this repository's own compose files is spelled — and a name with no dot cannot
be a public DNS name, so treating it as local is a fact rather than a guess.
`.localhost` is reserved to loopback by RFC 6761 and `.internal` to private
use, so neither can resolve to a public endpoint either.

Both helpers still match R1, because the checker sees the two assignments and
cannot see the host guard above them. They are therefore **in** the ledger —
with `hosts` naming what the guard actually admits, and a `why` that says it
records a fix rather than an allowance.

### The guard's own fail-open, and why it was a *table* that found it

The first version of both helpers decided on the **name**, and ended:

```python
if host.startswith("127."):       # a PREFIX test against a name
    return True
return "." not in host            # no dot, so it cannot be public
```

Both lines are wrong in the permissive direction, and both were reachable from
every credential-carrying call site above, whose URL is configuration:

- **No IPv6 address contains a dot**, so `"." not in host` read *every* IPv6
  literal as local. `tls_context("https://[2606:4700:4700::1111]/token")`
  returned an unverified context, and so did the Microsoft-owned
  `2620:1ec:bdf::1` — which contradicts the `hosts` field those entries are
  required to carry, since an IPv6 literal is not dotted and was not verified.
- `startswith("127.")` matches a *name*, so the attacker-chosen public name
  `127.0.0.1.evil.com` got one too.

The repair decides address literals on the **parsed address**
(`ipaddress.ip_address`, local only for `is_loopback` / `is_link_local` /
`is_private`), falls through to the name rules otherwise, and fails **closed**
on a colon-bearing host that will not parse — a malformed address literal is
not a dotless DNS name. Private and link-local addresses stay local because a
compose network *is* those: the docker bridge `172.17.0.2`, an IPv6 ULA
`fc00::1`, a scoped `fe80::1%eth0`.

Two things are worth stating about *how* this was found, because they are the
transferable part:

1. **Nothing asserted this boundary at all.** When the six skips were repaired,
   no test in `python/tests/` or `python/fabric-target/tests/` named
   `tls_scope` or `is_local_host`, and `check_script_test_coverage.py` scopes
   itself to `scripts/`, so no gate could notice. That is this repository's own
   thesis — a guard whose behaviour nothing asserts can stop guarding and go on
   reporting green — applied to the module that decides whether to authenticate
   a peer holding a client secret. It is now pinned by a host/expectation table
   (`python/tests/test_spark_agent_tls_scope.py`): 21 hosts that must be
   local and 14 that must verify, each run through the predicate *and* through
   `tls_context` itself — because a `tls_context` that stopped consulting the
   host rule would satisfy every predicate assertion and still hand an
   unverified context to everything. Six of the must-verify rows fail on the
   pre-repair code.
2. **The two copies are a deliberate mirror, and the bug was in both,
   verbatim.** `fabric-target` ships as its own distributable and cannot import
   the agent's helper, so duplication is the price of that packaging
   boundary — the same arrangement as `akv.hostScopedTLS` and `tls_scope`, one
   language apart. Both docstrings now say so, and the table runs over **both**
   implementations plus an agreement test, so a one-sided correction fails the
   build instead of leaving one credential boundary behind.

One smaller decision made deliberate rather than incidental: `urlsplit`
defers host validation to `.hostname`, which raises `ValueError` on a
malformed bracketed literal. Uncaught, that aborted a token mint with an
exception thrown from inside a TLS helper — fail-closed in that nothing was
transmitted, and a path nobody chose. Both helpers catch it and take the
verifying branch, which is what the test asserts; a test that merely
*tolerated* the `ValueError` would be pinning an accident.

### Two unchecked archive extractions, on a network download

`e2e/azcopy/run.py` downloads the real `azcopy` from an `aka.ms` redirect and
unpacked it with bare `extractall` — both branches:

```python
if zipfile.is_zipfile(arch_path):
    with zipfile.ZipFile(arch_path) as z:
        z.extractall(extract)          # every member, wherever its name points
else:
    with tarfile.open(arch_path) as t:
        t.extractall(extract)
```

A member named `../../x` writes outside `WORK`, which on a CI runner is the
checkout and on a laptop is the home directory. **Fixed rather than recorded:**
`filter="data"` on the tar branch (tarfile's own containment filter, available
on the 3.12 floor), and on the zip branch — which has no `filter=` — an
explicit check of every member name before extraction, with the checked list
handed back as `members=` so the extraction is over a list the function built.

The check is on the **name**, and it covers three ways out that
`extractall` would otherwise honour: an absolute path, a Windows drive letter
(`C:\…`, which `os.path.join` treats as absolute on that platform), and any
`..` **component** — a component rather than a substring, so a member
legitimately called `..azcopy` is not refused.

## 5. How to record an accepted site

Add an entry to [`security-footguns.json`](security-footguns.json):

```json
{
  "file": "internal/thing/thing.go",
  "rule": "tls-verification-off",
  "symbol": "New",
  "hosts": ["cfg.ThingURL — thing-emulator, a compose sibling"],
  "why": "Self-signed cert on the compose network; sends no credential."
}
```

- **`file`, `rule`, `symbol`** are the key. Keyed on the enclosing symbol rather
  than a line number, because a line number goes stale on any edit above it, and a
  ledger that must be renumbered to stay valid is one people delete entries from.
  The **rule** is in the key so an accepted TLS skip does not also exempt a
  credential leak added to that function later.
- **`why`** is required. An accepted site with no reason is indistinguishable from
  one nobody looked at.
- **`hosts`** is required for `tls-verification-off`, and is the field worth
  reading: a skip is only reviewable against the set of hosts that client can
  reach. Naming them is what distinguishes a compose sibling's certificate from a
  real Azure vault.

**The ledger is checked in both directions.** An entry whose site has gone away
or been fixed fails just as loudly as an unrecorded finding — a stale allowance
goes on excusing the file and would silently re-cover the shape if it came back,
which is the direction a "what's new" reader would never think to check. It is
the same rule `docs/script-test-coverage.json` follows.

## 6. What this still does not do

Stated rather than implied, in the manner of SECURITY.md's own "what does not
run":

- **It is a source-shape checker, not a taint analysis.** It sees that a
  credential-named identifier reaches a log; it cannot follow a value through
  three assignments and a struct field into one.
- **Go rules are regex and brace-scanning, not a parser.** The Python side uses
  `ast`; the Go side cannot, for the same reason every sibling checker here gives
  — these scripts are stdlib-only and take no third-party parser. Reasoning about
  a symbol's enclosing function is a straight-line reading of the file.
- **Two rules have no Go counterpart, and one has no Python finding.** Stated
  because "six rules, two languages" would otherwise read as twelve halves that
  all run. `python-shell-injection` is Python-only *by measurement, not by
  choice*: no non-test Go file in this tree imports `os/exec` at all, so a Go
  shell rule would be a rule with nothing to read, and adding one would put a
  speculative zero in the rule set where an honest absence belongs.
  `archive-extraction-unchecked` has both halves implemented, but its **Go half
  has never had a finding** — nothing here imports `archive/zip` or
  `archive/tar` — so it is a guard against a shape this repository does not yet
  have, not a measurement of one it does.
- **The `ssl` dialect is the gap this rule set already had once, and it will
  have others.** R1 shipped claiming to cover every certificate-verification
  skip in this tree while matching one library's spelling of it, and 39 sites
  — six of them carrying credentials — sat unexamined behind the stdlib's
  spelling for two releases. The lesson generalises past TLS: each rule covers
  the *spellings someone thought of*, and a rule's confident scope sentence is
  the thing most worth distrusting. `connection_verify` arrived the same way,
  from the Azure SDK.
- **It does not read TypeScript.** The portal is out of scope, as it is for the
  `check_*_test_flakiness.py` trio's AST-based members.
- **There is still no licence check.** That gap is SECURITY.md's and is
  unchanged by this document. The SBOM half of it closed: `make sbom` emits a
  CycloneDX inventory of the locked dependencies, without licence fields.
- **Only Go gets reachability filtering.** Python and npm are scanned against
  the advisory graph now (the two osv-scanner rows above), which they were not
  when this document was written, but version-level: a finding means the locked
  version is affected, not that anything here calls the affected symbol. The
  gap narrowed from "no advisory scan at all" to "no reachability analysis",
  and those are not the same claim. `docs/advisory-holds.json` records the
  advisories this repository knowingly carries, each with the upstream change
  that retires it.
