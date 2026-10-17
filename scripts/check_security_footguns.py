#!/usr/bin/env python3
"""Every security anti-pattern in this repository's own source is absent, or is
recorded in docs/security-footguns.json with the host set and the reason.

WHY THIS EXISTS. Nothing in this tree reads this repository's source looking for
insecure code SHAPES. Measured at the commit that added this: `.golangci.yml`
enables [errcheck, govet, ineffassign, staticcheck, unused] and no gosec, so the
Go linter has no security analyser at all; `.github/workflows/security.yml` runs
govulncheck (a vulnerable dependency this code can REACH) and gitleaks (a secret
string that was COMMITTED). Those are three different questions, and the third
one -- is the code here written in a shape that leaks a credential or drops a
trust check -- had nobody asking it. SECURITY.md's "what does not run" section
names the dependency gaps honestly and could not name this one, because the gap
was the absence of the category rather than a hole in it.

THE RULE SET IS SECURITY.md'S IN-SCOPE LIST, not a generic scanner's. This
emulator is DELIBERATELY insecure in documented ways -- seeded identities with
published secrets, self-signed TLS, an unauthenticated admin API -- and
SECURITY.md says so and calls those design rather than findings. A checker that
reported them would be argued with rather than fixed, which is how a guard stops
being read. So each rule below maps onto a bullet SECURITY.md already treats as
in scope: real credentials leaking (R1, R4), authorization logic that is wrong
rather than absent (R1, R3, R8), escape from the emulator to the host (R5, R6,
R7), and T-SQL or notebook input reaching the host beyond the documented
execution surface (R5, R8).

WHAT IS FLAGGED, and what each rule deliberately does NOT flag:

  * `tls-verification-off` (R1) -- `InsecureSkipVerify: true`, `verify=False`,
    `connection_verify=False`, AND the stdlib `ssl` dialect: an assignment of
    `ssl.CERT_NONE` to a `*.verify_mode`, of `False` to a `*.check_hostname`,
    a call to `ssl._create_unverified_context()`, or an assignment to
    `ssl._create_default_https_context`. Every site is LEDGERED rather than
    banned, because a local emulator talking to a sibling's self-signed cert is
    the normal case here. The ledger entry must name the HOST SET that client
    can reach, which is the thing that actually decides whether the skip is
    safe -- internal/akv reaches real `*.vault.azure.net` by design, which is
    why its skip is scoped to one configured host instead of the transport. A
    new unledgered site fails, so that distinction cannot be lost by default.

    THE `ssl` DIALECT WAS INVISIBLE FOR THIS RULE'S FIRST TWO RELEASES, and it
    is the same defect class as the internal/akv finding this docstring
    celebrates, hiding behind a spelling the rule had not learned. `verify=False`
    is requests'; the stdlib's is the pair above, and the pair is what this
    repository's OWN Python reaches for. Adding it took R1 from 14 findings to
    53 across 39 newly visible sites -- and six of those carried a credential
    over the unverified connection. `eventstream_kafka._fabric_request` is the
    sharp one: it sends a bearer token to a `FABRIC_API_URL` that DEFAULTS to
    `api.fabric.microsoft.com`, so the default configuration talked to a real
    Microsoft endpoint with the trust check off. Four more POST
    `ENTRA_CLIENT_SECRET` to a URL that is configuration. All six are now scoped
    per host (spark_agent/tls_scope.py and fabric_target's `_tls_context`,
    both mirroring akv.hostScopedTLS) rather than recorded, which is why the
    entries below for those files describe a GUARD rather than an allowance.

    THE PAIRED IDIOM REPORTS ONCE. `check_hostname = False` is suppressed when
    the same enclosing symbol also sets `verify_mode = ssl.CERT_NONE`, because
    that is one context and one trust decision. The ledger key is (file, rule,
    symbol), so a pair was always one ENTRY; this keeps the report, and the
    counts in the prose, matching what a reader would count by hand. It is
    scoped to the SYMBOL, not the file, so a second function's lone skip in the
    same file is still its own finding.
  * `weak-random-for-secrets` (R2) -- `math/rand` in Go or `random` in Python
    BOUND TO a credential: assigned to a credential-named target, or returned
    from a credential-named function. Security material must come from
    `crypto/rand` / `secrets`. Judged on what the randomness becomes rather than
    on words in the file, which is the correction that took this rule from four
    findings to zero -- all four were `rnd = random.Random(SEED)` in
    examples/contoso-fixtures*, a deliberately SEEDED generator producing rows an
    example asserts against, in files that say "key" because they describe a
    primary key. Baseline zero, and no non-test Go file imports `math/rand`.
  * `secret-compared-directly` (R3) -- a secret, signature, MAC or digest
    compared with `==` / `!=` / `strings.EqualFold` instead of `hmac.Equal` or
    `subtle.ConstantTimeCompare`. Comparisons against a LITERAL are not flagged
    (`if token == ""` is a presence check, and flagging it is how this rule
    would have become noise). Baseline zero, and it is a pure regression guard:
    the repo imports neither constant-time helper today and
    `internal/awssig/sigv4.go` is the only `crypto/hmac` user, for signing.
  * `credential-in-message` (R4) -- a credential VALUE interpolated into a log
    or an error. THE FORMAT STRING IS NEVER SCANNED, only the arguments after
    it, and that is the single decision that makes this rule usable here: the
    dominant shape in this tree is `fmt.Errorf("%w: signature", ErrBadToken)`
    (thirteen in internal/auth/auth.go alone) and
    `fmt.Errorf("unexpected token %q", t.text)` in the pipeline and DAX
    parsers, where the word "token" is in the MESSAGE and the argument is a
    sentinel or a parser lexeme. A naive scan reports every one of them.
    `Err*`-prefixed and `*Type`/`*Kind`/`*Name`/`*ID`/`*URL`-suffixed arguments
    are excluded for the same reason -- a discriminator is not a value, and
    `creds.CredentialType` is what the arguments-only reading hits. A call is
    judged by its FUNCTION, not its arguments, so `len(SECRETS)` is a count and
    `get_token_seconds_remaining(t)` is a duration. Baseline ONE, recorded: the
    `-terminal-url` pane prints its own generated token to stdout for the
    operator to copy, the way Jupyter does, and the line above it says so.
  * `python-shell-injection` (R5) -- `shell=True`, `os.system`, a `subprocess`
    call whose command is built by f-string or concatenation, `yaml.load`
    without a safe Loader, and `eval`/`exec` over anything but a literal.
    SECURITY.md puts "command injection through a pipeline expression" in
    scope; this is the same class in this repo's own tooling. A built command is
    only flagged WITH a shell behind it -- `subprocess.run(cmd + ["up"])` is
    argv, not syntax, and twenty sites across e2e/ have that shape.

    Baseline THIRTEEN, all recorded and all `exec`/`eval`: the spark agent's
    statement executor, the notebookutils UDF runner, and the harness/example
    engines that run notebook cells. Running submitted code is what those
    components ARE -- SECURITY.md scopes the in-scope case as escape "beyond the
    documented execution surface", and a notebook cell reaching the interpreter
    IS that surface. They are recorded rather than excluded so the SET of places
    this repository executes arbitrary code stays enumerated and a new one fails.
    Worth noting: the tree already carried fifteen `# noqa: S###` directives
    marking several of these, for a ruff rule family (flake8-bandit) that
    `pyproject.toml` does not select -- so those suppressions were suppressing a
    check nobody ran.
  * `world-writable-mode` (R6) -- `0o777`/`0o666` reaching `os.chmod`,
    `os.WriteFile`, `os.MkdirAll`. Baseline TEN findings in eight recorded
    sites, and the rule that motivated the ledger's shape: e2e harnesses widen a recording or
    coverage directory so a distroless-nonroot container (uid 65532) can write
    a bind mount that keeps the host's ownership. That is a documented
    workaround with its own comments (see scripts/coverage_prepare.sh and
    e2e/engine-matrix/run.py's `ensure_out_writable`), it is harness code that
    ships in no artifact, and the alternative -- a silently excluded directory
    -- is what this repository's other guards call out as the failure mode.
    So they are recorded, with the reason, and a NEW one still fails.
  * `archive-extraction-unchecked` (R7) -- an archive member extracted without
    anything checking where it lands. Python: `extractall` with no `filter=`
    and no `members=`, `shutil.unpack_archive`, and `.extract(<non-literal>)`
    on an archive object. Go: an archive header's name joined onto a
    destination with `filepath.Join` and no containment check in the same
    function. SECURITY.md puts escape from the emulator to the host in scope
    and R5/R6 already serve that bullet; this is its third shape.

    WHY `filepath.Join` IS NOT THE BOUND IT LOOKS LIKE: it CLEANS its result,
    so `Join("/out", "../../etc/x")` is `/etc/x` -- a perfectly clean path
    outside `/out`. Same for Python's default `extractall`, where
    `filter="data"` only became the default in 3.14 and this repository's floor
    is 3.12, so on the interpreter CI actually runs the unsafe default is live.

    Baseline ZERO, and both halves earned that differently. The PYTHON half had
    two real findings when it landed -- `e2e/azcopy/run.py` extracted an
    archive downloaded from an `aka.ms` redirect with bare `extractall`, both
    the zip and the tar branch -- and they were FIXED rather than recorded
    (`filter="data"` on the tar, a validated `members=` list on the zip), so
    the rule ships empty. The GO half is a pure regression guard and has never
    had a finding to report: no non-test Go file in this tree imports
    `archive/zip` or `archive/tar` at all, so it exists so that the first
    Go-side extractor lands as a finding instead of as a review comment.

    Two measured narrowings, both from false positives on this tree. `.extract`
    requires an archive module in the file AND a method call: the
    vendored-activity-types parser has `v.extract(page(...))`, which scrapes an
    HTML table, and `scripts/vendor_notebookutils_stubs.py` imports zipfile and
    defines its own `extract(url)` -- so the import gate alone did not save it,
    and that function reads members into a dict with `wheel.read(name)` and
    writes no member-named path. The Go clause needs the archive import for the
    same reason: `f.Name` is also how an `fs.DirEntry` is spelled.
  * `sql-identifier-interpolated` (R8) -- SQL text assembled by interpolation
    rather than built from a literal with its values bound. Python: a `.sql` /
    `.execute` / `.executemany` / `.executescript` METHOD call whose first
    argument is an f-string, a `%`/`+` build or `.format`, and whose
    author-written text starts on a DDL/DML verb. Go: `fmt.Sprintf`,
    `fmt.Fprintf` or a concatenating `strings.Builder` write carrying an
    upper-case SQL verb, inside a function that also calls `Exec*`/`Query*`,
    in a file that imports `database/sql`.

    A NAME SPLICED INTO SQL IS SYNTAX, NOT A VALUE. Backticks around it look
    like quoting and are not: a backtick inside the name closes the pair and
    the rest is parsed. It maps onto two SECURITY.md bullets at once --
    authorization logic that is wrong rather than absent, and T-SQL input
    reaching the host -- and R5 does not cover it, because R5's Python clause
    stops at `eval`/`exec`/`yaml.load` and a command string with a shell
    behind it. A SQL string handed to an engine is a different interpreter
    with a different fix.

    THE MEASURED ASYMMETRY IS WHY THIS RULE REPORTS A SHAPE RATHER THAN
    BANNING ONE. The GO half of this tree was already correct at every site:
    `internal/tds.principalName` doubles `]` -> `]]` for a bracket-quoted
    identifier and `quotedID` doubles `'` -> `''` for an N'...' literal,
    `internal/server`'s `sqlIdent`/`sqlLiteral` are the same pair one package
    over, and `enterDelegated` leans on SQL Server's own `QUOTENAME`. The
    PYTHON half escaped nothing -- not one backtick was doubled anywhere in
    `python/spark_agent` -- including `onelake_security.apply`, which installs
    the temp view that enforces OneLake row- and column-level security, where
    a name that breaks out of its statement is an authorization failure rather
    than a syntax error. A lakehouse table is a FOLDER under `Tables/` created
    by whoever can write to OneLake, and nothing in `internal/onelake` rejects
    a backtick in a path segment, so the name is data the whole way round.
    Eight symbols were repaired onto `python/spark_agent/sqlident.py`
    (`quote_ident` / `quote_literal` / `quote_name`, mirroring the Go pair);
    the six Go symbols are LEDGERED as a regression guard over code that is
    right.

    THE RULE RECOGNISES THE ESCAPE, and that is not a convenience. A rule that
    reported an escaped site as loudly as a raw one would say nothing about
    safety: every repaired site would need a ledger entry, and then deleting
    the `quote_ident` call would leave that entry valid and the build green --
    a guard that has stopped guarding and still reports success. So an
    escaped interpolation is not a finding, removing an escape makes it one
    again, and `--strict` goes to exit 1. It TRUSTS the named helper rather
    than re-deriving the escape, exactly as R7 trusts `filter="data"` instead
    of reimplementing tarfile; what makes that sound is that the helper is
    pinned by a name table driven through both the helper and the real call
    paths (python/tests/test_spark_agent_sql_ident.py).

    THE GO HALF HAS NO SUCH RECOGNITION, statedly: it is regex and
    brace-scanning rather than a parser, so it sees a FUNCTION and not the
    expression substituted into a format string. Its six sites are therefore
    ledgered, and for them the `values` field is the only place the escape is
    written down at all.

    Four measured narrowings, each from a false positive on this tree. The
    VERB GATE is what stops this being a rule about f-strings: without it
    every `f"{x}"` reaching any method called `execute` is a finding, and the
    verb must come from text the AUTHOR wrote, so interpolated parts are
    blanked before the test -- `f"{verb} FROM t"` must not pass on a word
    nobody typed. MODULE-LEVEL CONSTANTS are skipped, which is what most e2e
    probes are (`e2e/sail-session-isolation/probe.py` is
    `s.sql(f"DROP VIEW IF EXISTS {VIEW}")` over a file-local constant). The
    Go half needs the `database/sql` IMPORT GATE because `.Query(` is also how
    `net/url`'s is spelled -- `u.Query().Has("pbi_source")` in
    internal/api/fabriciq.go satisfied the "this function executes SQL" test
    on a URL parse -- and its verb search is CASE-SENSITIVE because four of
    the verbs (WITH, USE, SET, SHOW) are ordinary English: with `re.I` it
    reported that same file's `"No Report or SemanticModel with id %q that you
    can access."` on the word "with".

    Baseline TWENTY findings, all recorded: the six correct Go symbols, nine
    e2e harness probes and four example scripts whose interpolated values are
    all constants or literals this file itself wrote, and
    `onelake_security.apply` -- whose `name` is escaped and whose `sql` is the
    policy author's own row filter, spliced on purpose.

SCOPE IS NON-TEST SOURCE. Test files are excluded because a test's job includes
constructing the violation -- python/tests/test_check_example_portability.py
holds the literal string `verify=False` as a fixture, and this checker's own
test module holds one of every rule. e2e/ harness code is IN scope and is not
treated as test code: it runs real drivers against real containers, and its
`verify=False` calls are exactly the sites worth recording.

WHY A LEDGER RATHER THAN A FLAT BAN, and why it is checked in BOTH directions:
the same reasoning as its siblings. `--strict` fails on any finding not in
docs/security-footguns.json, and equally on any entry there whose site has gone
away or been FIXED -- a closed gap left recorded goes on excusing the file, and
would silently re-cover it if the shape came back. That direction is the one a
"what's new" reader would never think to check.

THE MEASURED BASELINE, as of the change that added R8: 93 findings across 578
non-test source files, in 85 distinct (file, rule, symbol) keys, every one
recorded in the ledger with its reason -- 49 tls-verification-off, 20
sql-identifier-interpolated, 13 python-shell-injection, 10 world-writable-mode,
1 credential-in-message, and zero for all three regression-guard rules
(weak-random-for-secrets, secret-compared-directly, archive-extraction-unchecked).

THE FILE COUNT HAD ALREADY DRIFTED, and it is restated here from a measured run
rather than carried forward. The three paragraphs above this one claimed 576
files while the gate itself was printing 577 -- prose that goes stale the moment
somebody adds a source file, in the one place a reader checks whether the sweep
still covers the tree. 578 is 577 plus `python/spark_agent/sqlident.py`, which
this change added.

SEVENTEEN FURTHER FINDINGS ARE NOT IN THE LEDGER, because they were FIXED
rather than recorded -- in three changes, and the first one is why this file
exists:

  * internal/akv set InsecureSkipVerify transport-wide from a flag documented
    for entra-emulator's certificate, on the one client whose allowlist
    deliberately admits a real `*.vault.azure.net`. Scoped to one host
    (akv.hostScopedTLS).
  * SIX `ssl`-dialect skips that the rule could not see until it learned that
    dialect, each applied to a CALL rather than to a host, and each on a URL
    that is configuration: `eventstream_kafka._fabric_request` (a bearer token
    to a `FABRIC_API_URL` defaulting to `api.fabric.microsoft.com`),
    `eventstream_kafka._fabric_token`, `storage._mint`,
    `storage._forge_attributed`, `usercontext.caller_token` (four of which POST
    `ENTRA_CLIENT_SECRET`) and `fabric_target`'s `_EmulatorCredential.get_token`.
    All now scoped per host, the same repair as akv's, one language over.
  * TWO unchecked archive extractions in `e2e/azcopy/run.py`, on an archive
    downloaded from an `aka.ms` redirect.
  * EIGHT symbols across `python/spark_agent` that spliced a table, schema,
    view, column or LOCATION into SQL text with nothing doubling a backtick or
    an apostrophe -- `catalog.register`, `catalog._make_unqualified_resolve`,
    `onelake_security.schemas_holding`, `.restore`, `._sweep_qualified`,
    `.secure_view_sql` (and `._drop`), `usercontext.register`, and
    `delta_ops.execute_ctas`. The sharp one is the pair that enforces policy:
    `apply` built the temp view holding the row filter, and `secure_view_sql`
    built a column-only projection, both from a name that originates as a
    OneLake folder. All now go through `sqlident.quote_ident` /
    `quote_literal` / `quote_name`, leaving ordinary names byte-identical so no
    fixture, recording or e2e output moved.

    THREE OF THOSE EIGHT THIS RULE COULD NOT SEE, and they were repaired
    anyway. Two it still cannot: `secure_view_sql` RETURNS a string rather
    than calling `.sql()`, and `_drop` binds its templates to a variable
    before `spark.sql(stmt)` reads them. A repair that stopped where the
    checker's vision stops would have left the CLS projection and the
    WITHHOLDING path -- the one that runs when a table must not be readable --
    as the only unescaped routes.

    THE THIRD IS NOW VISIBLE, and that is the correction worth reading rather
    than the count. `delta_ops.execute_ctas` is reached as `original_sql(...)`,
    a bare NAME that delta_ops binds to `spark.sql` and passes as a parameter,
    so R8's method requirement -- measured, and right about `execute` being a
    name anything may have -- walked straight past it. It spliced a
    catalog-supplied schema location into `LOCATION '{loc}'` with nothing
    doubling an apostrophe, in the one package this rule's scope sentence
    claimed to have swept, while `catalog.register` escaped the SAME value one
    module over. `_py_sql_aliases` closes it: a bare call counts when this file
    itself binds that name to a `.sql`/`.execute` attribute. So the repair
    RATCHETS -- deleting either escape takes `--strict` to exit 1 -- instead of
    being a correction nothing holds.

The ledger does carry the two host-scoped HELPERS, because the checker sees the
assignments and cannot see the guard above them -- those entries record a fix,
and say so. See docs/63-security-footguns.md.

Usage:
    check_security_footguns.py             report findings, exit 0
    check_security_footguns.py --strict    exit non-zero on unrecorded findings
"""
import ast
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
LEDGER = ROOT / "docs" / "security-footguns.json"

# Everything this repository actually writes. `third_party/` is vendored stubs,
# `portal/dist` is a build artifact committed for go:embed, and `.claude` can
# hold a git WORKTREE -- a second checkout of this repo nested inside it -- so a
# sweep that walked one would report findings against a sibling branch's files.
SKIP_DIRS = {
    ".git", ".claude", ".venv", "venv", "node_modules", "third_party",
    "dist", "build", "_site", "coverage", "covdata", ".svelte-kit",
    "__pycache__", ".pytest_cache", ".ruff_cache", "vendor",
}

# A rule is (id, human sentence) -- the sentence is what a failure prints, so it
# has to say what to do instead rather than merely naming the shape.
REMEDY = {
    "tls-verification-off":
        "Scope the skip to the one host whose certificate is actually untrusted "
        "(internal/akv's hostScopedTLS is the pattern), or record the site in "
        "docs/security-footguns.json with the host set it can reach and why "
        "verification off is correct there.",
    "weak-random-for-secrets":
        "Security material must come from crypto/rand (Go) or secrets (Python). "
        "math/rand and random are seeded predictably and are for test fixtures "
        "and jitter, not for anything an attacker may guess.",
    "secret-compared-directly":
        "Compare secrets, signatures, MACs and digests with hmac.Equal or "
        "subtle.ConstantTimeCompare: == returns as soon as two bytes differ, so "
        "how long it took is how much of the secret was right.",
    "credential-in-message":
        "Log or wrap an identifier for the credential, never its value: a log "
        "line, an error body and an event all outlive the request and get "
        "pasted into issues. SECURITY.md puts a real token in a log in scope.",
    "python-shell-injection":
        "Pass argv as a list with no shell, use yaml.safe_load, and do not "
        "eval/exec anything that is not a literal.",
    "world-writable-mode":
        "0o777 and 0o666 let any local user rewrite the file, including one the "
        "emulator later reads back as input. Use 0o755/0o644, or record the "
        "site with the reason the wider mode is needed.",
    "archive-extraction-unchecked":
        "An archive member's name is chosen by whoever built the archive, so "
        "`extractall` writes wherever the member says -- a `../../` component "
        "escapes the destination entirely (zip-slip / tar-slip). Pass "
        "filter=\"data\" to tarfile, or validate every member name against the "
        "extraction root before writing it.",
    "sql-identifier-interpolated":
        "A name spliced into SQL text is SQL SYNTAX, not a value: a backtick in "
        "a Spark identifier, or a `]` in a bracket-quoted T-SQL one, ends the "
        "quoting and the rest of the name becomes statement. Escape the "
        "identifier by DOUBLING its quote character -- spark_agent's "
        "quote_ident, or internal/tds.principalName one language over -- or "
        "record the site in docs/security-footguns.json with a `values` field "
        "naming what is interpolated and what escapes it.",
}


def relkey(path, root=None):
    """A path as the ledger spells it: relative to the root, forward slashes.

    Same reasoning as every sibling checker's `relkey` -- a raw
    `str(path.relative_to(ROOT))` is backslash-separated on Windows, and a
    ledger keyed on that separator reads every real site as unrecorded and every
    real entry as stale on that platform alone.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    try:
        p = p.relative_to(root if root is not None else ROOT)
    except ValueError:
        pass
    return str(p).replace("\\", "/")


def is_test_file(rel):
    """Test code, which is excluded -- see the docstring's SCOPE paragraph.

    e2e/ is deliberately NOT test code by this definition: it is harness code
    driving real clients at real containers, and its TLS-verification skips are
    the sites most worth recording.
    """
    name = rel.rsplit("/", 1)[-1]
    if name.endswith("_test.go"):
        return True
    if name.startswith("test_") or name.endswith("_test.py"):
        return True
    return rel.startswith("python/tests/")


def source_files(root=None):
    """Every non-test .go and .py file in scope, sorted for a stable report."""
    base = root if root is not None else ROOT
    out = []
    for path in sorted(base.rglob("*")):
        if path.suffix not in (".go", ".py") or not path.is_file():
            continue
        rel = relkey(path, base)
        if any(part in SKIP_DIRS for part in path.relative_to(base).parts[:-1]):
            continue
        if is_test_file(rel):
            continue
        out.append(path)
    return out


# --- shared vocabulary -----------------------------------------------------
# The words that make an identifier a credential. Matched case-insensitively
# against one segment of a dotted chain, so `creds.token` and `Token` both hit.
CRED_WORDS = (
    "token", "secret", "password", "passwd", "bearer", "credential",
    "apikey", "api_key", "privatekey", "private_key", "clientsecret",
    "client_secret", "sastoken", "sas_token", "connectionstring",
    "connection_string", "accountkey", "account_key",
)
# ...and the words that make one a secret worth comparing in constant time.
# Narrower than CRED_WORDS on purpose: a timing oracle needs a comparison whose
# duration leaks HOW MUCH of the value was right, which is about verifying a
# secret, not about carrying one around.
COMPARE_WORDS = (
    "secret", "signature", "sig", "mac", "hmac", "digest", "checksum",
    "token", "password", "passwd", "apikey", "api_key",
)
# Suffixes that make a credential-named identifier a DISCRIMINATOR rather than a
# value, and the `Err` prefix that makes it a sentinel. Both measured rather
# than guessed: under the arguments-only reading of R4, the only two hits in
# this repository are `creds.CredentialType` (a kind, logged to say which kind
# was refused) and the thirteen `ErrBadToken` wraps in internal/auth.
NON_VALUE_SUFFIXES = (
    "type", "kind", "name", "id", "url", "uri", "path", "count", "len",
    "error", "err", "mode", "method", "scheme", "host", "field", "header",
    "prefix", "suffix", "audience", "issuer", "expiry", "expires",
    # A quantity DERIVED from a credential is not the credential. Measured:
    # e2e/sempy logs `get_token_seconds_remaining(_t)`, which is how long a
    # token has left -- diagnostics that leak nothing.
    "remaining", "seconds", "secs", "size", "length", "exp", "at",
)
NON_VALUE_PREFIXES = ("err",)


def _segments(expr):
    """The identifier segments of an expression, e.g. `a.B.c()` -> [a, B, c]."""
    return re.findall(r"[A-Za-z_][A-Za-z0-9_]*", expr)


_QUOTED = re.compile(r"'[^']*'|\"[^\"]*\"|`[^`]*`")
_CALL_HEAD = re.compile(r"^\s*([A-Za-z_][\w.]*)\s*\(")


def _is_credential_ref(expr, words=CRED_WORDS):
    """Does this expression name a credential VALUE?

    Judged on the LAST identifier segment: `creds.CredentialType` is decided by
    `CredentialType`, not by `creds`, because the chain's head says where a value
    came from and its tail says what the value is.

    TWO measured corrections, both from false positives this rule produced
    against this tree on its first run:

      * STRING LITERALS ARE STRIPPED FIRST. `scalar(owner, 'SELECT count(*) FROM
        secret')` names a secret only inside a SQL string, where `secret` is a
        TABLE. Reading a quoted region as an identifier makes every query naming
        a sensitive table look like a leak.
      * A CALL IS JUDGED BY ITS FUNCTION, not by its arguments, because what a
        call evaluates to is described by the function and not by what went in.
        `len(SECRETS)` is a count and `r.Header.Get("Authorization")` is a
        lookup; under the arguments reading both looked like credential values.
    """
    expr = _QUOTED.sub("''", expr or "")
    head = _CALL_HEAD.match(expr)
    if head:
        expr = head.group(1)
    segs = _segments(expr)
    if not segs:
        return False
    last = segs[-1]
    low = last.lower()
    if any(low.startswith(p) for p in NON_VALUE_PREFIXES):
        return False
    if any(low.endswith(s) for s in NON_VALUE_SUFFIXES):
        return False
    return any(w in low for w in words)


def _balanced(text, open_paren):
    """The text between `text[open_paren]` == '(' and its matching ')'.

    Quote-aware, because an argument may contain a paren inside a string --
    `fmt.Errorf("bad (unclosed", x)` would otherwise swallow the rest of the
    file. Returns (inner, index_after_close).
    """
    depth, i, n = 0, open_paren, len(text)
    while i < n:
        ch = text[i]
        if ch in "\"'`":
            quote, i = ch, i + 1
            while i < n:
                if text[i] == "\\" and quote != "`":
                    i += 2
                    continue
                if text[i] == quote:
                    break
                i += 1
            i += 1
            continue
        if ch in "([{":
            depth += 1
        elif ch in ")]}":
            depth -= 1
            if depth == 0:
                return text[open_paren + 1:i], i + 1
        i += 1
    return "", n


def _split_args(inner):
    """Top-level comma split of an argument list, quote- and nesting-aware."""
    args, depth, cur, i, n = [], 0, [], 0, len(inner)
    while i < n:
        ch = inner[i]
        if ch in "\"'`":
            quote, start = ch, i
            i += 1
            while i < n:
                if inner[i] == "\\" and quote != "`":
                    i += 2
                    continue
                if inner[i] == quote:
                    break
                i += 1
            cur.append(inner[start:i + 1])
            i += 1
            continue
        if ch in "([{":
            depth += 1
        elif ch in ")]}":
            depth -= 1
        if ch == "," and depth == 0:
            args.append("".join(cur).strip())
            cur = []
            i += 1
            continue
        cur.append(ch)
        i += 1
    tail = "".join(cur).strip()
    if tail:
        args.append(tail)
    return args


# --- Go ---------------------------------------------------------------------
_GO_FUNC = re.compile(r"^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)")
_GO_INSECURE = re.compile(r"\bInsecureSkipVerify\s*:\s*true\b")
_GO_MATHRAND = re.compile(r'^\s*(?:[\w.]+\s+)?"math/rand(?:/v2)?"')
# Errors and logs only. fmt.Sprintf is deliberately NOT here: it builds request
# URLs, SQL and IDs all over this tree, so including it would make the rule
# about string formatting rather than about what reaches a log -- and a token in
# a URL is a different finding with a different fix.
_GO_LOGCALL = re.compile(
    r"\b(?:fmt\.Errorf"
    r"|log\.(?:Printf|Println|Print|Fatalf|Fatalln|Fatal|Panicf|Panicln)"
    r"|\w+\.(?:Printf|Errorf|Warnf|Infof|Debugf|Errorln|Warnln))\s*\(")
_GO_EQUALFOLD = re.compile(r"\bstrings\.EqualFold\s*\(")
# The operands may be calls, and the argument list has to be captured WITH them:
# `_compares_secrets` decides partly on whether a side is a call, and a pattern
# that stopped at the opening paren reported internal/purview's
# `token == r.Header.Get("Authorization")` as a secret comparison.
_GO_CMP = re.compile(
    r"([A-Za-z_][\w.]*(?:\([^()]*\))?)\s*(==|!=)\s*([A-Za-z_][\w.]*(?:\([^()]*\))?)")
_GO_MODE = re.compile(r"\b(?:os\.WriteFile|os\.MkdirAll|os\.Mkdir|os\.Chmod|\.Chmod)\s*\(")
_WORLD_WRITABLE = re.compile(r"\b0o?(?:777|666)\b")
# The archive rule's Go half. A GUARD AT ZERO, stated rather than implied: no
# non-test Go file in this tree imports archive/zip or archive/tar today, so
# this rule has never had a finding to report and is here so that the first
# Go-side extractor lands as one.
_GO_ARCHIVE_IMPORT = re.compile(r'^\s*(?:[\w.]+\s+)?"archive/(?:zip|tar)"')
# `filepath.Join(dest, hdr.Name)` -- the header's own name joined onto a
# destination. filepath.Join CLEANS its result, which is what makes this read
# as safe and is not: Join("/out", "../../etc/x") is "/etc/x", a perfectly
# clean path outside /out.
_GO_ARCHIVE_JOIN = re.compile(
    r"filepath\.Join\s*\([^)]*\b(?:hdr|header|h|f|file|zf|entry)\.Name\b")
# What makes it safe: the joined path compared back against the destination.
# Either spelling counts -- a prefix test, or filepath.Rel's `..` check.
_GO_CONTAINED = re.compile(
    r"strings\.HasPrefix\s*\(|filepath\.Rel\s*\(|\.\.(?:/|\"|`)")
# R8's Go half. Sprintf and Fprintf build the statement text; a strings.Builder
# `WriteString` does too, but only when its argument is CONCATENATED -- a
# WriteString of a bare literal interpolates nothing, and internal/server's
# onelakesync.go writes plenty of those.
_GO_SQL_FORMAT = re.compile(r"\b(?:fmt\.Sprintf|fmt\.Fprintf)\s*\(")
_GO_SQL_WRITESTRING = re.compile(r"\b\w+\.WriteString\s*\(")
# What makes the function a SQL one rather than a string-formatting one. Judged
# on the ENCLOSING FUNCTION, not the line, because the build and the execution
# are necessarily different statements -- and this gate is why `fmt.Sprintf`
# stayed out of R4's log vocabulary without costing this rule anything: the
# same call is a URL builder in internal/api and a statement builder here, and
# only the Exec/Query beside it tells them apart.
_GO_SQL_EXEC = re.compile(
    r"\.(?:Exec|ExecContext|Query|QueryContext|QueryRow|QueryRowContext)\s*\(")
# SEARCHED rather than anchored, which is the one place the Go half reads
# differently from the Python one. A Python `spark.sql(...)` argument IS the
# whole statement, so it starts on its verb; a Go statement is assembled from
# fragments, and this repository's are mid-statement by construction --
# `"\nIF IS_ROLEMEMBER(N'%s', N'%s') = 1 ALTER ROLE [%s] DROP MEMBER [%s];"`
# begins on IF and carries its verb four tokens in.
#
# AND CASE-SENSITIVE, which the Python half is not and does not need to be.
# Searching rather than anchoring means the verb may be matched anywhere in the
# string, and four of these words -- WITH, USE, SET, SHOW -- are ordinary
# English. Measured: with `re.I` this reported internal/api/fabriciq.go's
# `"No Report or SemanticModel with id %q that you can access."`, an MCP tool's
# refusal message, on the word "with". Every SQL statement this tree's Go
# assembles spells its keywords in upper case, so requiring that costs nothing
# and takes the prose out of scope.
_GO_SQL_VERB = re.compile(
    r"\b(?:SELECT|INSERT|UPDATE|DELETE|CREATE|DROP|ALTER|GRANT|REVOKE|SHOW"
    r"|MERGE|TRUNCATE|WITH|USE|SET)\b")
# ...and the file must actually hold a `database/sql` handle. The SAME import
# gate R7's `.extract` clause carries, for the same measured reason: `.Query(`
# is also how `net/url`'s is spelled, and `u.Query().Has("pbi_source")` in
# fabriciq.go satisfied the "this function executes SQL" test on a URL parse.
# A function with no sql.DB in the file is not running a statement.
_GO_SQL_IMPORT = re.compile(r'^\s*(?:[\w.]+\s+)?"database/sql"')


def _go_symbol(lines, idx):
    """The Go function a line sits in: the nearest preceding `func`.

    A straight-line reading, the same simplification the Go flakiness checker's
    brace counter makes. Exactly right for this tree, where every site scanned
    is inside a top-level func.
    """
    for i in range(idx, -1, -1):
        m = _GO_FUNC.match(lines[i])
        if m:
            return m.group(1)
    return "<file>"


def _go_func_span(lines, idx):
    """The lines of the Go function containing `lines[idx]`.

    Start-of-func to the next top-level `func`, which is the same straight-line
    reading `_go_symbol` makes and is exactly right for this tree, where every
    site scanned sits in a top-level func. Used by the archive rule to ask
    whether the containment check is in the SAME function as the join -- a
    `filepath.Rel` three functions away does not protect this one.
    """
    start = 0
    for i in range(idx, -1, -1):
        if _GO_FUNC.match(lines[i]):
            start = i
            break
    end = len(lines)
    for i in range(idx + 1, len(lines)):
        if _GO_FUNC.match(lines[i]):
            end = i
            break
    return lines[start:end]


def _literal(expr):
    """Is this a literal, rather than something computed?

    Used by R3: `token == ""` is a presence check and `n != 0` is arithmetic.
    Flagging those is how a constant-time-comparison rule turns into noise and
    gets argued with instead of fixed.
    """
    e = expr.strip()
    if not e:
        return True
    if e[0] in "\"'`":
        return True
    return e in ("nil", "true", "false") or re.fullmatch(r"-?\d[\w.]*", e) is not None


def _compares_secrets(lhs, rhs):
    """Is this comparison the one a constant-time helper exists for?

    Both sides credential-named, OR one side credential-named and NEITHER side a
    call. The call clause is the measured correction: this rule's only hit
    against this tree was internal/purview's
    `token == r.Header.Get("Authorization")`, which is a PRESENCE check --
    TrimPrefix having changed nothing means the `Bearer ` prefix was absent --
    and comparing a value against a lookup is dominantly structural here rather
    than a verification.

    The shape the rule is actually for survives both clauses: `mac == expected`
    is identifier-vs-identifier, and `sig != computeMAC(x)` has a
    credential-named function on the other side, so it is still caught.
    """
    lhs_cred = _is_credential_ref(lhs, COMPARE_WORDS)
    rhs_cred = _is_credential_ref(rhs, COMPARE_WORDS)
    if lhs_cred and rhs_cred:
        return True
    if not (lhs_cred or rhs_cred):
        return False
    return not (_CALL_HEAD.match(lhs.strip()) or _CALL_HEAD.match(rhs.strip()))


def findings_go(path, rel=None):
    """Every Go finding in one file."""
    rel = rel if rel is not None else relkey(path)
    text = path.read_text(encoding="utf-8", errors="replace")
    lines = text.splitlines()
    out = []

    def add(idx, rule, message):
        out.append({"file": rel, "line": idx + 1, "symbol": _go_symbol(lines, idx),
                    "rule": rule, "message": message,
                    "snippet": lines[idx].strip()[:160]})

    imports_math_rand = any(_GO_MATHRAND.match(ln) for ln in lines)
    imports_archive = any(_GO_ARCHIVE_IMPORT.match(ln) for ln in lines)
    imports_sql = any(_GO_SQL_IMPORT.match(ln) for ln in lines)

    for i, line in enumerate(lines):
        stripped = line.strip()
        if stripped.startswith("//"):
            continue

        if _GO_INSECURE.search(line):
            add(i, "tls-verification-off",
                "certificate verification is disabled for this client")

        # Judged on what the randomness BECOMES -- the assignment target, or the
        # function it is produced inside -- exactly like the Python side. A
        # file-level "does this file mention a key" test is what reported four
        # seeded fixture builders in examples/contoso-fixtures*, where the word
        # was describing a primary key.
        if imports_math_rand and re.search(r"\brand\.\w+", line):
            target = re.match(r"\s*([A-Za-z_][\w.]*)\s*(?::?=|=)", line)
            if (target and _is_credential_ref(target.group(1))) or \
                    _is_credential_ref(_go_symbol(lines, i)):
                add(i, "weak-random-for-secrets",
                    "math/rand produces security material -- it is seeded "
                    "predictably, so use crypto/rand")

        if _WORLD_WRITABLE.search(line) and _GO_MODE.search(line):
            add(i, "world-writable-mode",
                "a world-writable mode is passed to a file or directory create")

        # The archive rule, Go side. Judged on the FUNCTION rather than the
        # line, because the join and the check that makes it safe are
        # necessarily different statements -- and on nothing at all unless the
        # file imports an archive reader, so a `filepath.Join(dir, f.Name)`
        # over an os.ReadDir listing (a name this process wrote) is not read as
        # an archive escape.
        if imports_archive and _GO_ARCHIVE_JOIN.search(line) and \
                not any(_GO_CONTAINED.search(ln) for ln in _go_func_span(lines, i)):
            add(i, "archive-extraction-unchecked",
                "an archive header's name is joined onto a destination with no "
                "containment check -- filepath.Join CLEANS the result, so "
                "`../../x` becomes a valid path outside the destination")

        # R3, both spellings. EqualFold first so a match is not also read as a
        # plain `==` on the same line.
        m = _GO_EQUALFOLD.search(line)
        if m:
            args = _split_args(_balanced(line, m.end() - 1)[0])
            if len(args) == 2 and all(not _literal(a) for a in args) and \
                    _compares_secrets(*args):
                add(i, "secret-compared-directly",
                    "a secret is compared with strings.EqualFold")
        else:
            for lhs, _op, rhs in _GO_CMP.findall(line):
                if _literal(lhs) or _literal(rhs):
                    continue
                if _compares_secrets(lhs, rhs):
                    add(i, "secret-compared-directly",
                        f"a secret is compared with == / != ({lhs} vs {rhs})")
                    break

    # R4 over the whole file text, because a log call's arguments routinely wrap
    # onto the next line and a per-line scan would read half an argument list.
    for m in _GO_LOGCALL.finditer(text):
        inner, _ = _balanced(text, m.end() - 1)
        args = _split_args(inner)
        # THE FORMAT STRING IS SKIPPED. See the module docstring: the message is
        # where the word "token" legitimately appears, and scanning it reports
        # every ErrBadToken wrap and every parser's "unexpected token %q".
        for arg in args[1:]:
            if _is_credential_ref(arg):
                idx = text.count("\n", 0, m.start())
                add(idx, "credential-in-message",
                    f"the credential value `{arg}` is passed to a log or error")
                break

    # R8 over the whole file TEXT, not per line, and that is not a detail: the
    # statements here are raw backtick strings spanning five lines
    # (internal/tds.EnsurePrincipal's CREATE USER / ALTER ROLE block), so a
    # per-line scan would read a fragment and find no verb in it. `_balanced`
    # is quote-aware and counts backticks, which is what makes the multi-line
    # case work at all.
    #
    # ONE REPORT PER SYMBOL, like the Python half: revokePrincipal builds its
    # statement with an Fprintf inside a loop, and the ledger key has always
    # been (file, rule, symbol).
    seen_sql = set()
    for pattern, needs_concat in (((_GO_SQL_FORMAT, False),
                                   (_GO_SQL_WRITESTRING, True))
                                  if imports_sql else ()):
        for m in pattern.finditer(text):
            inner, _ = _balanced(text, m.end() - 1)
            if needs_concat and "+" not in inner:
                continue
            args = _split_args(inner)
            fmt = next((a for a in args if a[:1] in ('"', "`")), None)
            if fmt is None or not _GO_SQL_VERB.search(fmt):
                continue
            # A format string with no substitution and no concatenation builds
            # nothing: `fmt.Fprintf(&b, "COMMIT;")` is a literal with a comma
            # in the wrong place, not an interpolation.
            if len(args) < 2 and "+" not in inner:
                continue
            idx = text.count("\n", 0, m.start())
            sym = _go_symbol(lines, idx)
            if sym in seen_sql:
                continue
            span = _go_func_span(lines, idx)
            if not any(_GO_SQL_EXEC.search(ln) for ln in span):
                continue
            seen_sql.add(sym)
            add(idx, "sql-identifier-interpolated",
                "SQL text is assembled by interpolation in a function that "
                "executes it -- a name spliced into an identifier is SQL "
                "syntax, so the quote character inside it must be doubled")
    return out


# --- Python -----------------------------------------------------------------
# Python ships a parser, so these rules are decided on the syntax tree rather
# than by regex -- the same split the Go and Python flakiness checkers make, and
# for the same reason: `shell=True` as a keyword argument, an f-string's
# interpolated parts, and "is this eval's argument a literal" are all structural
# questions that a line-oriented scan answers only by accident.
_PY_LOG_FUNCS = {"debug", "info", "warning", "warn", "error", "exception",
                 "critical", "log", "print"}
_PY_SUBPROCESS = {"run", "call", "check_call", "check_output", "Popen"}


def _py_symbol_map(tree, total_lines):
    """line number -> enclosing def/class name, for the ledger key."""
    table = {}
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            end = getattr(node, "end_lineno", None) or total_lines
            for ln in range(node.lineno, end + 1):
                # Innermost wins: a nested def is written after its parent in
                # this walk order only by luck, so prefer the tighter range.
                prev = table.get(ln)
                if prev is None or (end - node.lineno) < prev[1]:
                    table[ln] = (node.name, end - node.lineno)
    return {ln: name for ln, (name, _span) in table.items()}


def _py_unparse(node):
    try:
        return ast.unparse(node)
    except Exception:  # pragma: no cover - ast.unparse is total on parsed trees
        return "<expr>"


def _py_is_false(node):
    return isinstance(node, ast.Constant) and node.value is False


def _py_builds_string(node):
    """A command string built by interpolation or concatenation.

    An f-string or a `+` is what turns caller-controlled data into shell syntax;
    a plain literal cannot.
    """
    if isinstance(node, ast.JoinedStr):
        return any(isinstance(v, ast.FormattedValue) for v in node.values)
    if isinstance(node, ast.BinOp) and isinstance(node.op, (ast.Add, ast.Mod)):
        return True
    return (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
            and node.func.attr in ("format", "join"))


# --- R1's `ssl` dialect ----------------------------------------------------
# requests' `verify=False` is one spelling of "do not authenticate the peer";
# the stdlib's is another, and it is the one this repository actually reaches
# for in its own Python. Decided on the AST rather than by regex for the same
# reason every rule below is: `ctx.verify_mode = ssl.CERT_NONE` is an
# assignment whose TARGET and VALUE both matter, and a line-oriented scan
# cannot tell it from a line that merely mentions CERT_NONE in a comment.
_SSL_OFF_ATTRS = ("verify_mode", "check_hostname")


def _py_ssl_skips(tree):
    """Sites where the stdlib `ssl` dialect turns peer verification off.

    Four spellings, all of which make the connection unauthenticated:

      * `<anything>.verify_mode = ssl.CERT_NONE` -- the certificate is not
        checked against any trust store at all.
      * `<anything>.check_hostname = False` -- a certificate is still required
        but need not be FOR the host being talked to, which is the half that
        makes a substituted peer work.
      * `ssl._create_unverified_context(...)` -- both of the above in one call.
      * an assignment to `ssl._create_default_https_context`, which redirects
        every later urllib HTTPS call in the process.

    PAIRS COLLAPSE TO ONE REPORT. The idiom in this tree is the two
    assignments together, three lines apart, and reporting both says the same
    thing twice -- so `check_hostname = False` is suppressed when the same
    enclosing symbol also sets `verify_mode = ssl.CERT_NONE`. The ledger key is
    (file, rule, symbol), so a pair was always ONE entry; this is about the
    report a human reads.

    Yields (node, message) rather than adding directly, so `findings_py` keeps
    one `add` and one symbol table.
    """
    # Pass one: which enclosing symbols turn verification off outright. Keyed
    # on the line so the caller's symbol map resolves it the same way it
    # resolves everything else.
    verify_off_lines = set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.Assign):
            continue
        for target in node.targets:
            if isinstance(target, ast.Attribute) and target.attr == "verify_mode" \
                    and _py_unparse(node.value) in ("ssl.CERT_NONE", "CERT_NONE"):
                verify_off_lines.add(node.lineno)
    out = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Assign):
            for target in node.targets:
                # Checked FIRST and on the unparse, because this target is
                # itself an Attribute (`ssl` . `_create_default_https_context`)
                # and would otherwise fall through the two attr tests below and
                # match neither -- which is exactly what the first version of
                # this did, and the test for this spelling is what caught it.
                # The right-hand side is a reference rather than a call, so the
                # Call clause further down does not see it either.
                if _py_unparse(target) == "ssl._create_default_https_context":
                    out.append((node, verify_off_lines,
                                "assigning ssl._create_default_https_context "
                                "disables certificate verification for every "
                                "later urllib HTTPS call in this process"))
                    continue
                if not isinstance(target, ast.Attribute):
                    continue
                if target.attr == "verify_mode" and \
                        _py_unparse(node.value) in ("ssl.CERT_NONE", "CERT_NONE"):
                    out.append((node, verify_off_lines,
                                "`verify_mode = ssl.CERT_NONE` disables "
                                "certificate verification"))
                elif target.attr == "check_hostname" and _py_is_false(node.value):
                    out.append((node, verify_off_lines,
                                "`check_hostname = False` accepts a certificate "
                                "issued for a different host"))
        elif isinstance(node, ast.Call) and \
                _py_unparse(node.func).endswith("ssl._create_unverified_context"):
            out.append((node, verify_off_lines,
                        "`ssl._create_unverified_context()` disables certificate "
                        "verification"))
    return out


# --- the archive rule ------------------------------------------------------
# SECURITY.md puts "escape from the emulator to the host" in scope, and
# R5/R6 already serve that bullet. This is the third shape of it: an archive
# member's path is chosen by whoever built the archive, so extracting one
# without checking where it lands writes outside the destination directory.
_PY_ARCHIVE_MODULES = ("zipfile", "tarfile")


def _py_imports_archive(tree):
    """Does this file import zipfile or tarfile?

    Gates the `.extract(` clause below and nothing else. `extractall` and
    `shutil.unpack_archive` are specific enough to stand alone, but `.extract`
    is a name anything may have -- this repository's own vendored-table parser
    has `v.extract(page(...))`, which is HTML scraping -- so that clause asks
    for an archive module in the file before it fires. Measured: without the
    gate the rule reported a documentation scraper as a path traversal.
    """
    for node in ast.walk(tree):
        if isinstance(node, ast.Import) and any(
                a.name.split(".")[0] in _PY_ARCHIVE_MODULES for a in node.names):
            return True
        if isinstance(node, ast.ImportFrom) and node.module \
                and node.module.split(".")[0] in _PY_ARCHIVE_MODULES:
            return True
    return False


# --- R8: SQL built by interpolation ----------------------------------------
# SECURITY.md puts T-SQL input reaching the host in scope, and "authorization
# logic that is wrong rather than absent" alongside it. This rule serves both:
# a NAME spliced into SQL text is SQL SYNTAX, not a value, so a backtick in a
# Spark identifier closes the quoting and the rest of the name becomes
# statement -- and one of this repository's sites is OneLake row/column
# security installing its own enforcement view.
#
# IT IS NOT R5. R5's Python-shell clause stops at `eval`/`exec`/`yaml.load` and
# a command string with a shell behind it; a SQL string handed to an engine is
# a different interpreter with a different fix, and R5 never looked at one.
#
# THE MEASURED ASYMMETRY IS THE POINT, and it is why this rule reports a shape
# rather than banning it. The GO side of this tree is already correct:
# internal/tds.principalName doubles `]` -> `]]` for a bracket-quoted
# identifier, `quotedID` doubles `'` -> `''` for an N'...' literal, and
# internal/server's sqlIdent/sqlLiteral do the same one package over. The
# PYTHON side escaped nothing at all -- not one backtick is doubled anywhere in
# python/spark_agent -- so the Go sites are ledgered as a regression guard over
# code that is right, and the Python sites were repaired.
_PY_SQL_METHODS = ("sql", "execute", "executemany", "executescript")

# THE ESCAPE THE RULE RECOGNISES, and recognising one is not optional. A rule
# that reported an escaped site exactly as loudly as a raw one would say
# nothing about safety: every repaired site would need a ledger entry, and
# then DELETING the `quote_ident` call would leave that entry valid and the
# build green -- a guard that has stopped guarding and still reports success,
# which is the failure this whole directory is written against. So an escaped
# interpolation is not a finding, and an unescaped one is; removing an escape
# turns its site back into a finding and fails the build. That is the ratchet.
#
# It TRUSTS THE NAMED HELPER rather than re-deriving the escape, exactly as
# R7 trusts `filter="data"` instead of reimplementing tarfile's containment.
# What makes that sound is that the helper's own behaviour is pinned by a name
# table driven through BOTH the helper and the real call paths
# (python/tests/test_spark_agent_sql_ident.py) -- the lesson the tls_scope
# follow-up paid for, where a boundary nothing asserted was wrong in the
# permissive direction for two releases.
#
# THE GO HALF HAS NO EQUIVALENT, deliberately and statedly: it is regex and
# brace-scanning rather than a parser, so it sees a FUNCTION and not the
# expression substituted into a format string. Its sites are therefore
# ledgered as regression guards over code that is already correct, which is
# also why the Go entries in docs/security-footguns.json read as "this is
# right and must stay right" instead of "this is allowed".
_PY_SQL_ESCAPES = ("quote_ident", "quote_literal", "quote_name")

# THE VERB GATE IS WHAT KEEPS THIS FROM BEING A RULE ABOUT f-STRINGS. Without
# it, every `f"{x}"` reaching any method called `execute` is a finding. The
# list is deliberately the DDL/DML vocabulary and nothing else: this tree's
# engine-matrix probes run `spark.sql(f"OPTIMIZE delta.`{p}`")` and
# `VACUUM delta.`{p}`` over a local path variable, which are maintenance
# commands over a URI rather than an identifier spliced into a relation, and
# including those verbs would have put 6 harness probes in the report to say
# nothing new.
_SQL_VERB = re.compile(
    r"^\s*(?:--[^\n]*\n|/\*.*?\*/|\s)*"
    r"(select|insert|update|delete|create|drop|alter|grant|revoke|show"
    r"|merge|truncate|with|use|set)\b", re.I | re.S)


def _py_template_text(node):
    """The literal text of a string built by interpolation, holes blanked.

    Every substituted part becomes a space, so the VERB test below reads only
    text the author wrote. Reading an interpolated part as text would let
    `f"{verb} FROM t"` pass the gate on a word the author never typed.
    """
    if isinstance(node, ast.Constant):
        return node.value if isinstance(node.value, str) else ""
    if isinstance(node, ast.JoinedStr):
        return "".join(
            v.value if isinstance(v, ast.Constant) and isinstance(v.value, str)
            else " " for v in node.values)
    if isinstance(node, ast.BinOp) and isinstance(node.op, (ast.Add, ast.Mod)):
        return _py_template_text(node.left)
    if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) \
            and node.func.attr == "format":
        return _py_template_text(node.func.value)
    return ""


def _py_interpolated_parts(node):
    """The expressions substituted into a built string, or None if it is not one.

    None and [] are different answers: None means "a plain literal, nothing was
    substituted" and [] means "built, but every hole was constant-folded away".
    Only the first is safe by construction.
    """
    if isinstance(node, ast.JoinedStr):
        return [v.value for v in node.values if isinstance(v, ast.FormattedValue)]
    if isinstance(node, ast.BinOp) and isinstance(node.op, (ast.Add, ast.Mod)):
        right = node.right
        parts = (list(right.elts) if isinstance(right, (ast.Tuple, ast.List))
                 else [right])
        left = _py_interpolated_parts(node.left)
        return parts + (left or [])
    if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) \
            and node.func.attr == "format":
        return list(node.args) + [kw.value for kw in node.keywords]
    return None


def _py_module_constants(tree):
    """Names bound once at MODULE level -- not inside any def or class.

    The narrowing this exists for, measured: most e2e probes are
    `s.sql(f"DROP VIEW IF EXISTS {VIEW}")` over a file-local constant
    (e2e/sail-session-isolation/probe.py). A value this file wrote into a
    module global is not caller-controlled, and reporting it would put a dozen
    harness probes in front of the three sites that decide who sees which rows.
    """
    names = set()
    for node in tree.body:
        if isinstance(node, ast.Assign):
            for t in node.targets:
                if isinstance(t, ast.Name):
                    names.add(t.id)
        elif isinstance(node, (ast.AnnAssign, ast.AugAssign)) and \
                isinstance(node.target, ast.Name):
            names.add(node.target.id)
    return names


def _py_is_escaped(node):
    """Is this substituted expression the output of a known escape helper?

    Decided on the CALLED FUNCTION, like `_is_credential_ref`: what a call
    evaluates to is described by its function and not by its arguments, so
    `quote_ident(row[1])` is escaped however the name got there.
    """
    if not isinstance(node, ast.Call):
        return False
    return _py_unparse(node.func).split(".")[-1] in _PY_SQL_ESCAPES


def _py_sql_aliases(tree):
    """Bare names this file binds to a SQL-execution method, e.g. `spark.sql`.

    THE THIRD BLIND SPOT THIS RULE SHIPPED WITH, and the one that cost a real
    finding rather than a hypothetical one. The clause below wants a METHOD for
    a measured reason -- `execute` is a name anything may have -- and that
    reasoning does not reach a bare name that IS `spark.sql`:

        original_sql = spark.sql                     # delta_ops.py:928
        ...
        execute_ctas(spark, original_sql, params)    # passed as a parameter
        original_sql(f"CREATE TABLE ... LOCATION '{loc}'")  # invisible to R8

    That site is `delta_ops.execute_ctas`; it spliced a catalog-supplied schema
    location into a quoted literal with nothing doubling an apostrophe, and it
    sits in the one package this rule's own scope sentence claims to have
    swept. The same value is escaped by `catalog.register` one module over, so
    without this clause the identical `sloc` was quoted in one statement and
    raw in another -- which is the argument for `quote_literal` existing,
    turned on the change that added it.

    SCOPED TO THIS FILE'S OWN BINDINGS, which is what keeps the method
    requirement's reasoning intact. A bare `execute(x)` is still ignored unless
    the file says somewhere that `execute` is a `.sql`/`.execute` attribute of
    something; the gate is not "a bare call" but "a bare call to a name this
    file aliased onto a SQL executor". Any scope counts, because the binding
    and the call are routinely in different functions -- here they are 400
    lines and one parameter apart -- so a module-scope-only reading (the one
    `_py_module_constants` makes, for its own different purpose) would have
    seen neither.
    """
    names = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Assign):
            targets = node.targets
        elif isinstance(node, ast.AnnAssign):
            targets = [node.target]
        else:
            continue
        value = node.value
        if not (isinstance(value, ast.Attribute) and value.attr in _PY_SQL_METHODS):
            continue
        for t in targets:
            if isinstance(t, ast.Name):
                names.add(t.id)
    return names


def _py_sql_interpolations(tree):
    """Sites where SQL text is assembled from something that is not a literal.

    Yields (node, message). A finding needs all three of:

      * a call to a SQL executor: a method named `.sql` / `.execute` /
        `.executemany` / `.executescript`, or a bare name this file binds to
        one of those (see `_py_sql_aliases`). A METHOD by default, like R7's
        `.extract` clause and for the same measured reason: `execute` is a name
        anything may have, and this repository's own
        scripts/vendor_notebookutils_stubs.py defines a bare `extract(url)`
        that the import gate alone did not save. The alias set is the narrow
        exception, and it is narrow on purpose: a name that IS `spark.sql` is
        the executor rather than a coincidence of spelling.
      * its first argument BUILT -- an f-string, a `%`/`+`, or `.format` --
        rather than a literal. `cur.execute("SELECT x FROM t WHERE id = ?",
        (v,))` is a parameterized query: the literal is fixed and the value is
        bound by the driver, which is the shape this rule wants people to
        write, so it must never be reported.
      * the author-written text starting on a SQL verb (see `_SQL_VERB`).

    ...and then survives the constant narrowing: an interpolation every one of
    whose holes is a literal or a module-level constant is skipped.
    """
    consts = _py_module_constants(tree)
    aliases = _py_sql_aliases(tree)
    out = []
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        func = node.func
        if isinstance(func, ast.Attribute):
            if func.attr not in _PY_SQL_METHODS:
                continue
        elif isinstance(func, ast.Name):
            if func.id not in aliases:
                continue
        else:
            continue
        if not node.args:
            continue
        arg = node.args[0]
        parts = _py_interpolated_parts(arg)
        if parts is None:
            continue
        if not _SQL_VERB.match(_py_template_text(arg) or ""):
            continue
        spliced = [p for p in parts
                   if not (isinstance(p, ast.Constant)
                           or (isinstance(p, ast.Name) and p.id in consts)
                           or _py_is_escaped(p))]
        if not spliced:
            continue
        shown = ", ".join(sorted({_py_unparse(p) for p in spliced}))
        out.append((node,
                    f"SQL text is assembled with `{shown}` spliced into it -- "
                    "a name interpolated into an identifier is SQL syntax, so "
                    "a backtick in it ends the quoting and the rest becomes "
                    "statement"))
    return out


def _py_random_targets(tree):
    """Line numbers where a `random.*` result is bound to a credential.

    WHY THE RULE IS SHAPED THIS WAY. Its first version asked whether the FILE
    mentioned a token, secret, key, nonce or password, and reported four sites in
    examples/contoso-fixtures*: `rnd = random.Random(SEED)`, a deliberately
    SEEDED generator producing the fixture rows an example asserts against, in
    files that say "key" because they describe a primary key. Determinism is the
    point there, `secrets` would break it, and the word was doing no work.

    What matters is what the randomness BECOMES, so that is what is inspected: an
    assignment target named like a credential, or a `return` from a function
    named like one.
    """
    lines = set()

    def uses_random(node):
        return any(isinstance(n, ast.Call) and _py_unparse(n.func).startswith("random.")
                   for n in ast.walk(node))

    for node in ast.walk(tree):
        if isinstance(node, (ast.Assign, ast.AnnAssign, ast.AugAssign)):
            targets = node.targets if isinstance(node, ast.Assign) else [node.target]
            if node.value is not None and uses_random(node.value) and \
                    any(_is_credential_ref(_py_unparse(t)) for t in targets):
                for n in ast.walk(node.value):
                    lines.add(getattr(n, "lineno", node.lineno))
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and \
                _is_credential_ref(node.name):
            for n in ast.walk(node):
                if isinstance(n, ast.Return) and n.value is not None and uses_random(n.value):
                    for sub in ast.walk(n.value):
                        lines.add(getattr(sub, "lineno", n.lineno))
    return lines


def findings_py(path, rel=None):
    """Every Python finding in one file."""
    rel = rel if rel is not None else relkey(path)
    text = path.read_text(encoding="utf-8", errors="replace")
    lines = text.splitlines()
    try:
        tree = ast.parse(text)
    except SyntaxError:
        # A file this interpreter cannot parse is not silently skipped: a rule
        # that inspects nothing passes vacuously, and that is the whole failure
        # this repository's guards are written against.
        return [{"file": rel, "line": 1, "symbol": "<file>", "rule": "unparsed",
                 "message": "this file could not be parsed, so no rule was "
                            "applied to it", "snippet": ""}]
    symbols = _py_symbol_map(tree, len(lines))
    out = []

    def add(node, rule, message):
        ln = getattr(node, "lineno", 1)
        out.append({"file": rel, "line": ln,
                    "symbol": symbols.get(ln, "<module>"),
                    "rule": rule, "message": message,
                    "snippet": (lines[ln - 1].strip()[:160] if 0 < ln <= len(lines) else "")})

    uses_random = any(
        (isinstance(n, ast.Import) and any(a.name == "random" for a in n.names))
        or (isinstance(n, ast.ImportFrom) and n.module == "random")
        for n in ast.walk(tree))
    random_for_secrets = _py_random_targets(tree)
    imports_archive = _py_imports_archive(tree)

    # R1's `ssl` dialect, before the Call walk below -- these are assignments,
    # and the walk's `continue` on a non-Call would skip them. Same rule id as
    # `verify=False`, so every site still has to name `hosts` in the ledger.
    for node, verify_off_lines, message in _py_ssl_skips(tree):
        if "check_hostname" in message:
            # The pair idiom: one context, one line of report. See
            # `_py_ssl_skips`.
            sym = symbols.get(node.lineno, "<module>")
            if any(symbols.get(ln, "<module>") == sym for ln in verify_off_lines):
                continue
        add(node, "tls-verification-off", message)

    # R8, also before the Call walk: these are Calls, but the walk below
    # dispatches on a keyword/name vocabulary this rule does not share, and
    # deciding "is the first argument a built SQL string" needs the whole call
    # rather than its function name. One pass, one `add`, same symbol table.
    #
    # ONE REPORT PER SYMBOL, as the paired `ssl` idiom does: `_sweep_qualified`
    # builds one statement in a loop and `restore` builds two, and three lines
    # saying the same thing about one function makes the report's count stop
    # matching what a reader would count by hand. The ledger key is
    # (file, rule, symbol), so these were always ONE entry.
    seen_sql = set()
    for node, message in _py_sql_interpolations(tree):
        sym = symbols.get(getattr(node, "lineno", 1), "<module>")
        if sym in seen_sql:
            continue
        seen_sql.add(sym)
        add(node, "sql-identifier-interpolated", message)

    for node in ast.walk(tree):
        # R2 -- see `_py_random_targets`: judged on what the randomness is
        # ASSIGNED TO or the function it is returned from, never on words
        # appearing somewhere in the file.
        if uses_random and isinstance(node, ast.Call):
            fn = _py_unparse(node.func)
            if fn.startswith("random.") and (
                    node.lineno in random_for_secrets
                    or _is_credential_ref(symbols.get(node.lineno, ""))):
                add(node, "weak-random-for-secrets",
                    f"`{fn}` produces security material -- seeded predictably, "
                    "so use the secrets module")

        if not isinstance(node, ast.Call):
            continue
        fn = _py_unparse(node.func)
        tail = node.func.attr if isinstance(node.func, ast.Attribute) else fn

        # R1
        for kw in node.keywords:
            if kw.arg in ("verify", "connection_verify") and _py_is_false(kw.value):
                add(node, "tls-verification-off",
                    f"`{kw.arg}=False` disables certificate verification")

        # R5
        for kw in node.keywords:
            if kw.arg == "shell" and kw.value is not None and \
                    isinstance(kw.value, ast.Constant) and kw.value.value is True:
                add(node, "python-shell-injection",
                    "shell=True runs the command through a shell, so any "
                    "interpolated value is shell syntax")
        if fn in ("os.system", "os.popen"):
            built = node.args and _py_builds_string(node.args[0])
            add(node, "python-shell-injection",
                f"`{fn}` always uses a shell"
                + (", and its command is built by interpolation" if built else ""))
        # A BUILT COMMAND IS ONLY AN INJECTION WITH A SHELL BEHIND IT, and this
        # clause exists because the first version of this rule got that wrong:
        # it reported 20 sites across e2e/ of the shape
        # `subprocess.run(cmd + ["up", "--build"])`, which is LIST
        # concatenation with no shell at all -- argv built that way cannot be
        # reinterpreted as syntax, so every one was noise. The dangerous shape
        # needs both halves: a string assembled from data AND something that
        # parses it.
        if tail in _PY_SUBPROCESS and ("subprocess" in fn or fn in _PY_SUBPROCESS) \
                and node.args and _py_builds_string(node.args[0]) \
                and any(kw.arg == "shell" and isinstance(kw.value, ast.Constant)
                        and kw.value.value is True for kw in node.keywords):
            add(node, "python-shell-injection",
                "the shell command is a string built by interpolation or "
                "concatenation -- pass argv as a list with no shell instead")
        if fn in ("yaml.load",) and not any(
                kw.arg == "Loader" for kw in node.keywords) and len(node.args) < 2:
            add(node, "python-shell-injection",
                "yaml.load with no Loader constructs arbitrary Python objects "
                "-- use yaml.safe_load")
        if fn in ("eval", "exec") and node.args and \
                not isinstance(node.args[0], ast.Constant):
            add(node, "python-shell-injection",
                f"`{fn}` over a non-literal executes whatever produced it")

        # The archive rule. `extractall` with NO `filter=` and no explicit
        # `members=` extracts every member at the path the member names, so a
        # crafted `../../` component writes outside the destination. Python
        # 3.14 makes `filter="data"` the default; this repository's floor is
        # 3.12, where the default is still the unsafe one -- which is exactly
        # why the shape has to be checked rather than assumed away.
        #
        # `members=` counts as a bound because supplying one means the caller
        # built the list, so the names went through their own code rather than
        # straight from the archive.
        if tail == "extractall" and not any(
                kw.arg in ("filter", "members") for kw in node.keywords):
            add(node, "archive-extraction-unchecked",
                "`extractall` with no filter= writes each member wherever "
                "the member's own name points, including outside the "
                "destination directory")
        if fn in ("shutil.unpack_archive",):
            add(node, "archive-extraction-unchecked",
                "`shutil.unpack_archive` extracts without validating member "
                "paths -- it has no filter= of its own")
        # A METHOD call only, and only in a file that imports an archive
        # module. BOTH clauses are measured corrections, from the two false
        # positives this clause produced against this tree on its first run:
        #
        #   * `v.extract(page(enough()))` -- the vendored-activity-types
        #     parser, which extracts a TABLE from HTML. Hence the import gate.
        #   * `files = extract(url)` in scripts/vendor_notebookutils_stubs.py,
        #     which imports zipfile AND defines its own `extract(url)`, so the
        #     import gate alone did not save it. That function reads members
        #     into a dict with `wheel.read(name)` and writes no member-named
        #     path at all. Hence the Attribute clause: a member extraction is
        #     always a method ON an archive object, never a bare call.
        if imports_archive and tail == "extract" \
                and isinstance(node.func, ast.Attribute) and node.args and \
                not isinstance(node.args[0], ast.Constant):
            add(node, "archive-extraction-unchecked",
                "`extract` of a member named by the archive rather than by a "
                "literal -- validate the name against the extraction root")

        # R6
        if fn in ("os.chmod", "os.fchmod", "os.lchmod", "path.chmod") or tail == "chmod":
            for arg in node.args:
                if isinstance(arg, ast.Constant) and isinstance(arg.value, int) \
                        and arg.value in (0o777, 0o666):
                    add(node, "world-writable-mode",
                        f"mode {oct(arg.value)} is writable by every local user")

        # R4 -- arguments after the first, exactly as on the Go side, plus the
        # interpolated parts of an f-string message (Python's own format-string
        # shape: `logging.info(f"token {tok}")` puts the VALUE in the message).
        if tail in _PY_LOG_FUNCS or fn.endswith("Error") or tail == "exception":
            for arg in node.args[1:]:
                if _is_credential_ref(_py_unparse(arg)):
                    add(node, "credential-in-message",
                        f"the credential value `{_py_unparse(arg)}` is passed "
                        "to a log")
                    break
            for arg in node.args:
                if isinstance(arg, ast.JoinedStr):
                    for part in arg.values:
                        if isinstance(part, ast.FormattedValue) and \
                                _is_credential_ref(_py_unparse(part.value)):
                            add(node, "credential-in-message",
                                f"the credential value "
                                f"`{_py_unparse(part.value)}` is interpolated "
                                "into a log message")
                            break
    return out


def scan(files=None):
    """Every finding across the tree."""
    out = []
    for path in (files if files is not None else source_files()):
        out.extend(findings_go(path) if path.suffix == ".go" else findings_py(path))
    return out


def ledger_key(item):
    """The ledger key for a finding or an accepted entry: file, rule AND symbol.

    Keyed on the enclosing symbol rather than the line number, like every
    sibling ledger here: a line number goes stale on any edit above it, and a
    ledger that must be renumbered to stay valid is a ledger people delete
    entries from instead of updating.

    The RULE is part of the key so an accepted `tls-verification-off` in a
    function does not also exempt a `credential-in-message` added to that same
    function later -- an entry without it would silently widen what was actually
    reviewed, which is the failure the flakiness ledgers' three-part keys exist
    to prevent.
    """
    missing = [k for k in ("file", "rule", "symbol") if not item.get(k)]
    if missing:
        raise KeyError(
            f"a security-footgun entry is missing {', '.join(missing)}: {item!r}. "
            "Every entry needs file, rule and symbol -- the rule is what scopes "
            "the exemption to one shape, so an entry without one would exempt "
            "the symbol from every rule.")
    return f"{item['file']}:{item['rule']}:{item['symbol']}"


def load_ledger(ledger=None):
    """The accepted sites, keyed by `ledger_key`.

    A `tls-verification-off` entry must also name `hosts`: the host set that
    client can reach is the fact that decides whether the skip is safe, and it
    is precisely what was missing when internal/akv carried a transport-wide
    skip on the one client whose allowlist admits a real Azure vault. An entry
    without it would record the site without recording the thing worth
    reviewing.
    """
    path = LEDGER if ledger is None else ledger
    if not path.exists():
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    out = {}
    for entry in data.get("accepted", []):
        key = ledger_key(entry)
        if not entry.get("why"):
            raise KeyError(
                f"the security-footgun entry {key} has no `why`. An accepted "
                "site with no reason is indistinguishable from one nobody "
                "looked at.")
        if entry["rule"] == "tls-verification-off" and not entry.get("hosts"):
            raise KeyError(
                f"the security-footgun entry {key} has no `hosts`. A TLS "
                "verification skip is only reviewable against the set of hosts "
                "that client can reach -- naming them is what distinguishes a "
                "compose sibling's self-signed cert from a real Azure vault.")
        if entry["rule"] == "sql-identifier-interpolated" and not entry.get("values"):
            raise KeyError(
                f"the security-footgun entry {key} has no `values`. The same "
                "requirement as `hosts` one rule over, and for the same "
                "reason: what decides whether spliced SQL is safe is WHAT is "
                "interpolated and WHAT escapes it, so an entry that does not "
                "name both records the site without recording the thing worth "
                "reviewing. For the Go sites it is the only place the escape "
                "is written down at all, because that half of the rule reads "
                "the enclosing function rather than the argument.")
        out[key] = entry
    return out


def main(argv):
    strict = "--strict" in argv[1:]

    files = source_files()

    # A sweep that walked nothing reports success. The same guard every sibling
    # carries, for the same reason: a check that inspects nothing passes for the
    # wrong reason, and it passes quietly.
    if not files:
        print("check_security_footguns: walked zero .go/.py source files - a "
              "check that inspects nothing passes vacuously")
        return 1

    found = scan(files)
    accepted = load_ledger()

    unrecorded, matched = [], set()
    for f in found:
        key = ledger_key(f)
        if key in accepted:
            matched.add(key)
        else:
            unrecorded.append(f)

    stale = sorted(set(accepted) - matched)

    if not strict:
        for f in found:
            mark = "accepted" if ledger_key(f) in accepted else "NEW"
            print(f"{mark:9} {f['file']}:{f['line']} {f['symbol']} "
                  f"[{f['rule']}] {f['message']}")
        for key in stale:
            print(f"{'STALE':9} {key} - recorded, but no longer found")
        by_rule = {}
        for f in found:
            by_rule[f["rule"]] = by_rule.get(f["rule"], 0) + 1
        print(f"\ncheck_security_footguns: {len(files)} source files, "
              f"{len(found)} finding(s) "
              f"({', '.join(f'{k}={v}' for k, v in sorted(by_rule.items())) or 'none'}), "
              f"{len(found) - len(unrecorded)} accepted, "
              f"{len(unrecorded)} not recorded, "
              f"{len(stale)} ledger entr(y/ies) stale")
        return 0

    problems = []
    if unrecorded:
        groups = {}
        for f in unrecorded:
            groups.setdefault(f["rule"], []).append(f)
        for rule, items in sorted(groups.items()):
            listing = "\n    ".join(
                f"{f['file']}:{f['line']} {f['symbol']}: {f['message']}"
                for f in items)
            problems.append(
                f"{len(items)} unrecorded {rule} finding(s):\n    {listing}\n"
                f"  {REMEDY.get(rule, 'Fix the site, or record it in ' + relkey(LEDGER) + ' with the reason.')}")
    if stale:
        problems.append(
            f"{len(stale)} ledger entr(y/ies) name a site that is no longer "
            "found; a stale allowance goes on excusing the file, and would "
            "silently re-cover the shape if it came back:\n    "
            + "\n    ".join(stale))

    if problems:
        print("check_security_footguns: " + "\n\n".join(problems))
        return 1

    print(f"check_security_footguns: {len(files)} source files scanned; "
          f"{len(found)} finding(s), all {len(accepted)} recorded in "
          f"{relkey(LEDGER)} with a reason")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
