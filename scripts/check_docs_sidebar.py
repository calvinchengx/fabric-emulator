#!/usr/bin/env python3
"""Every published doc must be reachable from the site's sidebar.

WHY THIS EXISTS. Starlight's sidebar here is an EXPLICIT list in
website/astro.config.mjs — there is no autogenerate. A doc left out of it still
builds, still gets a URL, and still turns up in search; it simply never appears
in the navigation. Nothing fails. The page is published and effectively
unreachable unless you already know its address.

That is the same failure shape as scripts/check_arch_services.py guards: a list
nothing asserts, drifting one well-meaning commit at a time. It is not
hypothetical here either — the branch adding docs/39-run-multiple-parity-plan.md
adds no sidebar entry, and every one of the 39 docs before it has one. A
convention held perfectly by hand until it wasn't.

WHAT COUNTS AS PUBLISHED. website/scripts/sync-docs.mjs decides, via DOC_RE:
`NN-name.md` chapters plus the two un-numbered living references (parity,
engine-matrix). **That regex is READ FROM sync-docs.mjs, not copied here.**

It used to be copied, under a comment saying "Mirrors DOC_RE in
website/scripts/sync-docs.mjs" — and a comment telling a future reader to keep
two things in step is a defect already filed, with no owner and no failing
test. The test that claimed to pin the coupling asserted only that the STRING
"DOC_RE" still appeared in the .mjs, then checked the local copy against a
hand-written list of filenames. Edit DOC_RE to publish a different set and
everything stayed green: the name was still there, and the hand-written list
still matched the stale copy. The assertion matched a phrase that co-occurs
with the claim rather than the claim itself.

Deriving it removes the second list instead of guarding it. The two regexes
share a syntax for this pattern (character classes, alternation, anchors), so
the JS source compiles in Python as-is. If it ever stops doing so, this fails
loudly rather than falling back to a copy — a silent fallback would rebuild
exactly the bug being removed.

ONE DIRECTION ONLY. Sidebar entries with no docs/ source are legitimate — the
landing page and the generated parity-history pages are synthesized by
sync-docs.mjs — and a slug pointing at nothing already fails the Starlight
build, so that direction needs no check here.

Usage:
    check_docs_sidebar.py            exit non-zero naming any unreachable doc
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
DOCS = ROOT / "docs"
CONFIG = ROOT / "website" / "astro.config.mjs"

SYNC_DOCS = ROOT / "website" / "scripts" / "sync-docs.mjs"

# `const DOC_RE = /…/;` — the literal, unanchored to any particular body so a
# future edit to the pattern is picked up rather than rejected.
_DOC_RE_DECL = re.compile(r"^const\s+DOC_RE\s*=\s*/(?P<body>.+)/(?P<flags>[a-z]*);\s*$", re.M)


class PatternUnavailable(RuntimeError):
    """sync-docs.mjs could not be read, found, or compiled."""


def published_pattern(path=None):
    """Compile the site's own DOC_RE so this check guards exactly its set.

    Raises rather than falling back: guarding a *guessed* set silently is the
    failure this function exists to remove.
    """
    path = path or SYNC_DOCS
    try:
        source = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise PatternUnavailable(f"cannot read {path}: {exc}") from exc
    match = _DOC_RE_DECL.search(source)
    if not match:
        raise PatternUnavailable(
            f"{path} no longer declares `const DOC_RE = /…/;` — the published set "
            "cannot be derived, so this check cannot know what to guard")
    body = match.group("body")
    try:
        return re.compile(body)
    except re.error as exc:
        raise PatternUnavailable(
            f"DOC_RE in {path} does not compile as a Python regex ({exc}); it has "
            "diverged from the shared subset and must be translated deliberately") from exc


def main():
    if not CONFIG.exists():
        print(f"check_docs_sidebar: {CONFIG} not found")
        return 1

    try:
        published = published_pattern()
    except PatternUnavailable as exc:
        print(f"check_docs_sidebar: {exc}")
        return 1

    slugs = set(re.findall(r"slug:\s*'([^']+)'", CONFIG.read_text()))
    if not slugs:
        # A parse that finds nothing must fail rather than vacuously pass.
        print("check_docs_sidebar: parsed no slugs from website/astro.config.mjs")
        return 1

    docs = sorted(p.name for p in DOCS.glob("*.md") if published.match(p.name))
    if not docs:
        print("check_docs_sidebar: parsed no published docs from docs/")
        return 1

    missing = [name for name in docs if name[:-3] not in slugs]
    if missing:
        print("check_docs_sidebar: these docs are published by the site but appear")
        print("in no sidebar group, so nothing links to them.\n")
        for name in missing:
            print(f"  docs/{name}")
        print(
            "\nAdd `{ slug: '<name-without-.md>' }` to the right group in\n"
            "website/astro.config.mjs (the sidebar is an explicit list — there\n"
            "is no autogenerate)."
        )
        return 1

    print(f"check_docs_sidebar: {len(docs)} published docs, all reachable from the sidebar")
    return 0


if __name__ == "__main__":
    sys.exit(main())
