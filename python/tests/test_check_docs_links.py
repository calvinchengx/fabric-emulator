"""Tests for the doc-link and sidebar-reachability check.

The checker guards two things the site build does not: a link to a page that
does not exist (the build exits 0 and publishes a 404) and a page missing from
the sidebar (published, but unreachable by navigation).

The interesting part is which files count as published. That is decided by
`DOC_RE` in website/scripts/sync-docs.mjs and is DERIVED here rather than
restated, so these tests pin the derivation itself: change the site's published
set and the checker follows, and if the declaration ever moves the checker must
fail loudly rather than guard the wrong set.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_docs_links as c  # noqa: E402


@pytest.fixture
def site(tmp_path, monkeypatch):
    """A docs tree, an astro config and a sync-docs.mjs we control."""

    def build(docs=(), slugs=(), doc_re=r"^(\d{2}-.*|parity)\.md$", readme=None,
              parity_versions=False, sync_body=None, synthesized="overview"):
        docs_dir = tmp_path / "docs"
        docs_dir.mkdir(exist_ok=True)
        for name, *body in [(d,) if isinstance(d, str) else d for d in docs]:
            (docs_dir / name).write_text(body[0] if body else "# doc\n")
        config = tmp_path / "astro.config.mjs"
        config.write_text("sidebar: [\n" + "\n".join(f"  {{ slug: '{s}' }}," for s in slugs) + "\n]\n")
        sync = tmp_path / "sync-docs.mjs"
        # The fixture must model BOTH things the checker derives from the
        # generator: the published-set regex and the pages it synthesizes.
        default_body = f"const DOC_RE = /{doc_re}/;\n"
        if synthesized:
            default_body += f"writeFileSync(join(OUT, '{synthesized}.md'), frontmatter + body);\n"
        sync.write_text(sync_body if sync_body is not None else default_body)
        root = tmp_path / "root"
        root.mkdir(exist_ok=True)
        if readme is not None:
            (root / "README.md").write_text(readme)
        monkeypatch.setattr(c, "DOCS", docs_dir)
        monkeypatch.setattr(c, "CONFIG", config)
        monkeypatch.setattr(c, "SYNC_DOCS", sync)
        monkeypatch.setattr(c, "ROOT", root)
        monkeypatch.setattr(c, "PARITY_VERSIONS", tmp_path / ("pv.mjs" if parity_versions else "absent.mjs"))
        if parity_versions:
            (tmp_path / "pv.mjs").write_text("// generator\n")

    return build


def test_clean_site_passes(site, monkeypatch):
    site(docs=["01-quickstart.md", "parity.md"], slugs=["01-quickstart", "parity"])
    assert c.problems() == []
    # main() parses sys.argv, which under pytest holds pytest's own arguments.
    monkeypatch.setattr(sys, "argv", ["check_docs_links.py"])
    assert c.main() == 0


def test_link_to_a_missing_page_is_reported(site):
    site(docs=[("01-quickstart.md", "see [next](02-install.md)\n")], slugs=["01-quickstart"])
    assert any("02-install.md" in p for p in c.problems())


def test_page_missing_from_the_sidebar_is_reported(site):
    site(docs=["01-quickstart.md", "02-install.md"], slugs=["01-quickstart"])
    assert any("02-install.md is not in the sidebar" in p for p in c.problems())


def test_sidebar_entry_without_a_page_is_reported(site):
    site(docs=["01-quickstart.md"], slugs=["01-quickstart", "99-ghost"])
    assert any("99-ghost" in p for p in c.problems())


def test_readme_link_into_docs_is_checked(site):
    site(docs=["01-quickstart.md"], slugs=["01-quickstart"],
         readme="[gone](docs/99-nope.md)\n")
    assert any("README.md links to docs/99-nope.md" in p for p in c.problems())


def test_a_synthesized_page_needs_no_sidebar_entry(site):
    """And the exemption is the generator's, not this test's."""
    site(docs=["overview.md", "01-quickstart.md"], slugs=["01-quickstart"],
         doc_re=r"^(\d{2}-.*|parity|overview)\.md$", synthesized="overview")
    assert c.problems() == []


def test_a_sidebar_slug_for_a_synthesized_page_is_not_dangling(site):
    """The live case: `overview` is first in the sidebar and has no docs file."""
    site(docs=["01-quickstart.md"], slugs=["01-quickstart", "overview"], synthesized="overview")
    assert c.problems() == []


def test_a_generator_that_synthesizes_nothing_is_an_error(site):
    """Empty would mean 'exempt nothing', which reads exactly like 'all fine'."""
    site(docs=["01-quickstart.md"], slugs=["01-quickstart"],
         sync_body=r"const DOC_RE = /^(\d{2}-.*)\.md$/;" + "\n")
    with pytest.raises(SystemExit) as exc:
        c.problems()
    assert "synthesizes no page" in str(exc.value)


def test_generated_parity_history_is_exempt_only_with_the_generator(site):
    site(docs=["01-quickstart.md"], slugs=["01-quickstart", "parity-history"],
         parity_versions=True)
    assert c.problems() == []
    site(docs=["01-quickstart.md"], slugs=["01-quickstart", "parity-history"],
         parity_versions=False)
    assert any("parity-history" in p for p in c.problems())


def test_the_published_set_follows_the_sites_own_regex(site):
    # A file the site does NOT publish is not required to be in the sidebar.
    site(docs=["01-quickstart.md", "notes.md"], slugs=["01-quickstart"])
    assert c.problems() == []


def test_a_moved_declaration_fails_loudly(site):
    site(docs=["01-quickstart.md"], slugs=["01-quickstart"],
         sync_body="const PUBLISHED = /whatever/;\n")
    with pytest.raises(SystemExit) as exc:
        c.problems()
    assert "DOC_RE" in str(exc.value)


def test_an_uncompilable_declaration_fails_loudly(site):
    site(docs=["01-quickstart.md"], slugs=["01-quickstart"], doc_re="(unclosed")
    with pytest.raises(SystemExit) as exc:
        c.problems()
    assert "does not compile" in str(exc.value)


def test_strict_returns_nonzero(site, capsys, monkeypatch):
    site(docs=["01-quickstart.md", "02-install.md"], slugs=["01-quickstart"])
    monkeypatch.setattr(sys, "argv", ["check_docs_links.py", "--strict"])
    assert c.main() == 1
    assert "not in the sidebar" in capsys.readouterr().out
