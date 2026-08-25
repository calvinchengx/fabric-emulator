#!/usr/bin/env python3
"""Keep the documentation navigable: no broken links, no unreachable pages.

Two failures, neither of which the site build catches:

1. **A link to a page that is not there.** Measured: the Astro build exits 0 on
   a dangling intra-doc link and publishes a 404. Renumbering or renaming a
   chapter rewrites filenames and the links pointing at them drift silently.
2. **A page missing from the sidebar.** Starlight's sidebar here is an EXPLICIT
   list, so a doc left out still builds, still gets a URL, and still appears in
   search. It is simply unreachable by navigation. Nothing fails.

WHAT COUNTS AS PUBLISHED is decided by `DOC_RE` in
`website/scripts/sync-docs.mjs`, and this **reads that regex rather than
copying it**. A copy under a comment saying "keep in step with sync-docs" is a
defect already filed, with no owner and no failing test: change the published
set and the copy stays green while guarding the wrong thing.

NOTE for this repo: `check_docs_sidebar.py` already guards the sidebar half and
has its own tests, so the two overlap there deliberately. Retiring the older one
is its owner's call, not a side effect of adding link checking.

Run with --strict in CI.
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
DOCS = ROOT / "docs"
CONFIG = ROOT / "website" / "astro.config.mjs"
SYNC_DOCS = ROOT / "website" / "scripts" / "sync-docs.mjs"

# `const DOC_RE = /…/;` in sync-docs.mjs. The body is taken as written so this
# guards exactly the set the site publishes.
_DOC_RE_DECL = re.compile(r"^const\s+DOC_RE\s*=\s*/(?P<body>.+)/(?P<flags>[a-z]*);\s*$", re.M)

# `](00-slug.md)`, `](./00-slug.md#anchor)`, `](parity.md)`, `](generated/x.md)`
DOC_LINK = re.compile(r"\]\((?:\./)?((?:[a-z0-9-]+/)?[a-z0-9][a-z0-9.-]*\.md)(#[^)]*)?\)")
README_LINK = re.compile(r"\]\(docs/((?:[a-z0-9-]+/)?[a-z0-9][a-z0-9.-]*\.md)(#[^)]*)?\)")
SIDEBAR_SLUG = re.compile(r"slug:\s*'([^']+)'")

# Pages sync-docs SYNTHESIZES rather than reading from docs/ — currently the
# docs overview, which is first in the sidebar and has no file behind it. This
# was a hardcoded `{"index"}` and went stale the moment the landing page moved
# to website/src/pages/index.astro and the overview took the sidebar slot: the
# checker then reported a page that was fine and exempted one that no longer
# existed, both silently. Derived from the generator instead, so it cannot
# disagree with what is actually written.


# NO SYNTHESIZED SET ANY MORE, and its own error message asked for this:
# "If that is deliberate, remove this derivation; do not leave it matching
# nothing."
#
# sync-docs.mjs used to write one page with no file behind it -- the docs
# overview -- so a sidebar entry for it had to be exempted from the
# has-a-page check. The docs root absorbed that page, nothing is synthesized
# by literal name, and an exemption matching nothing is worse than none: it
# would pass whatever a future sidebar listed.
#
# A page generated in FAMILIES rather than by name is still covered, by the
# parity-versions rule just below.

# Routes the site GENERATES rather than reads from docs/. `parity-versions.mjs`
# writes a `parity-history/` index, a `parity-history/changelog`, and one page
# per release tag, so a sidebar entry pointing at those has no file behind it
# and is correct. Only exempted when that generator is actually present, so a
# repo without it still gets a dangling slug reported.
GENERATED_PREFIX = "parity-history"
PARITY_VERSIONS = ROOT / "website" / "scripts" / "parity-versions.mjs"


def published_pattern() -> re.Pattern[str]:
    """Compile the site's own DOC_RE, so this guards exactly its set."""
    if not SYNC_DOCS.exists():
        raise SystemExit(f"docs-links: {SYNC_DOCS} not found; cannot derive the published set")
    # encoding="utf-8" EVERYWHERE IN THIS FILE, not the platform default.
    # `read_text()` decodes as cp1252 on Windows, and these docs are full of
    # em dashes and arrows — so every call here raised UnicodeDecodeError
    # there. It went unnoticed because this script only ran in `docs-build`,
    # a Linux-only job; it surfaced the moment `make check` began running it
    # on the three-OS matrix.
    match = _DOC_RE_DECL.search(SYNC_DOCS.read_text(encoding="utf-8"))
    if not match:
        raise SystemExit(
            f"docs-links: {SYNC_DOCS} no longer declares `const DOC_RE = /…/;`, so the "
            "published set cannot be derived. Fix the derivation rather than copying it here."
        )
    try:
        return re.compile(match.group("body"))
    except re.error as exc:
        raise SystemExit(
            f"docs-links: DOC_RE does not compile as a Python regex ({exc})"
        ) from exc


def problems() -> list[str]:
    found: list[str] = []
    published_re = published_pattern()

    for page in sorted(DOCS.glob("*.md")):
        for match in DOC_LINK.finditer(page.read_text(encoding="utf-8")):
            if not (DOCS / match.group(1)).exists():
                found.append(f"{page.name} links to {match.group(1)}, which does not exist")

    readme = ROOT / "README.md"
    if readme.exists():
        for match in README_LINK.finditer(readme.read_text(encoding="utf-8")):
            if not (DOCS / match.group(1)).exists():
                found.append(f"README.md links to docs/{match.group(1)}, which does not exist")

    if not CONFIG.exists():
        found.append(f"{CONFIG} not found; the sidebar cannot be checked")
        return found

    slugs = set(SIDEBAR_SLUG.findall(CONFIG.read_text(encoding="utf-8")))
    generated = PARITY_VERSIONS.exists()
    for slug in sorted(slugs):
        if generated and (slug == GENERATED_PREFIX or slug.startswith(GENERATED_PREFIX + "/")):
            continue
        if not (DOCS / f"{slug}.md").exists():
            found.append(f"the sidebar lists {slug}, which has no page")

    published = [p for p in sorted(DOCS.glob("*.md")) if published_re.match(p.name)]
    for page in published:
        if page.stem in slugs:
            continue
        found.append(f"{page.name} is not in the sidebar, so nothing on the site links to it")

    return found


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--strict", action="store_true", help="exit non-zero on any problem")
    arguments = parser.parse_args()
    found = problems()
    for problem in found:
        print(f"docs-links: {problem}")
    if found and arguments.strict:
        return 1
    if not found:
        published_re = published_pattern()
        count = len([p for p in DOCS.glob("*.md") if published_re.match(p.name)])
        print(f"docs-links: {count} published pages, every link resolves and every page is reachable")
    return 0


if __name__ == "__main__":
    sys.exit(main())
