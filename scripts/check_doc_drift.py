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

WHAT THIS CHECKS. Three classes of doc/code drift across the prose docs:

  1. DEAD REPO PATHS   -- a backticked `dir/path` under a tracked top-level
                          directory that no longer exists on disk.
  2. DEAD MAKE TARGETS -- `make <target>` naming a target the Makefile does not
                          define. Currently clean; it lands as a regression guard.
  3. UNREAD ENV VARS   -- a backticked FABRIC_/ENTRA_/... name that no
                          non-Markdown file in the repo reads, i.e. documented
                          but wired to nothing.

PRECISION OVER RECALL, deliberately, because a checker that cries wolf gets
muted and then it is a check that does not run (docs/10 has the full account of
what that costs). Three concessions buy it:

  * MAKE TARGETS ARE ANCHORED TO CODE SPANS. The naive `make \w+` regex was
    measured against this tree: 13 hits, every one of them English prose --
    "make the", "make it", "make every". Extraction is therefore restricted to
    an inline code span or a fenced-block line that reads like a COMMAND.
  * A DOT THAT IS NOT A FILE EXTENSION IS NOT A PATH, so the Go symbol
    `internal/tsql.DataFlows` is left alone while `internal/api/livy.go` is not.
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

ROOT = pathlib.Path(__file__).resolve().parent.parent
MAKEFILE = ROOT / "Makefile"

# The prose. Everything else tracked is either code, generated, or vendored.
DOC_GLOBS = ("README.md", "docs/*.md", "docs/**/*.md", "examples/README.md")

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

_FENCE = re.compile(r"^\s*(```|~~~)")
_INLINE = re.compile(r"`([^`\n]+)`")


def scan_lines(text):
    """Yield (lineno, line, in_fence) so callers see prose and code differently.

    The distinction is the whole basis of class 2's precision: a fenced line is
    a command, a prose line is a sentence, and `make` means different things in
    each.

    The marker that OPENS a fence is the only one that closes it. Toggling on
    ``` and ~~~ interchangeably means a ``` quoted inside a ~~~ block inverts the
    state for the whole REST of the file, silently swapping which lines are read
    as prose and which as commands -- and it fails silently in both directions,
    so neither a miss nor a false positive would point at the cause.
    """
    opener = None
    for lineno, line in enumerate(text.splitlines(), 1):
        match = _FENCE.match(line)
        if match:
            if opener is None:
                opener = match.group(1)
                continue
            if match.group(1) == opener:
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
    Asking git is exact everywhere, and it is the same list the other two
    classes already read.
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
_TARGET = re.compile(r"^([A-Za-z0-9_.\- ]+):(?!=)")
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
    """
    pattern = re.compile(rf"\b(?:{_ENV_ALT})[A-Z0-9_]+\b")
    names = set()
    for rel in tracked_files():
        if rel.endswith(".md") or rel.startswith(SKIP_PREFIXES):
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


# --- reporting ----------------------------------------------------------------

CLASSES = (
    ("path", "names a path that does not exist",
     "point it at the real path, or add an EXEMPT entry if the reference is "
     "deliberately forward-looking"),
    ("make", "invokes a target the Makefile does not define",
     "use a defined target, or add the target to the Makefile"),
    ("env", "documents a variable no non-Markdown file reads",
     "wire it up, or correct the name to the one the code actually reads"),
)


def docs():
    """The prose files in scope, deduplicated and ordered."""
    seen = {}
    for pattern in DOC_GLOBS:
        for path in ROOT.glob(pattern):
            rel = str(path.relative_to(ROOT)).replace("\\", "/")
            if path.is_file() and not rel.startswith(SKIP_DIRS):
                seen[rel] = path
    return sorted(seen.items())


def findings():
    """Every drift in the tree, as (kind, doc, lineno, shown, key)."""
    index = tracked_index()
    targets = make_targets()
    env_defined = env_names_in_code()
    found = []
    for rel, path in docs():
        text = path.read_text(encoding="utf-8")
        for kind, produce, arg in (("path", dead_paths, index),
                                   ("make", dead_targets, targets),
                                   ("env", unread_env, env_defined)):
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
        print(f"check_doc_drift: {len(docs())} docs, "
              f"{len(EXEMPT)} recorded forward references, no drift")
        return 0

    print("check_doc_drift: documentation names code that is not there.\n")
    for kind, label, fix in CLASSES:
        hits = [f for f in found if f[0] == kind]
        if not hits:
            continue
        print(f"  {label}:")
        for _, rel, lineno, shown, _key in hits:
            print(f"    {rel}:{lineno}  {shown}")
        print(f"    -> {fix}\n")
    return 1 if arguments.strict else 0


if __name__ == "__main__":
    sys.exit(main())
