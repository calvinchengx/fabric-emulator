"""Tests for the published-site assembly: redirect stubs, the oracle, the preview.

WHAT IS ACTUALLY AT RISK. This script exists because 105 published routes moved
under `/docs/`, and every one of them is a 404 for links nobody can enumerate.
Its guarantee is therefore negative — *nothing that used to resolve stops
resolving* — and a negative guarantee is the kind that fails silent. If
`routes_in` stops finding pages, no stub is written, the oracle finds nothing
to complain about only because it is comparing against an empty set, and the
run is green while the site is broken.

So every check here is driven from the failing side first: a route the oracle
must catch, a reference that must be reported dangling, a pattern that has
stopped matching. The passing case is the control, not the subject.

`resolve` gets its own attention because the preview server is worse than
useless if it routes more permissively than GitHub Pages: it would answer
requests the real host refuses, and show a working site where the published one
404s. That is the exact failure the landing page's missing favicon was.
"""
import pathlib
import sys
import threading
import urllib.error
import urllib.request

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import assemble_site as c  # noqa: E402

# Astro bakes the base into every href it emits, so a real built page always
# carries absolute references. The default here does too: a page with none of
# them trips the "the pattern has stopped matching" guard, which is correct
# behaviour and would make every other test here fail for the wrong reason.
DEFAULT_PAGE = '<html><head><link href="/fabric-emulator/docs/"></head><body>doc</body></html>'


def build_dist(root: pathlib.Path, routes=("", "01-quickstart", "parity"), page=None):
    """A stand-in for the Starlight build: one index.html per route."""
    dist = root / "dist"
    for route in routes:
        target = dist / route if route else dist
        target.mkdir(parents=True, exist_ok=True)
        (target / "index.html").write_text(
            page if page is not None else DEFAULT_PAGE, encoding="utf-8")
    return dist


@pytest.fixture
def site(tmp_path, monkeypatch):
    """Point the module's inputs at a tree this test owns."""

    def build(routes=("", "01-quickstart", "parity"), published=None, page=None,
              landing="<html>landing</html>", write_routes=True, write_landing=True,
              write_dist=True):
        if write_dist:
            monkeypatch.setattr(c, "DIST", build_dist(tmp_path, routes, page))
        else:
            monkeypatch.setattr(c, "DIST", tmp_path / "no-dist")
        landing_path = tmp_path / "index.html"
        if write_landing:
            landing_path.write_text(landing, encoding="utf-8")
        monkeypatch.setattr(c, "LANDING", landing_path)
        routes_file = tmp_path / "published-routes.txt"
        if write_routes:
            listed = published if published is not None else [r for r in routes if r]
            routes_file.write_text("\n".join(listed) + "\n", encoding="utf-8")
        monkeypatch.setattr(c, "ROUTES", routes_file)
        return tmp_path / "out"

    return build


# --- routes and stubs -------------------------------------------------------


def test_routes_in_reads_a_built_tree(tmp_path):
    dist = build_dist(tmp_path, ("", "a", "a/b"))
    assert c.routes_in(dist) == {"", "a", "a/b"}


def test_routes_in_ignores_anything_that_is_not_a_page(tmp_path):
    dist = build_dist(tmp_path, ("a",))
    (dist / "a" / "style.css").write_text("x", encoding="utf-8")
    (dist / "assets").mkdir()
    (dist / "assets" / "app.js").write_text("x", encoding="utf-8")
    assert c.routes_in(dist) == {"a"}


def test_a_stub_redirects_into_the_docs_base(tmp_path):
    c.write_stub(tmp_path, "01-quickstart")
    html = (tmp_path / "01-quickstart" / "index.html").read_text(encoding="utf-8")
    assert f'href="{c.BASE}01-quickstart/"' in html
    assert f'content="0; url={c.BASE}01-quickstart/"' in html
    # A redirect stub in the index would compete with the real page for the
    # same query, so it says so.
    assert 'name="robots" content="noindex"' in html


def test_the_root_is_never_stubbed(tmp_path):
    """It is the landing page. A stub there would redirect the front door."""
    c.write_stub(tmp_path, "")
    assert not (tmp_path / "index.html").exists()


# --- assembly ---------------------------------------------------------------


def test_assembly_puts_the_docs_under_the_base_and_the_landing_at_the_root(site, capsys):
    out = site()
    assert c.assemble(out) == 0

    assert (out / "index.html").read_text(encoding="utf-8") == "<html>landing</html>"
    assert (out / "docs" / "01-quickstart" / "index.html").is_file()
    # And the pre-move path still resolves, which is the whole point.
    assert f'href="{c.BASE}01-quickstart/"' in (
        out / "01-quickstart" / "index.html").read_text(encoding="utf-8")
    printed = capsys.readouterr().out
    assert "3 docs route(s)" in printed
    assert "2 pre-move route(s) still resolving" in printed


def test_the_oracle_catches_a_route_that_would_404(site, capsys):
    """The test that matters: a page the build no longer emits.

    `parity` is listed as published but absent from the build, so no stub is
    written for it and the pre-move URL dies. Every link to it in this family
    and outside it breaks at once.
    """
    out = site(routes=("", "01-quickstart"), published=["01-quickstart", "parity"])
    assert c.assemble(out) == 1
    err = capsys.readouterr().err
    assert "would 404" in err
    assert "/parity/" in err


def test_an_empty_oracle_is_refused(site):
    """A check that passes over nothing passes forever."""
    out = site(published=[])
    with pytest.raises(SystemExit, match="is empty"):
        c.assemble(out)


def test_a_missing_oracle_is_refused(site):
    out = site(write_routes=False)
    with pytest.raises(SystemExit, match="nothing to check against"):
        c.assemble(out)


def test_assembly_needs_a_docs_build(site):
    out = site(write_dist=False)
    with pytest.raises(SystemExit, match="no Starlight build"):
        c.assemble(out)


def test_assembly_needs_a_landing_page(site):
    out = site(write_landing=False)
    with pytest.raises(SystemExit, match="no landing page"):
        c.assemble(out)


def test_assembly_clears_what_was_there_before(site):
    """The workflow writes badges into this directory AFTER the script runs.

    If a stale tree survived, a page deleted in this build would keep being
    published from the last one.
    """
    out = site()
    stale = out / "gone" / "index.html"
    stale.parent.mkdir(parents=True)
    stale.write_text("stale", encoding="utf-8")

    assert c.assemble(out) == 0
    assert not stale.exists()


def test_a_renamed_route_is_stubbed_from_the_alias_table(site, monkeypatch):
    """ALIASES is empty today, so nothing in the repo exercises this branch."""
    monkeypatch.setattr(c, "ALIASES", {"old-name": "01-quickstart"})
    out = site(published=["01-quickstart", "parity", "old-name"])

    assert c.assemble(out) == 0
    assert f'href="{c.BASE}01-quickstart/"' in (
        out / "old-name" / "index.html").read_text(encoding="utf-8")


def test_an_alias_with_no_target_points_at_the_docs_root(site, monkeypatch):
    monkeypatch.setattr(c, "ALIASES", {"old-index": ""})
    out = site(published=["01-quickstart", "parity", "old-index"])

    assert c.assemble(out) == 0
    assert f'href="{c.BASE}"' in (out / "old-index" / "index.html").read_text(encoding="utf-8")


# --- absolute references ----------------------------------------------------

WITH_REF = '<html><head><link href="{href}"></head><body>doc</body></html>'


def test_a_reference_to_something_never_shipped_is_reported(site, capsys):
    """Why this check exists: the built pages asked for a favicon that was not
    in the tree, and the sibling site 404ed it on all 44 pages unnoticed."""
    out = site(page=WITH_REF.format(href="/fabric-emulator/favicon.svg"))
    assert c.assemble(out) == 1
    err = capsys.readouterr().err
    assert "these references 404" in err
    assert "/fabric-emulator/favicon.svg" in err
    assert "3 page(s)" in err


def test_a_reference_that_resolves_passes(site, capsys):
    out = site(page=WITH_REF.format(href="/fabric-emulator/favicon.svg"))
    # Assemble once to lay the tree down, then ship the asset and re-run.
    c.assemble(out)
    (out / "favicon.svg").write_text("<svg/>", encoding="utf-8")
    capsys.readouterr()

    assert c.check_absolute_refs(out) == 0
    assert "absolute reference(s) in the built pages resolve" in capsys.readouterr().out


def test_a_reference_to_a_directory_resolves_through_its_index(site):
    out = site(page=WITH_REF.format(href="/fabric-emulator/docs/parity/"))
    assert c.assemble(out) == 0


def test_a_reference_outside_the_project_prefix_is_dangling(site, capsys):
    """Nothing is published outside the prefix, so this is a 404 in production
    even though the path looks plausible."""
    out = site(page=WITH_REF.format(href="/parity/"))
    assert c.assemble(out) == 1
    assert "/parity/" in capsys.readouterr().err


def test_no_absolute_reference_at_all_is_a_failure_not_a_pass(site, capsys):
    """The silent case. If the pattern stops matching, the check guards nothing
    and reports success, which is indistinguishable from a clean build."""
    out = site(page="<html><body>no refs at all</body></html>")
    assert c.assemble(out) == 1
    assert "has stopped matching" in capsys.readouterr().err


# --- routing, which the preview must get exactly right ----------------------


@pytest.mark.parametrize(
    ("request_path", "expected"),
    [
        # Pages serves the USER site at /, never this project.
        ("/", ("redirect", c.SITE_PREFIX)),
        ("/fabric-emulator", ("redirect", c.SITE_PREFIX)),
        ("/fabric-emulator/", ("serve", "/")),
        ("/fabric-emulator/docs/parity/", ("serve", "/docs/parity/")),
        ("/fabric-emulator/coverage-go.json?v=2", ("serve", "/coverage-go.json")),
        ("/fabric-emulator/parity/#legend", ("serve", "/parity/")),
        ("/other-project/", ("404", "")),
        # The docs are NOT at the root any more; that was the move.
        ("/docs/", ("404", "")),
        ("", ("redirect", c.SITE_PREFIX)),
    ],
)
def test_resolve_routes_the_way_pages_does(request_path, expected):
    assert c.resolve(request_path) == expected


def test_the_scripts_own_self_test_passes(capsys):
    assert c.self_test() == 0
    assert "self-test passed" in capsys.readouterr().out


def test_the_self_test_would_notice_a_stub_writer_that_stopped_writing(monkeypatch, capsys):
    """The control on the control.

    A self-test that only ever passes is decoration. Neutering write_stub must
    turn it red.
    """
    monkeypatch.setattr(c, "write_stub", lambda out, route: None)
    assert c.self_test() == 1
    assert "FAIL" in capsys.readouterr().out


# --- the preview server -----------------------------------------------------


def test_the_preview_needs_an_assembled_tree(tmp_path):
    with pytest.raises(SystemExit, match="no assembled site"):
        c.serve(tmp_path / "nothing", 0)


def test_the_preview_answers_as_pages_would(site, monkeypatch):
    """Driven over a real socket, because the point of this server is the
    difference between it and `astro dev` — and that difference is entirely in
    how it answers requests."""
    import http.server

    out = site()
    assert c.assemble(out) == 0

    servers = []
    real = http.server.HTTPServer

    def capture(address, handler):
        # Port 0: the OS picks a free one, so a busy runner cannot flake this.
        server = real(("127.0.0.1", 0), handler)
        servers.append(server)
        return server

    monkeypatch.setattr(http.server, "HTTPServer", capture)
    thread = threading.Thread(target=lambda: c.serve(out, 0), daemon=True)
    thread.start()
    for _ in range(200):
        if servers:
            break
        threading.Event().wait(0.01)
    assert servers, "the preview server never started"
    port = servers[0].server_address[1]
    base = f"http://127.0.0.1:{port}"

    try:
        with urllib.request.urlopen(f"{base}/fabric-emulator/") as response:
            assert response.status == 200
            assert "landing" in response.read().decode()

        with urllib.request.urlopen(f"{base}/fabric-emulator/01-quickstart/") as response:
            # The pre-move URL answers, with the stub that forwards it.
            assert c.BASE + "01-quickstart/" in response.read().decode()

        # Outside the project prefix nothing is published, and the preview must
        # say so rather than helpfully serving it.
        with pytest.raises(urllib.error.HTTPError) as exc:
            urllib.request.urlopen(f"{base}/somewhere-else/")
        assert exc.value.code == 404

        # The root redirects rather than answering: on the real host it belongs
        # to the user site.
        request = urllib.request.Request(f"{base}/", method="GET")
        opener = urllib.request.build_opener(NoRedirect)
        with pytest.raises(urllib.error.HTTPError) as exc:
            opener.open(request)
        assert exc.value.code == 302
        assert exc.value.headers["Location"] == c.SITE_PREFIX
    finally:
        servers[0].shutdown()
        thread.join(timeout=5)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """urllib follows a 302 by default, which would hide the thing under test."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


# --- the command line -------------------------------------------------------


def test_main_dispatches_to_the_self_test(monkeypatch, capsys):
    monkeypatch.setattr(sys, "argv", ["assemble_site.py", "--self-test"])
    assert c.main() == 0
    assert "self-test passed" in capsys.readouterr().out


def test_main_assembles_into_the_named_directory(site, monkeypatch):
    out = site()
    monkeypatch.setattr(sys, "argv", ["assemble_site.py", "--out", str(out)])
    assert c.main() == 0
    assert (out / "docs" / "parity" / "index.html").is_file()


def test_main_dispatches_to_the_preview(site, monkeypatch):
    out = site()
    seen = {}
    monkeypatch.setattr(c, "serve", lambda site_path, port: seen.update(
        site=site_path, port=port) or 0)
    monkeypatch.setattr(sys, "argv", [
        "assemble_site.py", "--serve", "--site", str(out), "--port", "4321"])

    assert c.main() == 0
    assert seen == {"site": out, "port": 4321}


def test_the_repositorys_own_route_oracle_is_not_empty():
    """A control against the repo itself.

    Every test above builds its own oracle, so all of them would pass with
    website/published-routes.txt deleted or blank — which is precisely the
    state in which the real gate checks nothing.
    """
    routes = [r.strip() for r in c.ROUTES.read_text(encoding="utf-8").splitlines() if r.strip()]
    assert len(routes) > 100, f"{c.ROUTES} lists only {len(routes)} route(s)"
    assert "parity" in routes
    assert not any(r.startswith("/") or r.endswith("/") for r in routes), (
        "routes are stored without slashes; the assembler joins them as paths")
    assert len(set(routes)) == len(routes), "a duplicated route hides a missing one in the count"
