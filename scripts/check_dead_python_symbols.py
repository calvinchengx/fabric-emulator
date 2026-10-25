#!/usr/bin/env python3
"""Every module-level name in scripts/ and python/ is reached from somewhere.

WHY IT EXISTS. Nothing in this tree asks whether a Python name at MODULE scope
is still read. ruff runs over all of it (`make lint`, and the `ruff + ty` CI
job) and its `F` family answers a narrower question than people assume: F401 is
an unused IMPORT, F841 an unused LOCAL variable. Neither has any opinion about
a top-level `def`, `class` or constant that nothing references -- F811 fires
only on a REDEFINITION, and the one rule that would come close, vulture-style
dead-code detection, is not a ruff rule at all and is not in pyproject.toml's
`select`. So a module-level symbol that stops being read keeps importing, keeps
type-checking under `ty`, keeps passing pytest and keeps shipping.

The other two languages here are already covered, which is what made Python the
gap rather than the obvious place to look. `.golangci.yml` enables `unused`
(staticcheck's U1000 family), so an unreferenced Go identifier is a build
failure today -- and a planning scan over every exported Go identifier found
zero orphans, because that linter has been holding the line the whole time. On
the TypeScript side, `scripts/check_portal_components.py` now holds the
vendored `ui/` directories and `pnpm --filter fabric-emulator-portal check`
holds the rest; a sweep of every export under portal/src and website/src came
back empty too. Python module scope was the one surface with nobody asking.

WHAT WAS MEASURED, at the commit that added this. 107 in-scope files carrying
1538 module-level symbols, of which exactly THREE were reached by nothing:

  * `python/spark_agent/usercontext.py` `_KEEP` -- a one-element tuple of
    environment names, with a two-sentence comment above it describing an
    ALLOW list. `child_env` has never had one: it copies the environment and
    pops `CREDENTIAL_ENV` from it, which is a DENY list, and its own docstring
    says so and argues for it at length. So the comment documented behaviour
    the code never had, which is worse than the dead tuple underneath it.
  * `scripts/check_security_footguns.py` `_SSL_OFF_ATTRS` -- a pair of
    attribute names superseded by `_py_ssl_skips`, whose docstring enumerates
    the four spellings it matches straight off the AST.
  * `examples/contoso-fixtures/source_system.py` `ORDER_FIELDS` -- a second,
    silently drifting statement of the bronze_orders schema; the generator
    writes the row as a dict literal ~150 lines below it. Removed BY HAND,
    because it is outside this guard's scope -- see the next paragraph for why
    the scope stops where it does.

WHY THE SCOPE IS scripts/ AND python/ AND NOT EVERY PYTHON ROOT. `e2e/` holds
the counter-example that sets the boundary. `e2e/xmla/run.py` defines
`_shapes_screen5`, `_shapes_screen6` and `_shapes_screen7`, none of them
referenced anywhere, and all three say "Kept for the record" / "For the
record" in their own docstrings: they are an ARCHIVED PROBE of a client's
`capacityUri` handling, deliberately retained as the written record of what was
measured. Deleting them would destroy evidence, and recording three exemptions
would be a ledger apologising for a directory this check should not be reading.
`FORWARDING_443` in the same file is the other shape -- it binds a
side-effecting `start_443_forwarder()` call, so the NAME is incidental and the
assignment is the point. Harness and example code is written to be read and
kept; `scripts/` and `python/` are written to be CALLED, and an uncalled name
there is dead rather than archival.

MENTION-ANYWHERE, GENEROUSLY, ON PURPOSE. A name counts as reached if its
identifier appears anywhere in any text file in the tree outside its own
definition -- a Makefile recipe, a CI job, a docstring, a markdown doc, a
string literal, a JSON ledger. That is far weaker than a real reachability
analysis and it is the right weakness: a finding is then never arguable. If
this reports a name, there is NOTHING anywhere in the repository that so much
as spells it, and the conversation is over in one grep. The stricter reading
-- imports resolved, call graph walked -- would produce findings someone has to
litigate, and `scripts/check_perf_regressions.py` records at length what
happens to a guard whose output gets argued with instead of fixed
(`defer-in-loop`, one false positive, dropped before it shipped).

PROSE DOES NOT VOUCH FOR A NAME, and this is the defect this checker shipped
with rather than a tidiness measure. The house style here requires a guard to
state its measured baseline, and a baseline NAMES the symbols it found -- so
the paragraph above spells `_KEEP`, `_SSL_OFF_ATTRS` and `ORDER_FIELDS`, and
under a mention-anywhere reading that paragraph is a second occurrence of each.
Measured: with this file counted as prose, the first run over the unrepaired
tree reported 0 orphans and exit 0, having laundered all three of the symbols
it was written to find.

THE FIRST REPAIR WAS A THIRD OF ONE, which is the more useful half of this
history. It excluded this file and stopped -- but the style that creates the
duplication had already copied that same baseline paragraph into the `make
check` recipe, into the CI job comment above the step, and into this module's
own test. All three were still counted, so re-adding `_KEEP` to
python/spark_agent/usercontext.py left the guard at exit 0 while a freshly
invented name in the same file was correctly reported. The exclusion has to
follow the STYLE that duplicates a finding, not the one file that happened to
be noticed, or every future finding is laundered the same way.

So the rule is that a comment is not a call, and it is applied by file kind
(`vouching_identifiers`):

  * A `#`-comment file -- Makefile, YAML, TOML, shell -- contributes
    everything OUTSIDE its comments (`strip_hash_comments`). That is what
    reaches the two wiring files.
  * ANY Python file contributes its identifiers with `#` comments dropped, not
    merely the prose-blind ones below. The shape this closes is a comment
    naming the constant directly beneath it -- `# _KEEP is the allow list`
    over `_KEEP = (...)` -- which is ordinary Python style and is a second
    occurrence in the dead symbol's OWN file, enough to launder it
    single-handedly. This tree escaped that only because the comment above
    `_KEEP` described the tuple without spelling it.
  * A guard and its test -- `scripts/check_*.py` and
    `python/tests/test_check_*.py` (`prose_blind`) -- contribute their NAME
    tokens alone, so docstrings are dropped too. These are exactly the modules
    the style makes restate a finding, and the modules whose JOB is to talk
    about dead code.

Their CODE still counts, and that distinction is the repair's own first
version: skipping a file outright stopped its internal calls counting too, so
the nine helpers this module invokes from inside itself (`text_files`,
`module_symbols`, ...) every one reported as an orphan.

WHAT THIS DOES NOT CLAIM, so nobody reads more into a green run than is there:

  * That a REFERENCED name is actually live. It may be reached only from code
    that is itself dead -- exactly the `select/` -> `separator/` shape
    check_portal_components.py was written about, where dead code held dead
    code alive and a mention-anywhere sweep called it healthy. This check
    CANNOT see that, which is why that one reads reachability from the
    application instead. A text sweep catches the leaves of the dead region,
    not the region.
  * That a name mentioned only in prose is used. A markdown doc, a JSON
    ledger or a release note naming a deleted constant keeps it green here.
    Comments no longer do, and neither does a guard's own docstring -- see the
    section above -- but documentation at large still vouches, because that is
    where this tree's real cross-references live and narrowing it would start
    producing findings someone has to argue with.
  * Anything about names below module scope. A dead method on a live class, or
    a dead branch inside a live function, is invisible to this.

THE EXEMPTIONS, each with the reason and the measured cost of not having it:

  * `test_*.py`, `*_test.py`, `conftest.py` -- pytest DISCOVERS by name and
    injects fixtures by name, so a test function and a fixture are called by
    the runner and referenced by nobody. Measured: 2190 would-be findings
    without this (2143 in python/tests, 32 in python/fabric-target/tests, 15
    in python/fabric-target/conformance). It is by far the load-bearing one.
  * `__init__.py` -- a barrel publishes a surface whether or not today's
    callers use all of it. The same directory-vs-file reasoning
    check_portal_components.py records for `card-action.svelte`: flagging a
    re-export produces findings nobody should act on.
  * DECORATED top-level defs -- a fixture, a registration or a
    `@contextmanager` is reached through its decorator rather than by its name.
  * dunders, and `main` -- `__getattr__`, `__all__` and a `main` reached by
    `sys.exit(main(sys.argv))` are called by protocol, not by reference.
  * names listed in a module's own `__all__` -- the module has declared the
    name public, which is a statement that it is for callers this tree may not
    contain.
  * `build/` -- NOT a style exemption, and the one that would have silently
    disabled this check over a whole package. `python/build/` is gitignored but
    `python/fabric-target/build/lib/fabric_target/` is TRACKED: a committed
    copy of the source, predating the ignore rule. Left in, every fabric_target
    symbol would appear twice -- once in the source, once in its own build copy
    -- so nothing in that package could ever be reported, and the copy would be
    accepted as a referrer for symbols nothing else reads. Skipped in BOTH
    passes for the same reason check_portal_components.py refuses a referrer
    inside `ui/`: a derived artifact is not a caller.

The last four are at ZERO findings today and are stated as such rather than
implied to be load-bearing: they exist so that the first `@pytest.fixture` in a
non-test module, or the first `__all__` outside a barrel, lands as working code
rather than as a false positive somebody has to argue with.

Zero in-scope files, or zero symbols considered, is ITSELF a finding. A sweep
that inspects nothing passes vacuously, and it passes quietly -- the same guard
every sibling in this directory carries.

Usage:
    check_dead_python_symbols.py            report orphans, exit 0
    check_dead_python_symbols.py --strict   exit non-zero on any orphan
"""
import ast
import io
import pathlib
import re
import sys
import token as _token
import tokenize

ROOT = pathlib.Path(__file__).resolve().parent.parent

# Where a name that is not called is DEAD rather than archival. See the
# docstring's scope paragraph for why e2e/ and examples/ are not here: both
# hold deliberately-retained probes whose own docstrings say so.
SCAN_ROOTS = ("scripts", "python")

# Not walked, in EITHER pass. `.claude` can hold a git WORKTREE -- a second
# checkout of this repository nested inside it -- so a sweep that walked one
# would count a sibling branch's copy of a file as a referrer here, which is
# the same reasoning the three flakiness checkers carry.
#
# `build` is the load-bearing entry rather than tidiness: see the docstring.
SKIP_DIRS = {
    ".git", ".claude", ".venv", "venv", "node_modules", "third_party",
    "build", "dist", "_site", "coverage", "covdata", ".svelte-kit",
    "__pycache__", ".pytest_cache", ".ruff_cache", "vendor",
}

# Paths skipped in BOTH passes, as whole subtrees. portal/dist is a committed
# build artifact (go:embed bakes it into the binary), so a minified bundle
# happening to contain a short identifier is not a reference to it.
SKIP_PREFIXES = ("portal/dist/",)

# This file, named so its own test can point at it. It is the first member of
# the prose-blind family (`prose_blind`) rather than a case of its own.
SELF = "scripts/check_dead_python_symbols.py"

# Files whose comment syntax is `#` -- which is every wiring file this
# repository states a baseline in: the `make check` recipe and the CI job.
_HASH_COMMENT_SUFFIXES = {
    ".yml", ".yaml", ".toml", ".cfg", ".ini", ".sh", ".bash", ".zsh",
    ".mk", ".properties", ".conf", ".env",
}
_HASH_COMMENT_NAMES = {
    "Makefile", "makefile", "GNUmakefile", "Dockerfile", ".gitignore",
    ".dockerignore", ".env",
}

# Python's string tokens. FSTRING_MIDDLE is the literal text between an
# f-string's holes and exists from 3.12; the holes themselves arrive as NAME
# tokens either way, so an interpolated reference counts under both readings.
_STRING_TOKENS = {_token.STRING} | {
    t for t in (getattr(_token, "FSTRING_MIDDLE", None),) if t is not None}

# Generated or binary files a token scan must not read. The lock files are
# machine-written and enormous; a name colliding with a package or a hash
# substring in one is not a referrer.
SKIP_NAMES = {
    "uv.lock", "pnpm-lock.yaml", "package-lock.json", "go.sum",
    "poetry.lock", "Cargo.lock",
}
BINARY_SUFFIXES = {
    ".png", ".jpg", ".jpeg", ".gif", ".ico", ".svg", ".webp", ".pdf",
    ".woff", ".woff2", ".ttf", ".eot", ".zip", ".gz", ".tgz", ".bz2", ".xz",
    ".whl", ".so", ".dylib", ".dll", ".a", ".o", ".exe", ".bin",
    ".parquet", ".db", ".sqlite", ".jar", ".class", ".pyc", ".wasm",
}

# Called by protocol rather than by reference. `main` is reached through
# `sys.exit(main(sys.argv))` in the module's own `__main__` guard, which is a
# self-reference this check would otherwise have to special-case anyway.
PROTOCOL_NAMES = {"main"}

# A Python identifier, which is also how a name is spelled in a Makefile
# recipe, a YAML job, a docstring or a doc. One tokenizer pass over every text
# file, so a mention in any of those counts -- see the module docstring on why
# the reading is deliberately generous.
_TOKEN = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")


def relkey(path, root=None):
    """A path as a message should spell it: relative to the root, forward slashes.

    Same reasoning as every sibling checker's `relkey` -- a raw
    `str(path.relative_to(ROOT))` is backslash-separated on Windows, so one
    finding would read two ways across the three platforms this suite runs on.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    r = ROOT if root is None else root
    r = r if isinstance(r, pathlib.PurePath) else pathlib.PurePath(r)
    return (p.relative_to(r) if p.is_absolute() else p).as_posix()


def _skipped(rel_parts, rel_posix):
    """Is this path in a directory or subtree neither pass may read?"""
    if SKIP_DIRS.intersection(rel_parts):
        return True
    return any(rel_posix.startswith(prefix) for prefix in SKIP_PREFIXES)


def text_files(root=None):
    """Every file in the tree a reference could be written in.

    Deliberately NOT restricted to .py. The whole point of the generous
    reading is that a name mentioned in a Makefile recipe, a CI job, a JSON
    ledger or a markdown doc counts as reached -- this repository wires its
    scripts from the Makefile and ci.yml, so a Python-only sweep would report
    every one of them as an orphan.
    """
    base = ROOT if root is None else pathlib.Path(root)
    out = []
    for path in sorted(base.rglob("*")):
        if not path.is_file():
            continue
        rel = path.relative_to(base)
        if _skipped(set(rel.parts), rel.as_posix()):
            continue
        if path.name in SKIP_NAMES or path.suffix.lower() in BINARY_SUFFIXES:
            continue
        out.append(path)
    return out


def is_exempt_file(name):
    """Files whose module-level names are reached by something other than code.

    pytest discovers `test_*` by NAME and injects fixtures by NAME, and a
    barrel's whole job is to publish a surface its own package does not
    consume. See the docstring's exemption list for the measured cost of each.
    """
    if name in ("conftest.py", "__init__.py"):
        return True
    return name.startswith("test_") or name.endswith("_test.py")


def in_scope_files(root=None):
    """Every file whose module-level names this check holds.

    The exempt FILE kinds are dropped here rather than in the symbol walk, so
    the reported in-scope count is the number of files actually held -- a count
    that included 2190 symbols nobody checks would be a misleading success line.
    """
    base = ROOT if root is None else pathlib.Path(root)
    out = []
    for scan_root in SCAN_ROOTS:
        d = base / scan_root
        if not d.is_dir():
            continue
        for path in sorted(d.rglob("*.py")):
            rel = path.relative_to(base)
            if _skipped(set(rel.parts), rel.as_posix()):
                continue
            if is_exempt_file(path.name):
                continue
            out.append(path)
    return out


def _exported(tree):
    """The names a module declares in its own `__all__`, as a set.

    A module that says a name is public has made a statement about callers this
    repository may not contain, which is a different claim from "nothing here
    reads it".
    """
    for node in tree.body:
        targets = []
        if isinstance(node, ast.Assign):
            targets = node.targets
        elif isinstance(node, ast.AnnAssign):
            targets = [node.target]
        for t in targets:
            if isinstance(t, ast.Name) and t.id == "__all__" and \
                    isinstance(node.value, (ast.List, ast.Tuple)):
                return {e.value for e in node.value.elts
                        if isinstance(e, ast.Constant) and isinstance(e.value, str)}
    return set()


def module_symbols(path, source=None):
    """Module-level names this check holds, as [(name, lineno), ...].

    TOP LEVEL ONLY -- `tree.body`, not `ast.walk` -- because a method on a
    class and a name bound inside a function are not this check's subject: the
    enclosing symbol's own reachability is what is being asked about, and a
    live class may legitimately carry a method only its subclasses call.
    """
    text = source if source is not None else path.read_text(
        encoding="utf-8", errors="replace")
    tree = ast.parse(text)
    exported = _exported(tree)
    out = []
    for node in tree.body:
        names = []
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            # A decorated def is reached THROUGH its decorator -- a pytest
            # fixture, a registration, a @contextmanager -- so its name being
            # unreferenced says nothing about whether it runs.
            if node.decorator_list:
                continue
            names = [node.name]
        elif isinstance(node, ast.Assign):
            names = [t.id for t in node.targets if isinstance(t, ast.Name)]
        elif isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name):
            names = [node.target.id]
        for name in names:
            if name.startswith("__") and name.endswith("__"):
                continue
            if name in PROTOCOL_NAMES or name in exported:
                continue
            out.append((name, node.lineno))
    return out


def prose_blind(rel):
    """Files whose PROSE does not vouch for a name -- only their CODE does.

    THE HOUSE STYLE IS WHAT MAKES THIS A FAMILY RATHER THAN A SPECIAL CASE, and
    that generalisation is this checker's second repair rather than its first.
    Every guard in `scripts/` is required to state its measured baseline, and
    the baseline NAMES the symbols it found; its test module then drives those
    same findings. Both are prose ABOUT a dead name and neither is a call to
    one, so under a mention-anywhere reading each is a second occurrence that
    exempts exactly what the guard reported.

    Measured, and it is why this is not hypothetical. The first version scoped
    this to `SELF` alone -- and the baseline paragraph had been copied, as the
    style requires, into the Makefile recipe, the CI job comment and this
    module's own test. With only `SELF` excluded, re-adding `_KEEP` to
    python/spark_agent/usercontext.py left the guard at exit 0: three other
    files were vouching for it. So the exclusion has to follow the style that
    creates the duplication, not the one file that happened to be noticed.

    Its CODE still counts, and that distinction is the repair's own first
    version: skipping a file outright stops its internal calls counting too,
    so the nine helpers this module invokes from inside itself (`text_files`,
    `module_symbols`, ...) every one reported as an orphan.
    """
    name = rel.rsplit("/", 1)[-1]
    if not name.endswith(".py"):
        return False
    if rel.startswith("scripts/") and name.startswith("check_"):
        return True
    return rel.startswith("python/tests/") and name.startswith("test_check_")


def _py_identifiers(text, keep_strings=True):
    """The identifiers of a Python source, with its `#` comments dropped.

    `tokenize` is exact about which runs of characters are comments and string
    literals, where a regex would be guessing at where a docstring ends.

    A COMMENT IS NEVER A CALL, which is why this drops them for EVERY Python
    file rather than only the prose-blind ones. The common shape is a comment
    naming the constant directly beneath it -- `# _KEEP is the allow list`
    above `_KEEP = (...)` -- and that is a second occurrence of the name in the
    dead symbol's OWN file, so it would launder the symbol single-handedly. The
    tree got away with it here only because the comment above `_KEEP` described
    the tuple without spelling it.

    `keep_strings` is what separates the two readings. An ordinary module's
    string literals still vouch, because `getattr(m, "NAME")` is a real reach;
    a prose-blind module's are dropped, because there a string literal is
    overwhelmingly a docstring restating a finding.

    Falls back to the generous whole-text reading when the source will not
    tokenize, because the safe direction for a fallback is the one that reports
    FEWER orphans: a tokenizer failure must not invent findings.
    """
    try:
        toks = list(tokenize.generate_tokens(io.StringIO(text).readline))
    except (tokenize.TokenError, IndentationError, SyntaxError):
        return _TOKEN.findall(text)
    out = []
    for t in toks:
        if t.type == _token.NAME:
            out.append(t.string)
        elif keep_strings and t.type in _STRING_TOKENS:
            out.extend(_TOKEN.findall(t.string))
    return out


def code_identifiers(text):
    """The NAME tokens of a Python source -- no comments, no string literals."""
    return _py_identifiers(text, keep_strings=False)


def strip_hash_comments(text):
    """`#` comment content removed from a file whose comment syntax is `#`.

    THE MAKEFILE AND THE CI JOB ARE WHY THIS EXISTS. This repository records
    each guard's measured baseline in a tabbed `@#` block in the `make check`
    recipe and again in the `# ...` comment above the CI step, and both name
    the symbols that guard found. Those two files alone laundered two of the
    three findings this check was written to report.

    Quote-aware, so a `#` inside a string is left alone, and the `#` must
    additionally be at line start or follow whitespace or `@`. Both clauses are
    needed and neither is decoration: the whitespace rule keeps YAML's
    `key: foo#bar` and shell's `${#arr}` intact, where `#` is literal, and the
    `@` clause is what reaches the Makefile's own `\t@# ...` recipe-comment
    idiom, which is the single most important line this has to blank.
    """
    out = []
    for line in text.split("\n"):
        quote, cut = None, len(line)
        for i, ch in enumerate(line):
            if quote is not None:
                if ch == quote:
                    quote = None
            elif ch in "\"'":
                quote = ch
            elif ch == "#" and (i == 0 or line[i - 1].isspace()
                                or line[i - 1] == "@"):
                cut = i
                break
        out.append(line[:cut])
    return "\n".join(out)


def vouching_identifiers(path, rel, text):
    """The identifiers in one file that may VOUCH for a name.

    Three readings, narrowing as the file's prose gets more likely to be about
    dead code rather than to call it: a prose-blind module contributes its NAME
    tokens only, any other Python contributes those plus its string literals, a
    `#`-comment file contributes everything outside its comments, and anything
    else -- markdown, JSON, HTML -- contributes its whole text, which is the
    generous default the module docstring argues for.
    """
    suffix = path.suffix.lower()
    if suffix in (".py", ".pyi"):
        return _py_identifiers(text, keep_strings=not prose_blind(rel))
    if path.name in _HASH_COMMENT_NAMES or suffix in _HASH_COMMENT_SUFFIXES:
        return _TOKEN.findall(strip_hash_comments(text))
    return _TOKEN.findall(text)


def identifier_counts(files, root=None):
    """How often each identifier appears across every text file, as a dict.

    One pass, one tokenizer. A name whose total is 1 occurs exactly once in the
    whole repository -- which must be its own definition, since that definition
    is itself one of these files.
    """
    counts = {}
    for path in files:
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        for name in vouching_identifiers(path, relkey(path, root), text):
            counts[name] = counts.get(name, 0) + 1
    return counts


def orphans(root=None, files=None, counts=None):
    """Module-level names mentioned nowhere but their own definition.

    Returns ([{file, line, name}, ...], symbols_considered, [unparsed files]).
    `symbols_considered` is what `main` checks for vacuity: a sweep over 107
    files that considered no symbols has stopped working and would report
    success. A file this interpreter cannot parse is NOT silently skipped --
    it is a file no rule was applied to, and reporting success over it is the
    vacuous pass every guard here is written against.
    """
    paths = in_scope_files(root) if files is None else files
    tally = (identifier_counts(text_files(root), root) if counts is None
             else counts)
    found, considered, unparsed = [], 0, []
    for path in paths:
        rel = relkey(path, root)
        try:
            symbols = module_symbols(path)
        except SyntaxError:
            unparsed.append(rel)
            continue
        for name, lineno in symbols:
            considered += 1
            # EXACTLY ONE occurrence, not zero: the definition itself is in a
            # file the counting pass read, so a reached name is always >= 2.
            if tally.get(name, 0) <= 1:
                found.append({"file": rel, "line": lineno, "name": name})
    found.sort(key=lambda f: (f["file"], f["line"]))
    return found, considered, unparsed


def main(argv):
    strict = "--strict" in argv[1:]

    files = in_scope_files()
    counted = text_files()

    # Both vacuity guards BEFORE the sweep, and separately, because they have
    # different causes and deserve different messages. A moved scan root and an
    # empty tree both leave this reporting zero orphans, which is
    # indistinguishable from a healthy tree.
    if not files:
        print("check_dead_python_symbols: no in-scope .py files under "
              f"{', '.join(SCAN_ROOTS)} - either a scan root moved (update "
              "SCAN_ROOTS) or this check now inspects nothing, which passes "
              "vacuously")
        return 1
    if not counted:
        print("check_dead_python_symbols: walked zero text files, so no "
              "reference could be found for any name - every symbol would "
              "report as an orphan. That is a broken scan rather than a "
              "finding.")
        return 1

    found, considered, unparsed = orphans(files=files,
                                          counts=identifier_counts(counted))

    if not considered:
        print(f"check_dead_python_symbols: {len(files)} in-scope file(s) "
              "yielded zero module-level symbols - the AST walk has stopped "
              "recognising definitions, so this check inspects nothing and "
              "passes vacuously")
        return 1

    if unparsed:
        print("check_dead_python_symbols: "
              f"{len(unparsed)} in-scope file(s) could not be parsed, so no "
              "symbol in them was checked:\n    " + "\n    ".join(unparsed))
        return 1

    if not strict:
        for f in found:
            print(f"ORPHAN   {f['file']}:{f['line']} {f['name']}")
        print(f"\ncheck_dead_python_symbols: {len(files)} in-scope file(s), "
              f"{considered} module-level symbol(s), {len(counted)} text "
              f"file(s) searched, {len(found)} orphan(ed)")
        return 0

    if found:
        listing = "\n    ".join(
            f"{f['file']}:{f['line']} {f['name']}" for f in found)
        print("check_dead_python_symbols: "
              f"{len(found)} module-level name(s) occur exactly once in the "
              f"whole repository -- their own definition:\n    {listing}\n"
              "  ruff's F rules cover unused imports and unused locals and "
              "have no opinion on a module-level def or constant nobody reads, "
              "so one can keep importing, type-checking and shipping forever. "
              "The reading here is mention-anywhere and deliberately generous: "
              "a Makefile recipe, a CI job, a docstring or a doc all count, so "
              "a name reported here is spelled NOWHERE else in the tree. "
              "Delete it, or reference it from whatever was meant to use it.")
        return 1

    print(f"check_dead_python_symbols: {len(files)} in-scope file(s) under "
          f"{', '.join(SCAN_ROOTS)}, {considered} module-level symbol(s), "
          f"every one mentioned somewhere in {len(counted)} text file(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
