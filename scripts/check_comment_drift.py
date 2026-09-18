#!/usr/bin/env python3
r"""Go comments that name code, docs or retired vocabulary must still be true.

WHY THIS EXISTS. `check_doc_drift.py` holds prose to the tree it describes, and
`check_plan_freshness.py` holds a docs/24 row to the sub-plan beneath it. Both
stop at `docs/`. Nothing reads a Go comment, and a Go comment is where this
repo keeps its reasoning -- the cause of a refusal, the oracle a list came
from, the precedent an activity was allowed under.

The motivating failure is recent and is exactly the shape the other two
checkers were written for, one directory over. #490 measured the leaf/compute
distinction and retired it: no oracle has a connector activity type, so the
dispatch default was refusing nothing on behalf of a leaf that does not exist.
`parity.md` and `docs/43` were corrected in the same change. Two source
comments were not, and went on teaching the retired justification in the
PRESENT TENSE for a week:

  * internal/api/unrunnableactivities.go -- "The default is right for a
    CONNECTOR LEAF (a ServiceNow source needs a vendor SDK, and the run really
    did reach the leaf in dependsOn order with its inputs resolved)".
  * internal/api/azuremlactivity.go -- a false green is "worse here than for a
    connector leaf", comparing against a category that had been retired.

Neither is a typo. Each is a justification that expired, and the only thing
that would ever have caught them is somebody happening to read.

WHAT THIS CHECKS. Three classes across every tracked `.go` file's comments:

  1. DEAD REPO PATHS     -- a path under a tracked top-level directory that is
                            not in the tree. 113 resolve today, none dead; it
                            lands as a regression guard.
  2. DEAD DOC REFERENCES -- `docs/NN` naming a numbered document that does not
                            exist. 140 resolve today, none dead; likewise a
                            guard. This is the reference Go comments make most,
                            and a renumbered document breaks every one silently.
  3. RETIRED VOCABULARY  -- a term RETIRED records as no longer describing
                            anything, appearing more often than the tree is
                            pinned at.

PRECISION OVER RECALL, on the same argument as check_doc_drift: a checker that
cries wolf gets muted, and then it is a check that does not run. The naive
version of class 1 was measured against this tree before it was narrowed --
`any token with a file extension` reported 84 dead paths, and every one was a
false positive of three kinds:

  * MICROSOFT LEARN SLUGS. `onelake/onelake-access-api.md`,
    `governance/domains.md` -- citations of the oracle, which is the single
    most valuable thing a comment here can carry.
  * PAYLOAD FILENAMES. `data.json`, `copyjob-content.json`, `Files/in/day.csv`
    -- names inside the API being emulated, belonging to the caller's data and
    not to this repository.
  * FOREIGN PATHS. `entityTypes/Pipeline.json` (ADF's schema), `/jobs/etl.py`
    (a path on a Databricks cluster).

Class 1 is therefore restricted to tokens ROOTED AT A TRACKED TOP-LEVEL
DIRECTORY, which is what distinguishes `internal/api/livy.go` from all three.
That took the class from 84 findings to nought, and the ones it gave up were
never this checker's to make.

WHAT CLASS 3 DOES NOT DO, stated because the gap is the interesting part. It
catches a retired term being REINTRODUCED. It cannot catch a justification
written today going stale tomorrow, because nothing mechanical distinguishes a
true `is` from a false one -- the stale comment above read "the run really DID
reach the leaf", so even a past-tense heuristic would have passed it. Retiring
a concept is a deliberate act, and this class asks that the act be recorded
once so the tree cannot quietly drift back.

Usage:
    check_comment_drift.py            report drift, exit 0
    check_comment_drift.py --strict   exit non-zero on any drift (CI, `make check`)
"""
import argparse
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent

# This checker and its own test. Every retired term and every sample path is
# written here as an ARGUMENT to the check, never as a live claim about the
# tree, so counting them would be counting the question as an answer.
SELF = ("scripts/check_comment_drift.py", "python/tests/test_check_comment_drift.py")

# A dot in the last segment is only a file extension if it is one of these.
# Without it the Go symbol `internal/tsql.DataFlows` reads as a missing file --
# the same concession check_doc_drift.py makes, for the same reason.
KNOWN_EXTS = {
    ".go", ".py", ".md", ".yml", ".yaml", ".json", ".sql", ".sh", ".ts", ".tsx",
    ".js", ".mjs", ".ipynb", ".txt", ".toml", ".tf", ".ps1", ".csv", ".java",
    ".html", ".css", ".svelte", ".mod", ".sum", ".lock", ".env",
}

# Vocabulary retired by a measurement, pinned to the occurrences the tree is
# known to carry. The count is what makes an entry enforceable: every
# occurrence below has been read and is PAST TENSE, recording that the idea was
# held and was retired, which is history worth keeping. A fifth occurrence is
# either a fifth such record -- raise the count in the same commit, and the
# raise is the review -- or the idea creeping back in, which is what this is
# for.
#
# Counts rather than line numbers on purpose: a line number moves whenever
# anything above it is edited, and a checker that fails for an unrelated edit
# is a checker people learn to re-baseline without reading.
RETIRED = {
    "connector leaf": {
        "reason":
            "#490 measured it and retired it: a connector is not an activity "
            "type in either oracle, it is the `type` of a Copy source or sink. "
            "The dispatch default now refuses everything that reaches it, so "
            "there is no leaf for it to be right about.",
        "pinned": {
            "internal/api/unknownactivity.go": 1,
            "internal/api/unrunnableactivities.go": 1,
            "internal/api/unrunnableactivities_test.go": 1,
            "internal/api/pipelines.go": 1,
        },
    },
}


# --- extraction ---------------------------------------------------------------

def tracked_files(pattern=None):
    """Tracked paths, from git rather than the filesystem.

    A working tree holds a great deal git does not: a `.venv/` under an
    example, `node_modules/`, a `.claude/worktrees/` copy of the whole tree.
    Asking git also means a file is in scope exactly when it is reviewable.
    """
    argv = ["git", "ls-files"] + ([pattern] if pattern else [])
    out = subprocess.run(argv, cwd=ROOT, capture_output=True, text=True, check=True)
    return [line for line in out.stdout.splitlines() if line]


def tracked_index():
    """(top-level directories, every tracked path including ancestor dirs)."""
    tops, paths = set(), set()
    for rel in tracked_files():
        paths.add(rel)
        parts = rel.split("/")
        if len(parts) > 1:
            tops.add(parts[0])
        for i in range(1, len(parts)):
            paths.add("/".join(parts[:i]))
    return tops, paths


def comment_spans(text):
    """Yield (lineno, comment_text) for every Go comment in `text`.

    A HAND-WALKED SCANNER RATHER THAN A REGEX, because the two constructs are
    not separable line by line. `"https://learn.microsoft.com/..."` contains
    `//` and is a string; a `//` inside a raw backtick literal is a string too;
    and a `/*` inside either opens nothing. Matching `//.*$` reads the second
    half of every documentation URL in the tree as a comment, which is both a
    miss and a flood of findings about the oracle's own filenames.

    Only the four states Go actually has are tracked: code, line comment, block
    comment, and string (interpreted, raw, or rune). Escapes matter inside an
    interpreted string and not inside a raw one, which is the one asymmetry.
    """
    i, line, n = 0, 1, len(text)
    while i < n:
        ch = text[i]
        if ch == "\n":
            line += 1
            i += 1
        elif text.startswith("//", i):
            end = text.find("\n", i)
            end = n if end == -1 else end
            yield line, text[i + 2:end]
            i = end
        elif text.startswith("/*", i):
            end = text.find("*/", i + 2)
            end = n if end == -1 else end + 2
            body = text[i:end]
            start_line = line
            for offset, chunk in enumerate(body.split("\n")):
                yield start_line + offset, chunk
            line += body.count("\n")
            i = end
        elif ch in ('"', "'", "`"):
            i += 1
            while i < n:
                if text[i] == "\n":
                    line += 1
                    # An interpreted string cannot span a line; bail rather
                    # than swallowing the rest of the file on unbalanced input.
                    if ch != "`":
                        break
                elif ch != "`" and text[i] == "\\":
                    i += 1
                elif text[i] == ch:
                    break
                i += 1
            i += 1
        else:
            i += 1


# --- class 1: dead repo paths -------------------------------------------------

_TOKEN = re.compile(r"[A-Za-z0-9_./-]+")


def dead_paths(text, index):
    """Yield (lineno, token) for a repo-rooted path that is not in the tree."""
    tops, paths = index
    for lineno, body in comment_spans(text):
        for token in _TOKEN.finditer(body):
            name = token.group(0).strip(".,;:")
            if "/" not in name or name.split("/")[0] not in tops:
                continue
            if pathlib.PurePosixPath(name).suffix.lower() not in KNOWN_EXTS:
                continue
            if name not in paths:
                yield lineno, name


# --- class 2: dead doc references ---------------------------------------------

_DOCNUM = re.compile(r"\bdocs/(\d{1,3})\b")


def numbered_docs():
    """{'43': 'docs/43-activity-completion-plan.md'} for every numbered doc.

    Keyed by the number as written with leading zeros stripped, so `docs/7` and
    `docs/07` resolve alike -- both spellings are in the tree's comments.
    """
    found = {}
    for rel in tracked_files("docs/*.md"):
        stem = rel.split("/")[-1].split("-")[0]
        if stem.isdigit():
            found[stem.lstrip("0") or "0"] = rel
    return found


def dead_doc_refs(text, known):
    """Yield (lineno, token) for a `docs/NN` naming no such document."""
    for lineno, body in comment_spans(text):
        for match in _DOCNUM.finditer(body):
            number = match.group(1).lstrip("0") or "0"
            if number not in known:
                yield lineno, f"docs/{match.group(1)}"


# --- class 3: retired vocabulary ----------------------------------------------

def retired_counts(rel, text):
    """{term: occurrences} for every RETIRED term in this file's comments.

    Case-insensitive: the tree writes the same idea as `CONNECTOR LEAF` for
    emphasis and `connector leaf` in running prose, and a retirement applies to
    the idea rather than to a capitalisation of it.
    """
    counts = {}
    joined = "\n".join(body for _, body in comment_spans(text)).lower()
    for term in RETIRED:
        found = joined.count(term.lower())
        if found:
            counts[term] = found
    return counts


def retired_drift(per_file):
    """Yield (term, rel, found, pinned) wherever a file exceeds its pin."""
    for term, record in sorted(RETIRED.items()):
        pinned = record["pinned"]
        for rel in sorted(set(per_file) | set(pinned)):
            found = per_file.get(rel, {}).get(term, 0)
            if found > pinned.get(rel, 0):
                yield term, rel, found, pinned.get(rel, 0)


# --- reporting ----------------------------------------------------------------

CLASSES = (
    ("path", "a comment names a path that does not exist",
     "point it at the real path, or drop the reference if the file is gone"),
    ("doc", "a comment cites a numbered document that does not exist",
     "cite the document's current number, or drop the reference"),
)


def go_files():
    """Every tracked `.go` file except this checker's own fixtures."""
    return [rel for rel in tracked_files("*.go") if rel not in SELF]


def findings():
    """(class findings, retired-vocabulary findings)."""
    index = tracked_index()
    known = numbered_docs()
    found, per_file = [], {}
    for rel in go_files():
        text = (ROOT / rel).read_text(encoding="utf-8", errors="ignore")
        for lineno, shown in dead_paths(text, index):
            found.append(("path", rel, lineno, shown))
        for lineno, shown in dead_doc_refs(text, known):
            found.append(("doc", rel, lineno, shown))
        counts = retired_counts(rel, text)
        if counts:
            per_file[rel] = counts
    return found, list(retired_drift(per_file))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--strict", action="store_true",
                        help="exit non-zero on any drift")
    arguments = parser.parse_args()

    found, retired = findings()
    if not found and not retired:
        pins = sum(len(r["pinned"]) for r in RETIRED.values())
        print(f"check_comment_drift: {len(go_files())} Go files, "
              f"{len(RETIRED)} retired term(s) pinned across {pins} file(s), "
              f"no drift")
        return 0

    print("check_comment_drift: Go comments name things that are not there.\n")
    for kind, label, fix in CLASSES:
        hits = [f for f in found if f[0] == kind]
        if not hits:
            continue
        print(f"  {label}:")
        for _, rel, lineno, shown in hits:
            print(f"    {rel}:{lineno}  {shown}")
        print(f"    -> {fix}\n")

    if retired:
        print("  a retired term is used more than the tree is pinned at:")
        for term, rel, count, pinned in retired:
            print(f"    {rel}  \"{term}\" x{count} (pinned at {pinned})")
        for term, _, _, _ in retired:
            print(f'    -> "{term}": {RETIRED[term]["reason"]}')
        print("    -> if the new use RECORDS the retirement rather than "
              "relying on it, raise the pin in the same commit\n")
    return 1 if arguments.strict else 0


if __name__ == "__main__":
    sys.exit(main())
