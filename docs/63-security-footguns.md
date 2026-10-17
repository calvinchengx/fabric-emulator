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
extractions, under a new rule. A third change added `sql-identifier-interpolated`
and found eight more, on the path that enforces OneLake row- and column-level
security — one of them invisible to that rule's own first cut. All seventeen
were fixed rather than recorded.**

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
| `sql-identifier-interpolated` | authorization logic wrong rather than absent; T-SQL input reaching the host |

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

### `sql-identifier-interpolated` — the escape is recognised, which is the point

SQL text assembled by interpolation rather than built from a literal with its
values bound. Python: a call to a SQL executor — a `.sql` / `.execute` /
`.executemany` / `.executescript` **method**, or a bare name *this file* binds
to one of those — whose first argument is an f-string, a `%`/`+` build or
`.format`, and whose author-written text starts on a DDL/DML verb. Go:
`fmt.Sprintf`, `fmt.Fprintf` or a concatenating `strings.Builder` write
carrying an upper-case SQL verb, inside a function that also calls
`Exec*`/`Query*`, in a file that imports `database/sql`.

The aliased-callee clause is the one part of this rule that **widens** rather
than narrows, and it is there because the method requirement had a hole with a
real finding behind it: `delta_ops` writes `original_sql = spark.sql` and
passes it as a parameter, so the splice that mattered most in that module was
invisible to the rule that claimed to have swept it (§4). It stays narrow — a
bare `execute(x)` with no such binding in the file is still ignored, so R7's
measured lesson about `execute` being a name anything may have is not
discarded, only scoped.

**A name spliced into SQL is syntax, not a value.** Backticks around it look
like quoting and are not:

```python
name = "x` AS SELECT * FROM secret --"
f"CREATE OR REPLACE TEMP VIEW `{name}` AS {policy}"
# CREATE OR REPLACE TEMP VIEW `x` AS SELECT * FROM secret --` AS SELECT …
```

This is not R5. R5's Python clause stops at `eval`/`exec`/`yaml.load` and a
command string with a shell behind it; a SQL string handed to an engine is a
different interpreter with a different fix, and R5 never looked at one.

**The rule recognises the escape, and that decision is load-bearing rather
than a convenience.** If an escaped site reported as loudly as a raw one, every
repaired site would need a ledger entry — and then deleting the `quote_ident`
call would leave that entry valid and the build green. That is a guard that has
stopped guarding while still reporting success, which is this document's own
thesis turned on the rule that was meant to enforce it. So an escaped
interpolation is not a finding, and removing an escape takes `--strict` to
exit 1. It *trusts* the named helper rather than re-deriving the escape,
exactly as the archive rule trusts `filter="data"` instead of reimplementing
tarfile; what makes that sound is that the helper's behaviour is pinned by a
name table driven through both the helper and the real call paths.

Four narrowings, each a measured false positive here:

- **The verb gate**, without which this is a rule about f-strings rather than
  about SQL — `.sql` and `.execute` are not rare method names. The verb must
  be text the *author* wrote, so interpolated parts are blanked before the
  test: `f"{verb} FROM t"` must not qualify on a word nobody typed, or the
  rule's scope would depend on runtime values.
- **Module-level constants are skipped.** This is what most e2e probes are —
  `e2e/sail-session-isolation/probe.py` is `s.sql(f"DROP VIEW IF EXISTS
  {VIEW}")` over a file-local constant. Without it a dozen harness probes
  crowd out the eight sites that decide who sees which rows.
- **The Go half needs a `database/sql` import**, because `.Query(` is also how
  `net/url`'s is spelled. `u.Query().Has("pbi_source")` in
  `internal/api/fabriciq.go` satisfied the "this function executes SQL" test
  on a URL parse. Same import gate the archive rule's `.extract` clause
  carries, for the same reason.
- **The Go verb search is case-sensitive**, because it *searches* rather than
  anchors and four of the verbs — `WITH`, `USE`, `SET`, `SHOW` — are ordinary
  English. With `re.I` it reported that same file's `"No Report or
  SemanticModel with id %q that you can access."` on the word "with". Every
  SQL statement this tree's Go assembles spells its keywords upper case.

Twenty findings, all recorded: the six correct Go symbols, nine e2e probes and
four example scripts over their own constants, and `onelake_security.apply`,
whose `name` is escaped and whose `sql` is spliced on purpose.


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

### The same defect class in the Python agent, eight times — and the Go side was already right

`internal/akv`'s defect was a TLS skip applied to a client rather than a host.
This one is the same *shape* of asymmetry in a different dimension: the rule
that found it, `sql-identifier-interpolated`, reports a shape rather than
banning one precisely **because the Go half of this tree was already correct at
every site and the Python half escaped nothing at all.**

| Where | Quote char | Escape |
| --- | --- | --- |
| `internal/tds.principalName` | `]` | `]` → `]]` ✅ |
| `internal/tds` — `EnsurePrincipal`'s `quotedID` local | `'` | `'` → `''` ✅ |
| `internal/server.sqlIdent` / `sqlLiteral` | `]` / `'` | both doubled ✅ |
| `internal/server.enterDelegated` | `]` | SQL Server's own `QUOTENAME` ✅ |
| `python/spark_agent/*` | `` ` `` | **nothing, anywhere** ❌ |

Worth being exact about the second row rather than letting the table imply
symmetry: `principalName` and `sqlIdent`/`sqlLiteral` are named helpers, but
the `'` escape in `internal/tds` is an inline `strings.ReplaceAll` repeated at
each site — a local called `quotedID` in `EnsurePrincipal`, and open-coded
again in `SyncOneLakeMemberships` and `SyncShortcutAccess`. Correct at every
one of them, and a shape where correctness has to be re-established by reading
rather than by a call. That is the argument for the Python repair being *one*
helper.

Not one backtick was doubled in `python/spark_agent`, and the sharp site is the
one that enforces authorization:

```python
# onelake_security.apply, before
spark.sql(f"CREATE OR REPLACE TEMP VIEW {name} AS {sql}")   # no quoting at all
```

That is the temp view holding a OneLake **row filter and column projection** —
the relation a narrowed principal reads instead of the real table. A name that
breaks out of that statement does not produce a syntax error; it chooses the
body of the view that decides which rows someone sees.

**And the name is reachable, which is what makes this a finding rather than a
tidiness note.** A lakehouse table is a *folder* under `Tables/` in OneLake,
created by whoever can write there, and nothing in `internal/onelake` rejects a
backtick in a path segment. The whole way round, the name is data chosen
outside this process:

> OneLake `Tables/<folder>` → `catalog.register` → `SHOW TABLES` →
> `onelake_security.apply` → the view that enforces the policy

**The fix** is one helper, `python/spark_agent/sqlident.py`, mirroring the Go
pair rather than half of it: `quote_ident` doubles the backtick and
`quote_literal` doubles the apostrophe, because `internal/server` has
`sqlIdent` **and** `sqlLiteral` and a repair that brought over only the
identifier half would leave `LOCATION '{loc}'` — same statements, same catalog
data — able to close its quote and append a clause. Both fail **closed** on a
name carrying NUL or a line break: neither can appear in a real identifier,
both can truncate or split a statement depending on who parses it, and
`apply`'s caller already treats an exception as "withhold the table".

Eight symbols were repaired — `catalog.register`,
`catalog._make_unqualified_resolve`, `onelake_security.schemas_holding`,
`.restore`, `._sweep_qualified`, `.secure_view_sql` (and `._drop`),
`usercontext.register`, and `delta_ops.execute_ctas`. **An ordinary name comes
back byte-identical** to the hand-written `` f"`{name}`" `` it replaces, which
is the only reason a change touching every statement the agent builds moved no
fixture, recording or e2e output.

**Three of those eight the rule could not see, and they were repaired anyway.
Two it still cannot.** `secure_view_sql` *returns* a string rather than calling
`.sql()`, and `_drop` binds its templates to a variable before
`spark.sql(stmt)` reads them — so neither is a finding (§6). They are the same
names on the same enforcement path: `secure_view_sql` is what a **column-only**
grant produces, and `_drop` is the **withholding** path that runs when a table
must not be readable. A repair that stopped where the checker's vision stops
would have left exactly those two as the unescaped routes.

#### The third one closed the rule's own blind spot, and it was the sharper finding

`delta_ops.execute_ctas` is reached as `original_sql(…)`. That is `spark.sql`,
bound to a name at `delta_ops.py:928` and passed into the function as a
parameter — so the callee at the splice is a bare `ast.Name`, and R8 required
an `ast.Attribute`:

```python
# delta_ops.execute_ctas, before
original_sql(f"CREATE TABLE IF NOT EXISTS {name} USING delta LOCATION '{loc}'")
```

The method requirement is measured and it is right — `execute` is a name
anything may have, which is R7's `.extract` lesson reused — but **that
reasoning does not reach a bare name that IS `spark.sql`.** So the rule's own
scope sentence claimed to have swept `python/spark_agent`, and this site sat in
it with nothing doubling an apostrophe.

The live route is `loc`, and it is the same value the repair had just escaped
one module over:

> `catalog.register`'s `sloc` → `catalog._remember_schema_location` →
> `delta_ops.remember_schema` → `_SCHEMA_LOCATIONS` →
> `known_schema_location(schema)` → `LOCATION '{loc}'`

`remember_schema` validates nothing; it strips a trailing slash. So after the
first cut of this change the identical catalog-supplied location was wrapped in
`quote_literal` by `catalog.register:219` and spliced raw into
`execute_ctas:512`, one module apart, on statements built from the same data —
which is verbatim the argument for `quote_literal` existing at all, turned
around on the change that added it. The two `_CTAS`-parsed routes cannot carry
the character (`target` has its backticks stripped, and the `LOCATION` capture
is `[^']+`), so the schema-location route is the whole of the exposure, and it
is the one route no regex bounds.

**`_py_sql_aliases` closes it**, narrowly: a bare call counts when *this file*
binds that name to a `.sql`/`.execute`/`.executemany`/`.executescript`
attribute of something. A bare `execute(x)` with no such binding is still
ignored, so the method requirement's reasoning survives intact. Any scope
counts, because the binding and the call are routinely in different functions —
here they are 400 lines and one parameter apart.

That is what makes the repair a **ratchet** rather than a correction nothing
holds. Verified in all four directions: removing `quote_literal` from the
`LOCATION` → exit 1; removing `quote_name` from the identifier → exit 1; both
escapes in place → exit 0; and with `_py_sql_aliases` stubbed to the empty set,
the raw site reports **green**, which is what the clause is for.

`quote_name` is the third helper, and it is the dotted composition of
`quote_ident` rather than a new escape. It exists for one measured reason: this
site's **schema half is optional**, so the two-hole spelling `catalog.register`
uses cannot be written here without a conditional inside the f-string — and a
conditional in the hole is not a call, so R8 would read the repaired site as
unescaped and the fix would not hold. The escape has to be *at* the splice for
the same reason a local would defeat it: the rule decides on the expression in
the hole and does not follow a variable back (§6).

**What is *not* fixed, deliberately.** `apply` still splices `entry["rows"]`
raw, because a row filter **is** the policy author's own SQL and using it as
the view's source is how this module avoids parsing user SQL — its docstring
notes that "a parser that is 95% right is a security control that is 5%
absent". So `apply` stays a ledger entry, recording the splice the way
`python-shell-injection` records the places this repository executes submitted
code.

That entry also names a limitation it *creates*, which is worth stating because
it is not obvious: the ledger key is `(file, rule, symbol)`, so once `apply` is
recorded for `sql`, the gate can no longer tell `quote_ident(name)` from a raw
`{name}` there — **verified, not assumed; reverting the name leaves `--strict`
at exit 0.** For that one symbol the guard is
`python/tests/test_spark_agent_sql_ident.py`, which pins the emitted
`CREATE OR REPLACE TEMP VIEW` text and fails on exactly that revert. Every
other repaired site is *not* ledgered and is held by the gate itself.

**The test is a table, and that is the `tls_scope` lesson applied rather than
restated.** When six TLS skips were repaired onto a host-scoped helper, nothing
asserted the helper, and it was wrong in the permissive direction for two
releases — both defects being single rows nobody thought to write. So
`sqlident` ships with the rows that describe how a name can carry the quote
character (one backtick, a run of them, a `` name` AS SELECT … ``-shaped
breakout, dotted, empty, a non-string, NUL and newline refusals), driven
through the helper **and** through `apply`, `restore`, `_sweep_qualified`,
`_drop`, `schemas_holding`, `secure_view_sql` and `usercontext.register` with a
recording fake engine. A perfect helper proves nothing if `apply` stops calling
it, so a one-sided correction — helper kept, call site reverted, or the
reverse — fails.


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
- **`values`** is required for `sql-identifier-interpolated`, and is that
  rule's `hosts`: what decides whether spliced SQL is safe is *what* is
  interpolated and *what escapes it*, so an entry naming neither records the
  site without recording the thing worth reviewing. For the six Go entries it
  is the only place the escape is written down at all, because that half of
  the rule reads the enclosing function rather than the argument.

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
- **R8 reads the statement's shape, not the value's taint — and the cost is
  measurable.** It asks whether SQL text is assembled by interpolation, never
  whether what goes in crossed a trust boundary. Its one narrowing in that
  direction is shallow by design (a literal, or a name bound at module level),
  so thirteen of its twenty ledger entries sit exactly *one* step past it: a
  loop variable over a literal tuple, a local assigned from a constant. Those
  are recorded rather than narrowed away on purpose — following the step is
  taint analysis, and a narrowing would have made a probe that *starts*
  interpolating a catalog name invisible — but it does mean the ledger is
  larger than the risk, and a reader should take the `values` field, not the
  entry count, as the signal.
- **R8 reads the argument AT the call, so a template bound to a variable first
  is invisible to it.** `spark.sql(stmt)` where `stmt` was built three lines
  earlier is not a finding, and neither is a function that *returns* SQL text
  for someone else to execute. Both shapes existed in the code this rule was
  written against: `onelake_security._drop` binds its two statements to a
  variable, and `secure_view_sql` returns the projection a column-only grant
  produces. **Both were repaired anyway**, because they are the same names on
  the same enforcement path — a fix that stopped where the checker's vision
  stops would have left the withholding path and the CLS projection as the
  only unescaped routes. They are held by
  `python/tests/test_spark_agent_sql_ident.py` rather than by the gate.

  The same limit runs the other way and constrains how a repair may be
  *written*: the escape has to sit in the hole. `name = quote_name(schema,
  tbl)` followed by `f"… {name}"` is escaped code that the rule reads as
  unescaped, so `delta_ops.execute_ctas` calls both helpers inline — not for
  style, but because a local there would have left the fix unheld in exactly
  the direction this bullet describes.
- **R8's third blind spot was an ALIASED CALLEE, and finding it is the honest
  part of this rule's history.** It required `ast.Attribute` — a *method* — on
  R7's measured reasoning that `execute` is a name anything may have. That
  reasoning does not reach a bare name that **is** `spark.sql`, and
  `delta_ops` writes exactly that (`original_sql = spark.sql`, then passed as a
  parameter). So the module whose `LOCATION` splice was the most reachable of
  the eight was the one site R8 could not see, inside the package its own
  scope sentence named. It is closed now (`_py_sql_aliases`, §4), and the two
  bullets above are the shapes that remain. **What generalises is not the fix
  but the pattern**: this is the third time in this document a rule's
  confident scope sentence outran its implementation — `verify=False` without
  the `ssl` dialect, a host check that read a name instead of an address, and
  now a method requirement that missed the method bound to a name. Each was
  found by someone re-reading the claim against the tree rather than by the
  gate, and that is the thing this section exists to keep possible.
- **R8 has no T-SQL or Spark-SQL parser, and no TypeScript half.** It decides
  "is this SQL" from the first verb the author wrote, which is why its Go side
  needs an upper-case match and a `database/sql` import to stay out of English
  prose. A statement whose verb arrives through a hole, or one spelled in a
  dialect that starts somewhere else, is not read as SQL.
- **R8's Go half cannot see an escape, so its six sites are ledgered rather
  than green.** The Python half decides on the AST and recognises
  `quote_ident`/`quote_literal`, so a repaired site reports clean and removing
  the escape fails the build. The Go half is regex and brace-scanning, reads
  the enclosing *function*, and cannot inspect the expression substituted into
  a format string — so `internal/tds.principalName`'s correct `]` doubling and
  a raw `%s` look identical to it. Those entries are regression guards whose
  `values` field is the only written record of the escape, and the Go tests
  are what actually pin the emitted text.
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
