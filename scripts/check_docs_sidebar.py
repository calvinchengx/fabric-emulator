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
engine-matrix). That regex is mirrored below rather than re-derived, so a file
this check considers is exactly a file the site publishes. Anything else in
docs/ (release notes, subdirectories) is not synced and not required.

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

# Mirrors DOC_RE in website/scripts/sync-docs.mjs — the set the site publishes.
PUBLISHED = re.compile(r"^(\d{2}-.*|parity|engine-matrix)\.md$")


def main():
    if not CONFIG.exists():
        print(f"check_docs_sidebar: {CONFIG} not found")
        return 1

    slugs = set(re.findall(r"slug:\s*'([^']+)'", CONFIG.read_text()))
    if not slugs:
        # A parse that finds nothing must fail rather than vacuously pass.
        print("check_docs_sidebar: parsed no slugs from website/astro.config.mjs")
        return 1

    docs = sorted(p.name for p in DOCS.glob("*.md") if PUBLISHED.match(p.name))
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
