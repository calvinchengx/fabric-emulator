"""The docs-reachability check had no tests of its own.

It caught a real miss immediately after landing — `docs/39-run-multiple-parity-plan.md`
was published by the site and linked from no sidebar group, so nothing reached
it — but the checker itself was executed by no test, which is the state it
exists to prevent elsewhere.

The interesting part is the PUBLISHED pattern: it mirrors `DOC_RE` in
website/scripts/sync-docs.mjs, and a mismatch there means the checker guards a
different set of files than the site actually publishes. That is the drift worth
pinning.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_docs_sidebar as c  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parents[2]


@pytest.fixture
def site(tmp_path, monkeypatch):
    """A docs/ directory and an astro config we control."""
    def build(docs=(), slugs=(), config=None):
        d = tmp_path / "docs"
        d.mkdir(exist_ok=True)
        for name in docs:
            (d / name).write_text("# doc\n")
        cfg = tmp_path / "astro.config.mjs"
        if config is None:
            config = "sidebar: [\n" + "\n".join(f"  {{ slug: '{s}' }}," for s in slugs) + "\n]\n"
        cfg.write_text(config)
        monkeypatch.setattr(c, "DOCS", d)
        monkeypatch.setattr(c, "CONFIG", cfg)
    return build


def test_passes_when_every_published_doc_has_a_slug(site, capsys):
    site(docs=["01-quickstart.md", "parity.md"], slugs=["01-quickstart", "parity"])
    assert c.main() == 0
    assert "all reachable" in capsys.readouterr().out


def test_fails_and_names_the_unreachable_doc(site, capsys):
    # The failure this shipped to catch: a numbered doc added without a slug.
    site(docs=["01-quickstart.md", "39-plan.md"], slugs=["01-quickstart"])
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "docs/39-plan.md" in out
    assert "astro.config.mjs" in out, "the message must say where to add it"


# --- the PUBLISHED pattern must match what the site actually publishes --------

@pytest.mark.parametrize("name,published", [
    ("01-quickstart.md", True),      # NN- prefixed
    ("39-run-multiple.md", True),
    ("parity.md", True),             # named exceptions
    ("engine-matrix.md", True),
    ("README.md", False),            # not published by the site
    ("witnesses.json", False),       # not markdown
    ("1-short.md", False),           # NN- means exactly two digits
    ("demo/flow.md", False),         # only the top level is globbed
])
def test_only_site_published_docs_are_required(site, capsys, name, published):
    # A doc the site does NOT publish must not be demanded in the sidebar —
    # that would fail the build for a file nobody can navigate to anyway.
    if "/" in name:
        pytest.skip("nested paths are excluded by the glob, not the pattern")
    # A known-good doc rides along so the "no slugs" and "no published docs"
    # guards cannot fire — otherwise every case fails for the wrong reason and
    # the test proves nothing about `name`. It is deliberately NOT one of the
    # parametrised names: an anchor that is also the subject has a slug, and
    # the published cases would pass by accident.
    site(docs=["00-anchor.md", name], slugs=["00-anchor"])
    rc = c.main()
    assert (rc == 1) == published, f"{name}: published={published} but rc={rc}"


def test_the_pattern_matches_the_sites_own_regex():
    # The checker guards the set sync-docs.mjs publishes. If that regex is
    # edited and this one is not, the check silently guards a different set.
    mjs = (ROOT / "website" / "scripts" / "sync-docs.mjs").read_text()
    assert "DOC_RE" in mjs, "sync-docs.mjs no longer defines DOC_RE"
    for name in ("01-quickstart.md", "parity.md", "engine-matrix.md"):
        assert c.PUBLISHED.match(name), name
    for name in ("README.md", "AUDIT.md"):
        assert not c.PUBLISHED.match(name), name


# --- refusing to pass vacuously ----------------------------------------------

def test_a_config_that_yields_no_slugs_fails(site, capsys):
    # A parse finding nothing must fail, not pass: an astro config that moved
    # to another quoting style would otherwise disarm the check completely.
    site(docs=["01-quickstart.md"], config="sidebar: []\n")
    assert c.main() == 1
    assert "parsed no slugs" in capsys.readouterr().out


def test_no_published_docs_fails(site, capsys):
    site(docs=["README.md"], slugs=["01-quickstart"])
    assert c.main() == 1
    assert "parsed no published docs" in capsys.readouterr().out


def test_a_missing_config_fails(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(c, "CONFIG", tmp_path / "nope.mjs")
    assert c.main() == 1
    assert "not found" in capsys.readouterr().out


def test_the_real_repo_passes_its_own_check(capsys):
    assert c.main() == 0, capsys.readouterr().out
