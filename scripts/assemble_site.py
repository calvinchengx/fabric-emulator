#!/usr/bin/env python3
"""Assemble the published site: the landing page at the root, the docs beneath it.

    scripts/assemble_site.py --out _site
    scripts/assemble_site.py --self-test

WHY THIS EXISTS. The docs moved from `/fabric-emulator/<slug>/` to
`/fabric-emulator/docs/<slug>/`, which is the shape the rest of the
family uses. That moved **105 published routes**, and a moved URL is a 404 for
every link already pointing at it: this project's own README, anything in the
sibling repositories, and anything outside them nobody can enumerate. Three of
them are still linked from committed documentation in this family today
(`/10-testing/`, `/12-e2e-matrix/`, `/parity/`), and all three are 404 on the
live site, measured. The links nobody can grep are the reason for this file.

So every old path gets a redirect stub here, and the stubs are not optional
politeness: `website/published-routes.txt` is the route list captured from the
build immediately BEFORE the move, and this script fails if any entry in it
would 404. That file is the oracle. Deriving the check from the new build
instead would only prove the new build agrees with itself.

ASTRO'S `redirects:` CANNOT DO THIS. A redirect key is emitted underneath the
configured base, so after the move `/01-quickstart/` publishes at
`/fabric-emulator/docs/01-quickstart/` and nothing answers the
root-level path anyone actually linked to. The stubs must be written outside
Astro, which is here.

THE BADGES STAY AT THE ROOT, and are written by the workflow after this script
runs: their numbers come from the last CI run's artifact, which is not
something a build here can reproduce. This script clears the output directory,
so nothing may be written into it beforehand.
"""

from __future__ import annotations

import argparse
import re
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DIST = ROOT / "website" / "dist"
LANDING = ROOT / "site" / "index.html"
ROUTES = ROOT / "website" / "published-routes.txt"
BASE = "/fabric-emulator/docs/"

# Routes RENAMED by the move rather than merely relocated, which therefore have
# no same-named page under /docs/ to stub from.
#
# Empty here, and that is a finding rather than an omission: every one of this
# project's 105 published routes keeps its own name under /docs/. The one route
# that did change meaning is the site root, which used to serve the docs index
# and now serves the landing page. It is deliberately absent from the oracle
# below: it does not 404, it shows a different page, which is the point of the
# change.
ALIASES: dict[str, str] = {}

STUB = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Moved</title>
<link rel="canonical" href="{target}">
<meta http-equiv="refresh" content="0; url={target}">
<meta name="robots" content="noindex">
</head>
<body>This page moved to <a href="{target}">{target}</a>.</body>
</html>
"""


def routes_in(tree: Path) -> set[str]:
    """Every route a built tree serves, as `a/b` with no leading or trailing slash."""
    out = set()
    for page in tree.rglob("index.html"):
        rel = page.relative_to(tree).parent.as_posix()
        out.add("" if rel == "." else rel)
    return out


def write_stub(out: Path, route: str) -> None:
    if not route:
        return  # the root is the landing page, never a stub
    page = out / route / "index.html"
    page.parent.mkdir(parents=True, exist_ok=True)
    page.write_text(STUB.format(target=f"{BASE}{route}/"), encoding="utf-8")


def assemble(out: Path) -> int:
    if not (DIST / "index.html").is_file():
        raise SystemExit(
            f"assemble_site: no Starlight build at {DIST}. Run the docs build first."
        )
    if not LANDING.is_file():
        raise SystemExit(f"assemble_site: no landing page at {LANDING}")

    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)

    shutil.copytree(DIST, out / "docs")
    shutil.copy2(LANDING, out / "index.html")

    # A stub for every route the docs now serve, at the path it used to have.
    new = routes_in(out / "docs")
    for route in sorted(new):
        write_stub(out, route)
    for old_route, target in ALIASES.items():
        page = out / old_route / "index.html"
        page.parent.mkdir(parents=True, exist_ok=True)
        page.write_text(
            STUB.format(target=f"{BASE}{target}/" if target else BASE), encoding="utf-8"
        )

    # THE ORACLE. Every route the site published before the move must resolve.
    if not ROUTES.is_file():
        raise SystemExit(f"assemble_site: {ROUTES} is missing; there is nothing to check against")
    old = {r.strip() for r in ROUTES.read_text(encoding="utf-8").splitlines() if r.strip()}
    if not old:
        raise SystemExit(f"assemble_site: {ROUTES} is empty; the check would pass over nothing")
    missing = sorted(r for r in old if not (out / r / "index.html").is_file())
    if missing:
        print("assemble_site FAILED: these published routes would 404", file=sys.stderr)
        for r in missing:
            print(f"  /{r}/", file=sys.stderr)
        return 1

    print(
        f"assemble_site: {len(new)} docs route(s) under {BASE}, "
        f"{len(old)} pre-move route(s) still resolving, landing page at the root"
    )
    return check_absolute_refs(out)


# THE PROJECT PREFIX GitHub Pages serves this whole site under. The docs sit at
# BASE, the landing page one level up at this, and nothing at all is served
# outside it.
SITE_PREFIX = "/" + BASE.strip("/").split("/")[0] + "/"


def resolve(request_path: str) -> tuple[str, str]:
    """Decide a request the way GitHub Pages would, as (verb, argument).

    Pure, so the self-test can exercise it without a socket. `redirect` names
    the target; `serve` names the path RELATIVE to the assembled tree; `404`
    names nothing.
    """
    path = request_path.split("?", 1)[0].split("#", 1)[0]
    if path in ("", "/", SITE_PREFIX.rstrip("/")):
        # Pages serves the user/org site at /, not this project. A preview that
        # answered the landing page there would hide every absolute link that
        # is missing the prefix, which is the bug this exists to catch.
        return "redirect", SITE_PREFIX
    if not path.startswith(SITE_PREFIX):
        return "404", ""
    return "serve", "/" + path[len(SITE_PREFIX) :]


def check_absolute_refs(out: Path) -> int:
    """Every absolute href/src the built docs emit must resolve in the tree.

    Astro bakes the base into these, so nothing downstream re-checks them, and
    a reference to a file that was never shipped publishes a 404 on every page
    at once. Not hypothetical: this was written because the built pages asked
    for `favicon.svg`, website/public/ did not exist, and the live entra site
    had been 404ing it on all 44 pages unnoticed.

    The hand-written landing page is Astro's blind spot and is checked
    separately; between the two the whole tree is covered.
    """
    dangling: dict[str, set[str]] = {}
    checked = 0
    for page in sorted((out / "docs").rglob("*.html")):
        for ref in re.findall(r'(?:href|src)="(/[^"/][^"]*)"', page.read_text(encoding="utf-8")):
            checked += 1
            verb, local = resolve(ref)
            target = out / local.lstrip("/") if verb == "serve" else None
            if target is not None and (target.is_file() or (target / "index.html").is_file()):
                continue
            dangling.setdefault(ref, set()).add(page.relative_to(out).as_posix())
    if not checked:
        print(
            "assemble_site FAILED: no absolute reference found in any built page. "
            "The pattern has stopped matching, so this check guards nothing.",
            file=sys.stderr,
        )
        return 1
    if dangling:
        print("assemble_site FAILED: these references 404", file=sys.stderr)
        for ref, pages in sorted(dangling.items()):
            where = sorted(pages)
            print(f"  {ref}  ({len(where)} page(s), e.g. {where[0]})", file=sys.stderr)
        return 1
    print(f"assemble_site: {checked} absolute reference(s) in the built pages resolve")
    return 0


def serve(site: Path, port: int) -> int:
    """Serve an already-assembled tree at the URLs it will publish at.

    Not `astro dev`, and not a substitute for it. `astro dev` is based at BASE
    and knows nothing about the tree around it, so under it the landing page
    does not exist, the redirect stubs do not exist, and the badge endpoints
    the landing page fetches do not exist. Everything that has broken on this
    site broke in that gap. This serves the artifact instead.
    """
    from http.server import HTTPServer, SimpleHTTPRequestHandler

    if not (site / "index.html").is_file():
        raise SystemExit(
            f"assemble_site: no assembled site at {site}. Run `make docs-build` first."
        )

    class Handler(SimpleHTTPRequestHandler):
        def __init__(self, *a, **kw):
            super().__init__(*a, directory=str(site), **kw)

        def send_head(self):
            verb, arg = resolve(self.path)
            if verb == "redirect":
                self.send_response(302)
                self.send_header("Location", arg)
                self.end_headers()
                return None
            if verb == "404":
                self.send_error(404, f"nothing is published outside {SITE_PREFIX}")
                return None
            return super().send_head()

        def translate_path(self, path):
            verb, arg = resolve(path)
            return super().translate_path(arg if verb == "serve" else path)

    httpd = HTTPServer(("127.0.0.1", port), Handler)
    print("the assembled site, at the paths it publishes under:")
    print(f"    http://localhost:{port}{SITE_PREFIX}   landing page")
    print(f"    http://localhost:{port}{BASE}   docs")
    print("Ctrl-C to stop. 404s are logged below, and they are real.")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print()
    return 0


def self_test() -> int:
    """Prove the oracle can fail, on a tree built by hand.

    A gate nobody has watched fail is a gate nobody knows the direction of.
    """
    import tempfile

    ok = True
    with tempfile.TemporaryDirectory() as d:
        out = Path(d) / "site"
        (out / "docs" / "kept").mkdir(parents=True)
        (out / "docs" / "kept" / "index.html").write_text("x")
        (out / "docs" / "index.html").write_text("x")
        for route in sorted(routes_in(out / "docs")):
            write_stub(out, route)

        have = (out / "kept" / "index.html").is_file()
        ok &= have
        print(f"  {'ok  ' if have else 'FAIL'} a stub is written for each docs route")

        gone = not (out / "vanished" / "index.html").is_file()
        ok &= gone
        print(f"  {'ok  ' if gone else 'FAIL'} a route with no page has no stub, so the oracle can catch it")

        root_is_free = not (out / "index.html").exists()
        ok &= root_is_free
        print(f"  {'ok  ' if root_is_free else 'FAIL'} the root is left for the landing page")

        points_at_docs = f'href="{BASE}kept/"' in (out / "kept" / "index.html").read_text()
        ok &= points_at_docs
        print(f"  {'ok  ' if points_at_docs else 'FAIL'} the stub points into {BASE}")

    # The preview server must route the way Pages does, or it is worse than no
    # preview: it would answer requests the real host refuses and show a site
    # that works when the published one does not.
    for request, expected in (
        ("/", ("redirect", SITE_PREFIX)),
        (SITE_PREFIX.rstrip("/"), ("redirect", SITE_PREFIX)),
        (SITE_PREFIX, ("serve", "/")),
        (f"{BASE}some-page/", ("serve", "/docs/some-page/")),
        (f"{SITE_PREFIX}witnesses.json?v=2", ("serve", "/witnesses.json")),
        ("/some-other-project/", ("404", "")),
        ("/docs/", ("404", "")),  # the docs are NOT at the root; that was the move
    ):
        got = resolve(request)
        ok &= got == expected
        print(f"  {'ok  ' if got == expected else 'FAIL'} {request} -> {got[0]} {got[1]}")

    print("self-test passed" if ok else "self-test FAILED")
    return 0 if ok else 1


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--out", type=Path, default=ROOT / "_site")
    p.add_argument("--self-test", action="store_true")
    p.add_argument(
        "--serve",
        action="store_true",
        help="serve an already-assembled tree at its published paths, instead of assembling",
    )
    p.add_argument("--site", type=Path, default=ROOT / "_site", help="tree to serve")
    p.add_argument("--port", type=int, default=8099)
    a = p.parse_args()
    if a.self_test:
        return self_test()
    if a.serve:
        return serve(a.site, a.port)
    return assemble(a.out)


if __name__ == "__main__":
    raise SystemExit(main())
