# 63 — Security foot-guns: what scanned this source, what did not, and what now guards it

**Status: nothing in this repository read its own source for insecure code
shapes. Three scanners run here and all three answer a different question —
govulncheck watches dependencies, gitleaks watches committed strings, and
Dependabot watches versions. `.golangci.yml` enables no gosec and
`pyproject.toml` does not select ruff's flake8-bandit family. The category was
absent rather than incomplete, which is why
[SECURITY.md](../SECURITY.md)'s honest "what does not run" section could not
name it. `scripts/check_security_footguns.py` now holds it, and the first run
found one real defect.**

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
| `check_dismissed_advisories.py` | `security.yml` | a dismissal whose justification expired | no — alert state |
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

## 3. The rules, and what each deliberately does *not* flag

The exclusions are the substance. Each one below was a real false positive on
this tree, and each is pinned by a test in
`python/tests/test_check_security_footguns.py` that fails if it is lost.

### `tls-verification-off` — ledgered, not banned

`InsecureSkipVerify: true`, `verify=False`, `connection_verify=False`. A local
emulator talking to a sibling's self-signed certificate is the normal case here,
so every site is recorded rather than forbidden — **and the entry must name the
host set that client can reach.** That field is the whole point: it is precisely
what nobody had written down when `internal/akv`'s skip reached an Azure domain.
Twelve sites, thirteen findings.

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

## 4. The one real finding, and its fix

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
- **It does not read TypeScript.** The portal is out of scope, as it is for the
  `check_*_test_flakiness.py` trio's AST-based members.
- **There is still no SBOM and no licence check**, and Python and npm
  dependencies still get no reachability-filtered advisory scan. Those gaps are
  SECURITY.md's and are unchanged by this document.
