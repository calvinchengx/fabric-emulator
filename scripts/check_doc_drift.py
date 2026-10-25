#!/usr/bin/env python3
r"""Documentation that names code must name code that exists.

WHY THIS EXISTS. Prose is the one artifact in this repo with nothing underneath
it. A renamed directory breaks a Go import and CI goes red; the same rename in a
sentence breaks nothing, so the sentence keeps its confident tone and quietly
starts lying. Three had already drifted when this was written, and each was the
same shape -- a reference that was true when it was typed:

  * docs/38 cited `e2e/warehouse-tds` as a directory. The WAREHOUSE READER IS
    REAL and runs on every push; it is a `go test` job, and there has never been
    a directory of that name for a reader to go look in.
  * docs/31 cited `examples/medallion-pyspark/common.py` for the failure
    reporting that motivates the whole document. That file lives at
    `examples/contoso-fixtures/common.py`, so the sentence explaining WHY the
    feature exists pointed at nothing.
  * docs/13 cited `internal/store/fabric.go` for the workspace-identity object
    -- a real file in the SIBLING entra-emulator repo, read here as one of ours.

None of these is a typo. Each is a fact that expired, and the only thing that
would ever have caught them is somebody happening to click.

WHAT THIS CHECKS. Six classes of doc/code drift across the prose docs:

  1. DEAD REPO PATHS   -- a backticked `dir/path` under a tracked top-level
                          directory that no longer exists on disk.
  2. DEAD MAKE TARGETS -- `make <target>` naming a target the Makefile does not
                          define. Currently clean; it lands as a regression guard.
  3. UNREAD ENV VARS   -- a backticked FABRIC_/ENTRA_/... name that no
                          non-Markdown file in the repo reads, i.e. documented
                          but wired to nothing.
  4. DEAD GO SYMBOLS   -- a backticked repo package reference such as
                          `internal/api.SomeName` whose tracked Go package
                          directory or top-level symbol no longer exists.
  5. DEAD MD LINKS     -- a Markdown link to a repo file, directory, or simple
                          Markdown heading anchor that no longer exists.
  6. DEAD CLI FLAGS    -- a single-dash flag with an internal hyphen, such as
                          `-lro-delay`, that the emulator's flag set does not
                          register. It found one the day it landed: docs/35
                          named a -clock-offset flag that exists nowhere in
                          this tree, so a reader was told to reach for a lever
                          the binary would reject. The clock is controllable
                          only through `POST /_emulator/clock`.

PRECISION OVER RECALL, deliberately, because a checker that cries wolf gets
muted and then it is a check that does not run (docs/10 has the full account of
what that costs). These concessions buy it:

  * MAKE TARGETS ARE ANCHORED TO CODE SPANS. The naive `make \w+` regex was
    measured against this tree: 13 hits, every one of them English prose --
    "make the", "make it", "make every". Extraction is therefore restricted to
    an inline code span or a fenced-block line that reads like a COMMAND.
  * A DOT THAT IS NOT A FILE EXTENSION IS NOT A PATH, so the Go symbol
    `internal/tsql.DataFlows` is not reported as a dead FILE while
    `internal/api/livy.go` is. Go package/symbol references are checked by their
    own narrower class.
  * GO SYMBOLS ARE INDEXED FROM SOURCE TEXT, NOT A FULL PARSER. The check only
    trusts top-level declarations in tracked `.go` files: funcs, including
    generic funcs, and type/var/const declarations. It deliberately ignores
    methods and deeper language semantics so it can fail on stale prose without
    pretending to be `go/types`.
  * MARKDOWN ANCHORS ARE SIMPLE HEADING SLUGS ONLY. Ordinary links to files and
    directories are checked against git's tracked paths; anchors are checked
    only for Markdown targets that exist, and only when the fragment looks like
    a GitHub-style heading slug rather than a line anchor or generated id.
  * A CLI FLAG MUST CARRY AN INTERNAL HYPHEN. `-lro-delay` is this emulator's
    shape. `-race`, `-count`, `-it` and `-j4` belong to other people's tools,
    and `--strict`/`--no-sync` are this repo's own Python and uv invocations;
    the internal-hyphen rule is what leaves all of them alone without a list
    of third-party flags to keep current. Measured before it was chosen, not
    after: 30 distinct single-dash tokens in inline spans across the prose, 29
    of them live emulator flags and the thirtieth the dead -clock-offset this
    class was written to find, plus 3 more in fenced command lines, all live.
    No third-party noise at all, because every third-party flag this tree's
    prose and transcripts write is double-dash or a single word. THE RECALL
    COST, STATED: a single-word flag such as `-addr` is not checked, and the
    direction is one-way -- documented-but-unregistered is this class, while
    registered-but-undocumented is a coverage question, not a drift one.
  * A TEST FILE DOES NOT REGISTER A FLAG. `_test.go` sources are skipped when
    indexing, for the same reason `SELF` exists for the env class: a flag only
    a test registers is not a flag the emulator has, so crediting one would
    leave a doc reference passing after the real registration was deleted.
  * AN EMPTY FLAG INDEX IS AN INDEX FAILURE, NOT THIRTY FINDINGS. If the
    registration form moves -- a different flag package, a helper wrapping
    `fs.StringVar` -- the index empties and every documented flag in the tree
    looks dead at once. The checker reports THAT instead, under its own
    heading and still non-zero under `--strict`, because thirty false
    positives in one run is how a check gets muted in a single sitting.
  * RELEASE NOTES ARE OUT OF SCOPE. docs/release-notes/** describes the repo as
    it was at a tag. A v0.16 note naming a since-renamed file is CORRECT, and
    editing it would be falsifying a historical record to please a checker.

The env scan credits CODE UNDER docs/. `docs/demo/flow.py` and
`docs/demo/flow-override.yml` are real code that happens to live beside prose,
so the exclusion is by EXTENSION (.md) and never by directory -- excluding
`docs/` wholesale wrongly reports `DEMO_FABRIC_PORT` as read by nothing.

EXEMPT records a deliberate forward reference instead of hiding it. A doc may
legitimately name something not built yet -- a planned checker, a test worth
having -- and the alternative to an exemption is editing true prose into vaguer
prose. Every entry carries a written reason, so the list is auditable rather
than a silencer.

Usage:
    check_doc_drift.py            report drift, exit 0
    check_doc_drift.py --strict   exit non-zero on any drift (CI, `make check`)
"""
import argparse
import pathlib
import re
import subprocess
import sys
import urllib.parse

ROOT = pathlib.Path(__file__).resolve().parent.parent
MAKEFILE = ROOT / "Makefile"

# The prose. Everything else tracked is either code, generated, or vendored.
#
# The per-directory READMEs earn their place: a README beside an example or a
# suite is the prose MOST likely to cite a path inside its own directory, and
# the first version of this list skipped them all in favour of the one at
# `examples/README.md`. Measured when they were added, `e2e/dbt-fabric/README.md`
# was citing `docs/17-parity.md` -- a document that has never existed under that
# name -- so the gap was not hypothetical.
DOC_GLOBS = ("README.md", "docs/*.md", "docs/**/*.md", "examples/README.md",
             "examples/*/README.md", "e2e/**/README.md", "python/**/README.md")

# Historical snapshots: correct about a tree that no longer exists. See the
# docstring -- this is the exclusion that keeps the check honest rather than
# merely quiet.
SKIP_DIRS = ("docs/release-notes/",)

# Build output and vendored trees. `git ls-files` already excludes most of this;
# the list matters for the filesystem fallback, where nothing does.
SKIP_PREFIXES = ("_site/", "node_modules/", ".venv/", "dist/", ".claude/",
                 "third_party/", "__pycache__/")

# A dot in the last segment is only a file extension if it is one of these.
# Without this, the Go symbol `internal/tsql.DataFlows` reads as a missing file.
KNOWN_EXTS = {
    ".go", ".py", ".md", ".yml", ".yaml", ".json", ".sql", ".sh", ".ts", ".tsx",
    ".js", ".mjs", ".ipynb", ".txt", ".toml", ".tf", ".ps1", ".csv", ".java",
    ".html", ".css", ".svelte", ".tape", ".gif", ".png", ".xml", ".cfg", ".lock",
    ".mod", ".sum", ".env", ".properties", ".conf", ".ini", ".jsonl", ".parquet",
}

# The project's own variable families. Narrow on purpose: a bare PATH or HOME in
# backticks is not this repo's to account for.
ENV_PREFIXES = ("FABRIC_", "ENTRA_", "EMULATOR_", "ONELAKE_", "VAULT_",
                "SPARK_", "DEMO_")
_ENV_ALT = "|".join(ENV_PREFIXES)
ENV_RE = re.compile(rf"^(?:{_ENV_ALT})[A-Z0-9_]+$")

# This checker and its own test. See env_names_in_code: a name written here is
# an argument to the check, never something the product reads.
SELF = ("scripts/check_doc_drift.py", "python/tests/test_check_doc_drift.py")

# Binary-ish files the env scan need not decode.
BINARY_EXTS = {".gif", ".png", ".jpg", ".jpeg", ".ico", ".woff", ".woff2",
               ".parquet", ".zip", ".gz", ".pdf", ".ttf", ".otf", ".webp"}

# A deliberate forward reference, recorded rather than silently invisible.
# Keyed by (doc path relative to the repo root, the exact token). Adding an
# entry is a claim that the reference is INTENDED to point at something not yet
# built; the reason is what makes that claim reviewable.
EXEMPT = {
    ("docs/30-odcs-data-contracts.md", "scripts/check_contracts.py"):
        "planned, not yet built: the O2 row of that document's own milestone "
        "table, which describes work to come rather than work that landed.",
    ("docs/54-onelake-security.md", "e2e/task-parameters"):
        "planned, not yet built: cited as a suite whose discipline is 'worth "
        "having', which is an argument for writing it, not a claim it exists.",
    ("docs/13-roadmap.md", "internal/store/fabric.go"):
        "a file in the SIBLING entra-emulator repo, not this one. The prose "
        "now says so; this checker cannot resolve a path across repos.",
}


# --- extraction ---------------------------------------------------------------

_FENCE = re.compile(r"^\s*(`{3,}|~{3,})")
_INLINE = re.compile(r"`([^`\n]+)`")
_MD_LINK = re.compile(r"(?<!!)\[[^\]\n]+\]\(([^)\s]+)(?:\s+[^)]*)?\)")


def scan_lines(text):
    """Yield (lineno, line, in_fence) so callers see prose and code differently.

    The distinction is the whole basis of class 2's precision: a fenced line is
    a command, a prose line is a sentence, and `make` means different things in
    each.

    THE MARKER THAT OPENS A FENCE IS THE ONLY ONE THAT CLOSES IT, and a marker
    is its CHARACTER AND ITS LENGTH, not its character alone. Both halves are
    load-bearing and the failure is identical either way -- the state inverts
    for the whole REST of the file, silently swapping which lines are read as
    prose and which as commands, so neither the miss nor the false positive
    that follows points anywhere near the cause:

      * a ``` quoted inside a ~~~ block would close it, if the CHARACTER were
        ignored;
      * a ``` quoted inside a ```` block would close it, if the LENGTH were --
        which is exactly how a document explaining this checker\'s own Markdown
        handling would break it, since four-backtick fences exist to quote
        three-backtick ones.

    CommonMark\'s rule is what is implemented: a closing fence uses the
    opener\'s character and is at least as long, so a longer run inside a block
    is content and a shorter one cannot end it.
    """
    opener = None
    for lineno, line in enumerate(text.splitlines(), 1):
        match = _FENCE.match(line)
        if match:
            marker = match.group(1)
            if opener is None:
                opener = marker
                continue
            if marker[0] == opener[0] and len(marker) >= len(opener):
                opener = None
            continue
        yield lineno, line, opener is not None


def inline_spans(text):
    """Yield (lineno, content) for every inline code span outside a fence.

    Fenced blocks are excluded because they are transcripts -- shell sessions,
    sample output, YAML -- where a path may legitimately describe another
    machine's filesystem rather than this repo's.
    """
    for lineno, line, in_fence in scan_lines(text):
        if in_fence:
            continue
        for match in _INLINE.finditer(line):
            yield lineno, match.group(1)


def markdown_links(text):
    """Yield (lineno, target) for Markdown links outside fences.

    The extractor is intentionally small: it catches ordinary inline Markdown
    links, not every CommonMark edge case. Images, fenced examples, reference
    links, and HTML are outside this class's precision budget.
    """
    for lineno, line, in_fence in scan_lines(text):
        if in_fence:
            continue
        for match in _MD_LINK.finditer(line):
            target = match.group(1).strip().strip("<>")
            if target:
                yield lineno, target


# --- class 1: dead repo paths -------------------------------------------------

_LINE_SUFFIX = re.compile(r":\d+(?:-\d+)?$")
_UNPATHLIKE = set(" \t*?<>{}$|\\\"'()[]")


def tracked_index():
    """(top-level dirs, every tracked path incl. ancestor dirs) -- from git.

    Derived so a new top-level package is covered the day it lands, and so
    untracked build output (`_site/`, `node_modules/`) is never a candidate
    prefix in the first place.

    The second set is what existence is decided against, rather than
    `(ROOT / candidate).exists()`. A filesystem answer is CASE-INSENSITIVE on a
    default macOS APFS volume and case-sensitive on the Linux CI runner, so a
    reference with the wrong case passes `make check` on a laptop and fails in
    CI -- the checker disagreeing with itself across platforms, which is the
    one failure that makes a guard untrustworthy rather than merely wrong.
    Asking git is exact everywhere, and it is the same list the other classes
    already read.
    """
    paths = tracked_files()
    tops = {path.split("/", 1)[0] for path in paths if "/" in path}
    known = set(paths)
    for path in paths:
        parts = path.split("/")
        for depth in range(1, len(parts)):
            known.add("/".join(parts[:depth]))
    return tops, known


def path_candidate(token):
    """The repo-relative path this token refers to, or None if it is not one."""
    token = token.strip()
    # `docs/54-onelake-security.md:243` -- a citation, not a filename.
    token = _LINE_SUFFIX.sub("", token)
    token = token.rstrip(".,;:)]}>'\"").rstrip("/")
    if not token or "/" not in token:
        return None
    if _UNPATHLIKE & set(token):
        return None
    last = token.rsplit("/", 1)[1]
    if last.isdigit():
        # `docs/24`, `docs/10` -- this repo's own shorthand for a NUMBERED
        # DOCUMENT, used constantly ("`docs/24`'s L is unchanged"). No tracked
        # file anywhere has a purely numeric name, so this costs no coverage
        # and removes nine false positives that would have muted the check.
        return None
    if "." in last:
        ext = last[last.rindex("."):].lower()
        if ext not in KNOWN_EXTS:
            # `internal/tsql.DataFlows`, `pipeline.ActivityRun` -- a qualified
            # symbol, which is prose about code rather than a pointer to a file.
            return None
    return token


def dead_paths(doc, text, index):
    tops, known = index
    for lineno, token in inline_spans(text):
        candidate = path_candidate(token)
        if candidate is None:
            continue
        if candidate.split("/", 1)[0] not in tops:
            continue
        if candidate in known:
            continue
        yield lineno, token, candidate


# --- class 2: dead make targets -----------------------------------------------

# A rule may name SEVERAL targets (`foo bar:`), so the whole left-hand side is
# captured and split. `(?!=)` keeps `VAR := x` and `VAR ?= x` out; the `$`-free
# class keeps `$(GEN)/thing:` out, which is a computed name this cannot resolve.
#
# The names are spelled as a first name plus repeats rather than as one class
# with a space in it. `[A-Za-z0-9_.\- ]+` reads the same and is not: a class
# containing a space matches a LEADING space too, so an INDENTED line carrying a
# colon parses as a rule. ` Targets: check lint` inside a `define` block would
# register a target called `Targets`, and a phantom name is the quiet direction
# of wrong -- it makes `make Targets` look defined and mutes a real finding
# instead of inventing one. This form cannot start with whitespace at all, which
# also excludes recipe lines (tab-indented by construction).
_TARGET = re.compile(r"^([A-Za-z0-9_.-]+(?:[ \t]+[A-Za-z0-9_.-]+)*)[ \t]*:(?!=)")
_MAKE_CMD = re.compile(r"^\$?\s*make\s+(\S+)")


def make_targets():
    """Target names the Makefile defines.

    `.PHONY` is dropped: it is a directive whose VALUE is the target list, and
    counting it as a target would make `make .PHONY` look defined.
    """
    names = set()
    for line in MAKEFILE.read_text(encoding="utf-8").splitlines():
        match = _TARGET.match(line)
        if not match:
            continue
        for name in match.group(1).split():
            if not name.startswith("."):
                names.add(name)
    return names


def make_invocations(text):
    r"""Yield (lineno, target) for `make <target>` written as a COMMAND.

    An inline span starting `make `/`$ make `, or a fenced line doing the same.
    Bare prose is never read, which is what separates this from the naive
    `make \w+` regex -- measured on this tree, that form returns 13 hits and all
    13 are English ("make the", "make it", "make every", "make a").
    """
    for lineno, line, in_fence in scan_lines(text):
        sources = [line] if in_fence else [m.group(1) for m in _INLINE.finditer(line)]
        for source in sources:
            match = _MAKE_CMD.match(source.strip())
            if match:
                yield lineno, match.group(1)


# A hyphen is legal INSIDE a target name (`e2e-run`, `up-jvm`) and never at the
# front, where it starts a flag. The character class is therefore split rather
# than written `[A-Za-z0-9_.-]+`: that single class admits a leading hyphen, so
# `make -j4` was captured as a target named `-j4` and reported as undefined --
# the comment below promised the opposite, and the code did not hold it.
_TARGET_NAME = re.compile(r"^[A-Za-z0-9_.][A-Za-z0-9_.-]*$")


def dead_targets(doc, text, defined):
    for lineno, word in make_invocations(text):
        # The first word after `make` is only a TARGET if it is a bare name.
        # `make PROFILE=lean` is a variable override and `make -j4` is a flag;
        # neither names a target, and reporting either would be a false
        # positive of exactly the kind that gets a check muted. The word is
        # captured whole (`\S+`) so the guard can see the `=` -- an earlier
        # version captured `[A-Za-z0-9_.-]+`, which stopped AT the `=` and
        # handed the guard a plausible-looking "PROFILE" to flag.
        #
        # THE RECALL COST, STATED. Only the first word is looked at, so
        # rejecting it abandons the real target BEHIND it: `make -j4 test` and
        # `make -C portal build` are skipped entirely rather than resolved to
        # `test` and `build`. Walking past the flags is not free -- `-j4` takes
        # no argument and `-C` takes the next word, so a version that skipped
        # flags blindly would read `portal` as a target and report the false
        # positive this guard exists to prevent. Knowing which make flags
        # consume an argument is a second list to keep current, for a form no
        # doc in this tree writes; the one invocation the guard skips today is
        # the `make <target>` placeholder in docs/10.
        if not _TARGET_NAME.match(word) or word in defined:
            continue
        yield lineno, f"make {word}", word


# --- class 3: env vars nothing reads ------------------------------------------

def tracked_files():
    """Repo-relative paths git tracks, falling back to a walk outside a checkout."""
    try:
        out = subprocess.run(["git", "ls-files", "-z"], cwd=ROOT, check=True,
                             capture_output=True, text=True).stdout
        paths = [p for p in out.split("\0") if p]
        if paths:
            return paths
    except (OSError, subprocess.CalledProcessError):
        pass
    return [str(p.relative_to(ROOT)).replace("\\", "/")
            for p in ROOT.rglob("*") if p.is_file()]


def env_names_in_code():
    """Every project env name appearing in a tracked NON-Markdown file.

    The .md exclusion is by extension and never by directory: `docs/demo/flow.py`
    and `docs/demo/flow-override.yml` are code, and skipping `docs/` wholesale
    reports `DEMO_FABRIC_PORT` as read by nothing.

    A CHECKER MAY NOT CREDIT ITSELF. This file and its test are tracked .py
    files, so without SELF are scanned like any other -- and a name appearing
    in either is a LITERAL IN AN ARGUMENT, never a reader. That is a real
    weakening rather than a tidiness point: the test names `FABRIC_FORCE_LRO`
    and `DEMO_FABRIC_PORT`, both of which the product genuinely reads today, so
    if the last real reader of one were deleted the doc reference would keep
    passing on the strength of a test fixture. Excluded, the answer comes only
    from code that would actually break.
    """
    pattern = re.compile(rf"\b(?:{_ENV_ALT})[A-Z0-9_]+\b")
    names = set()
    for rel in tracked_files():
        if rel.endswith(".md") or rel.startswith(SKIP_PREFIXES):
            continue
        if rel in SELF:
            continue
        if pathlib.PurePosixPath(rel).suffix.lower() in BINARY_EXTS:
            continue
        try:
            text = (ROOT / rel).read_text(encoding="utf-8", errors="ignore")
        except OSError:
            continue
        names.update(pattern.findall(text))
    return names


def unread_env(doc, text, defined):
    for lineno, token in inline_spans(text):
        name = token.strip().rstrip(".,;:")
        if not ENV_RE.match(name) or name in defined:
            continue
        yield lineno, name, name


# --- class 4: dead Go package/symbol references -------------------------------

_GO_REF = re.compile(
    r"^([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+)\.([A-Za-z_][A-Za-z0-9_]*)$"
)
_GO_TOP_FUNC = re.compile(
    r"^\s*func\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:\[[^\]]+\])?\s*\("
)
_GO_DECL = re.compile(r"^\s*(?:type|var|const)\s+([A-Za-z_][A-Za-z0-9_]*)\b")
_GO_DECL_BLOCK = re.compile(r"^\s*(type|var|const)\s*\(")
_GO_DECL_BLOCK_NAME = re.compile(r"^\s*([A-Za-z_][A-Za-z0-9_]*)\b")
_GO_PACKAGE = re.compile(r"^\s*package\s+([A-Za-z_][A-Za-z0-9_]*)\b", re.MULTILINE)


def _strip_go_line_comment(line):
    """Drop // comments outside simple string/rune literals.

    This is intentionally small rather than parser-shaped. It keeps commented
    examples from defining symbols while avoiding the common `http://` and
    `//`-inside-a-string mistakes that a plain split would make.
    """
    quote = None
    escaped = False
    for index, char in enumerate(line):
        if quote:
            if escaped:
                escaped = False
            elif quote != "`" and char == "\\":
                escaped = True
            elif char == quote:
                quote = None
            continue
        if char in ("'", '"', "`"):
            quote = char
            continue
        if char == "/" and index + 1 < len(line) and line[index + 1] == "/":
            return line[:index]
    return line


def _go_top_level_symbols(text):
    """Top-level Go symbols visible as package-qualified doc references."""
    symbols = set()
    depth = 0
    decl_block = None
    for raw in text.splitlines():
        line = _strip_go_line_comment(raw)
        if depth == 0:
            if decl_block:
                if line.strip().startswith(")"):
                    decl_block = None
                else:
                    match = _GO_DECL_BLOCK_NAME.match(line)
                    if match:
                        symbols.add(match.group(1))
            else:
                for pattern in (_GO_TOP_FUNC, _GO_DECL):
                    match = pattern.match(line)
                    if match:
                        symbols.add(match.group(1))
                        break
                else:
                    if _GO_DECL_BLOCK.match(line):
                        decl_block = True
        depth += line.count("{") - line.count("}")
        if depth < 0:
            depth = 0
    return symbols


def go_package_index():
    """Tracked Go package directories and their top-level symbols."""
    packages = {}
    for rel in tracked_files():
        if not rel.endswith(".go") or rel.startswith(SKIP_PREFIXES):
            continue
        path = ROOT / rel
        try:
            text = path.read_text(encoding="utf-8", errors="ignore")
        except OSError:
            continue
        if not _GO_PACKAGE.search(text):
            continue
        directory = rel.rsplit("/", 1)[0] if "/" in rel else "."
        packages.setdefault(directory, set()).update(_go_top_level_symbols(text))
    return packages


def dead_go_symbols(doc, text, packages):
    for lineno, token in inline_spans(text):
        candidate = token.strip().rstrip(".,;:)]}>'\"")
        if path_candidate(candidate) is not None:
            continue
        match = _GO_REF.match(candidate)
        if not match:
            continue
        package_dir, symbol = match.groups()
        if package_dir not in packages:
            if package_dir.split("/", 1)[0] in {p.split("/", 1)[0] for p in packages}:
                yield lineno, token, candidate
            continue
        if symbol not in packages[package_dir]:
            yield lineno, token, candidate


# --- class 5: dead Markdown links --------------------------------------------

_HEADING = re.compile(r"^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$")
_SIMPLE_HEADING_ANCHOR = re.compile(r"^[a-z0-9][a-z0-9-]*$")
_LINE_ANCHOR = re.compile(r"^L\d+(?:-L\d+)?$")
_HTML_TAG = re.compile(r"<[^>]+>")
_INLINE_IMAGE = re.compile(r"!\[[^\]]*\]\([^)]*\)")
_INLINE_LINK = re.compile(r"\[([^\]]*)\]\([^)]*\)")
_MD_PUNCT = re.compile(r"[^\w\s-]")
_MD_SPACE = re.compile(r"\s")


def _normalise_repo_path(path):
    """Collapse a POSIX path and return None if it escapes the repo."""
    parts = []
    for part in pathlib.PurePosixPath(path).parts:
        if part in ("", "."):
            continue
        if part == "..":
            if not parts:
                return None
            parts.pop()
            continue
        parts.append(part)
    return "/".join(parts)


def _resolve_doc_link(doc, target):
    """Return (repo path or None for same doc, anchor, shown key), or None."""
    pieces = urllib.parse.urlsplit(target)
    if pieces.scheme or pieces.netloc:
        return None
    if pieces.path.startswith("/"):
        return None
    if pieces.scheme in ("mailto",):
        return None

    raw_path = urllib.parse.unquote(pieces.path)
    anchor = urllib.parse.unquote(pieces.fragment)
    if pieces.query:
        return None
    if not raw_path and not anchor:
        return None

    if raw_path:
        base = pathlib.PurePosixPath(doc).parent
        resolved = _normalise_repo_path(str(base / raw_path))
    else:
        resolved = doc
    if resolved is None:
        return None
    return resolved, anchor, target


def github_heading_slug(heading):
    """GitHub-style slug base for a Markdown heading."""
    heading = _HTML_TAG.sub("", heading)
    heading = _INLINE_IMAGE.sub("", heading)
    heading = _INLINE_LINK.sub(r"\1", heading)
    heading = re.sub(r"`([^`]*)`", r"\1", heading)
    heading = heading.strip().lower()
    heading = _MD_PUNCT.sub("", heading)
    heading = _MD_SPACE.sub("-", heading).strip("-")
    return heading


def markdown_heading_anchors(rel):
    """Simple GitHub-style heading anchors for a Markdown file."""
    try:
        text = (ROOT / rel).read_text(encoding="utf-8")
    except OSError:
        return set()

    anchors = set()
    seen = {}
    for _lineno, line, in_fence in scan_lines(text):
        if in_fence:
            continue
        match = _HEADING.match(line)
        if not match:
            continue
        base = github_heading_slug(match.group(2))
        count = seen.get(base, 0)
        seen[base] = count + 1
        anchors.add(base if count == 0 else f"{base}-{count}")
    return anchors


def dead_markdown_links(doc, text, index):
    _tops, known = index
    anchor_cache = {}
    for lineno, target in markdown_links(text):
        if target.startswith(("http://", "https://", "mailto:")):
            continue
        resolved = _resolve_doc_link(doc, target)
        if resolved is None:
            continue
        rel, anchor, key = resolved
        if rel.startswith(SKIP_PREFIXES):
            continue
        if rel not in known:
            yield lineno, target, key
            continue
        if not anchor:
            continue
        if _LINE_ANCHOR.match(anchor) or not _SIMPLE_HEADING_ANCHOR.match(anchor):
            continue
        if pathlib.PurePosixPath(rel).suffix.lower() != ".md":
            continue
        anchors = anchor_cache.setdefault(rel, markdown_heading_anchors(rel))
        if anchor not in anchors:
            yield lineno, target, key


# --- class 6: dead CLI flags --------------------------------------------------

# Where the emulator registers its flags today. Named only so the index-failure
# message can point somewhere useful; the scan itself reads every tracked
# non-test Go file, because a flag registered outside `cmd/` would still be a
# flag the emulator has -- and a registration this index misses reports a LIVE
# flag as dead, which is the loud direction of wrong.
CLI_SOURCE = "cmd/fabric-emulator/main.go"

_FLAG_KINDS = "Bool|Duration|Float64|Int|Int64|String|Uint|Uint64"

# `fs.StringVar(&cfg.Addr, "addr", ...)`, `flag.BoolVar(&x, "name", ...)`. THE
# `&` IS THE DISCRIMINATOR: a method taking a pointer and then a quoted
# lowercase name is a flag registration by convention, which is what lets this
# index the registrations without knowing the name of the variable holding the
# flag set. `[^,\n]+` rather than `[^,]+` because `[^,]` matches a newline, and
# a cross-line match would pair one call's pointer with the next call's name.
_FLAG_VAR = re.compile(
    rf'\b[A-Za-z_]\w*\.(?:{_FLAG_KINDS}|Text)?Var\(\s*&[^,\n]+,\s*"([a-z0-9][\w.-]*)"'
)
# The non-Var forms -- `fs.String("name", default, usage)` -- which return a
# pointer instead of filling one. This tree writes none of them today; indexing
# them anyway means switching to one does not read as thirty flags dying.
_FLAG_FUNC = re.compile(
    rf'\b[A-Za-z_]\w*\.(?:{_FLAG_KINDS})\(\s*"([a-z0-9][\w.-]*)"\s*,'
)


def cli_flags():
    """Flag names the emulator registers, from tracked non-test Go sources.

    A `_test.go` FILE DOES NOT COUNT, which is the same weakening `SELF`
    prevents for the env class: a flag registered only by a test is not a flag
    the binary has, so crediting one would leave a doc reference passing after
    the real registration had been deleted.

    An empty answer does not mean the emulator has no flags. It means the
    registration form is no longer the one these patterns know, and `findings`
    reports THAT rather than reporting every documented flag at once.
    """
    names = set()
    for rel in tracked_files():
        if not rel.endswith(".go") or rel.endswith("_test.go"):
            continue
        if rel.startswith(SKIP_PREFIXES):
            continue
        try:
            text = (ROOT / rel).read_text(encoding="utf-8", errors="ignore")
        except OSError:
            continue
        for pattern in (_FLAG_VAR, _FLAG_FUNC):
            names.update(pattern.findall(text))
    return names


# A flag this project could own: one dash, lowercase, and at least one INTERNAL
# hyphen. See the docstring -- the internal hyphen is this class's whole
# precision budget, and it is what leaves `--strict`, `--no-sync`, `-race` and
# `-j4` alone without a list of other people's tools to keep current.
_CLI_FLAG = re.compile(r"^-([a-z][a-z0-9]*(?:-[a-z0-9]+)+)$")

# Punctuation a flag can be wrapped in, in a sentence or a shell transcript.
# `-` IS DELIBERATELY ABSENT: stripping it would turn `--strict` into
# `-strict`, inventing a finding out of a flag that was never this emulator's.
_FLAG_TRIM = "`'\"(),;:.[]{}<>\\|"


def flag_tokens(text):
    """Yield (lineno, token) for every dash-led word in a span or a command.

    BOTH halves are read, unlike the path and env classes, which skip fences
    because a transcript may describe another machine's filesystem. A fenced
    line is where a quickstart actually SHOWS the invocation -- docs/01 starts
    the emulator with `-entra-issuer` inside a fence and nowhere else -- so
    skipping fences would have cost three of the thirty-three references
    measured. The risk fences carry for the other classes does not transfer:
    a flag has to be single-dash AND internally hyphenated to be read at all,
    and nothing in this tree's transcripts but this emulator writes that shape.
    """
    for lineno, line, in_fence in scan_lines(text):
        sources = [line] if in_fence else [m.group(1) for m in _INLINE.finditer(line)]
        for source in sources:
            for word in source.split():
                token = word.strip(_FLAG_TRIM)
                if token.startswith("-"):
                    yield lineno, token


def dead_flags(doc, text, defined):
    for lineno, token in flag_tokens(text):
        # `-lro-delay=5` and `-lro-delay 5` document the same flag, so the
        # value is cut before the lookup. Without this, the `=` form resolves
        # to a name no flag set could ever hold and every documented default
        # becomes a finding.
        name = token.split("=", 1)[0]
        match = _CLI_FLAG.match(name)
        if not match or match.group(1) in defined:
            continue
        yield lineno, token, name


# --- reporting ----------------------------------------------------------------

CLASSES = (
    ("path", "names a path that does not exist",
     "point it at the real path, or add an EXEMPT entry if the reference is "
     "deliberately forward-looking"),
    ("make", "invokes a target the Makefile does not define",
     "use a defined target, or add the target to the Makefile"),
    ("env", "documents a variable no non-Markdown file reads",
     "wire it up, or correct the name to the one the code actually reads"),
    ("go", "names a Go package or symbol that does not exist",
     "point it at the live package/symbol, or add an EXEMPT entry if the "
     "reference is deliberately forward-looking"),
    ("markdown-link", "links to a Markdown target that does not exist",
     "point it at the live file, directory, or heading, or add an EXEMPT entry "
     "if the reference is deliberately forward-looking"),
    ("cli-flag", "documents a flag the emulator does not define",
     "name the flag the binary registers, or the control surface that actually "
     "exists, or add an EXEMPT entry if the reference is deliberately "
     "forward-looking"),
)

# NOT DRIFT: the checker failing to see the code it checks against. Reported in
# its own right because an empty flag index makes every documented flag in the
# tree look dead at once, and thirty false positives in one run is how a check
# gets muted in a single sitting. It shares the `--strict` exit path, because a
# class that has quietly stopped checking anything is a build failure too --
# `make check` going green on a class that matched nothing is item Eight of
# docs/10 one level in.
INDEX_FAILURE = (
    "cli-flag-index", "found no flag registrations, so class 6 checked nothing",
    "teach cli_flags() the registration form the code now uses, rather than "
    "leaving the class to pass by seeing nothing",
)


def docs():
    """The prose files in scope, deduplicated and ordered.

    INTERSECTED WITH WHAT GIT TRACKS, for the same reason `tracked_index` asks
    git rather than the filesystem. A glob walks whatever is on disk, and a
    working tree holds a great deal that is not this repo's prose: a
    `.venv/` under an example, `node_modules/`, a `.claude/worktrees/` copy of
    the whole tree. Measured on this machine, widening the globs to the
    per-directory READMEs without this pulled in 763 files and reported drift in
    a vendored `dompurify` README -- findings about somebody else's
    documentation, which is the fastest way to teach a reader to skim past this
    check. Tracked-only also means a doc is in scope exactly when it is
    reviewable.
    """
    tracked = set(tracked_files())
    seen = {}
    for pattern in DOC_GLOBS:
        for path in ROOT.glob(pattern):
            rel = str(path.relative_to(ROOT)).replace("\\", "/")
            if not path.is_file() or rel not in tracked:
                continue
            if not rel.startswith(SKIP_DIRS):
                seen[rel] = path
    return sorted(seen.items())


def findings():
    """Every drift in the tree, as (kind, doc, lineno, shown, key)."""
    index = tracked_index()
    targets = make_targets()
    env_defined = env_names_in_code()
    go_packages = go_package_index()
    flags_defined = cli_flags()
    classes = [("path", dead_paths, index),
               ("make", dead_targets, targets),
               ("env", unread_env, env_defined),
               ("go", dead_go_symbols, go_packages),
               ("markdown-link", dead_markdown_links, index)]
    found = []
    if flags_defined:
        classes.append(("cli-flag", dead_flags, flags_defined))
    else:
        # The class is SKIPPED, not run against an empty index, so the report
        # is one index failure rather than every flag the docs name.
        found.append((INDEX_FAILURE[0], CLI_SOURCE, 0,
                      "no flag registration matched", INDEX_FAILURE[0]))
    for rel, path in docs():
        text = path.read_text(encoding="utf-8")
        for kind, produce, arg in classes:
            for lineno, shown, key in produce(rel, text, arg):
                if (rel, key) in EXEMPT:
                    continue
                found.append((kind, rel, lineno, shown, key))
    return found


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--strict", action="store_true",
                        help="exit non-zero on any drift")
    arguments = parser.parse_args()

    found = findings()
    if not found:
        # The class count is DERIVED, so this line cannot drift from the
        # thing it describes the way the prose about this checker can.
        print(f"check_doc_drift: {len(docs())} docs, {len(CLASSES)} classes, "
              f"{len(EXEMPT)} recorded forward references, no drift")
        return 0

    print("check_doc_drift: documentation names code that is not there.\n")
    for kind, label, fix in (*CLASSES, INDEX_FAILURE):
        hits = [f for f in found if f[0] == kind]
        if not hits:
            continue
        print(f"  {label}:")
        for _, rel, lineno, shown, _key in hits:
            # A lineno of 0 is the index failure, which is about a FILE rather
            # than a line, and `main.go:0` would send a reader to look at one.
            where = f"{rel}:{lineno}" if lineno else rel
            print(f"    {where}  {shown}")
        print(f"    -> {fix}\n")
    return 1 if arguments.strict else 0


if __name__ == "__main__":
    sys.exit(main())
