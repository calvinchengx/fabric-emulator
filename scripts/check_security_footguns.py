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
rather than absent (R1, R3), escape from the emulator to the host (R5, R6).

WHAT IS FLAGGED, and what each rule deliberately does NOT flag:

  * `tls-verification-off` (R1) -- `InsecureSkipVerify: true`, `verify=False`,
    `connection_verify=False`. Every site is LEDGERED rather than banned,
    because a local emulator talking to a sibling's self-signed cert is the
    normal case here. The ledger entry must name the HOST SET that client can
    reach, which is the thing that actually decides whether the skip is safe:
    six of the seven reach a compose service and nothing else, and the seventh
    (internal/akv) reaches real `*.vault.azure.net` by design, which is why its
    skip is now scoped to one configured host instead of the transport. A new
    unledgered site fails, so that distinction cannot be lost by default.
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

THE MEASURED BASELINE AT LANDING: 37 findings across 551 non-test source files,
in 30 distinct (file, rule, symbol) keys, every one recorded in the ledger with
its reason -- 13 tls-verification-off, 13 python-shell-injection, 10
world-writable-mode, 1 credential-in-message, and zero for both regression-guard
rules. One further finding is NOT in the ledger because it was FIXED in the same
change: internal/akv set InsecureSkipVerify transport-wide from a flag documented
for entra-emulator's certificate, on the one client whose allowlist deliberately
admits a real `*.vault.azure.net`. See docs/63-security-footguns.md.

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
