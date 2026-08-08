"""The docs-reachability check had no tests of its own.

It caught a real miss immediately after landing — `docs/39-run-multiple-parity-plan.md`
was published by the site and linked from no sidebar group, so nothing reached
it — but the checker itself was executed by no test, which is the state it
exists to prevent elsewhere.

The interesting part is which set of files counts as published. That is decided
by `DOC_RE` in website/scripts/sync-docs.mjs, and the checker used to keep a
copy of it — a mismatch there meant the checker guarded a different set than the
site published, silently. The copy is gone; the pattern is derived, and the
tests below pin the derivation rather than a restatement of it.
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


# THE COUPLING THIS USED TO PRETEND TO PIN.
#
# The checker guards the set sync-docs.mjs publishes. It used to keep its own
# copy of that regex under a comment saying "Mirrors DOC_RE in …", and the test
# here asserted `"DOC_RE" in mjs` — that the NAME still appeared in the file —
# then checked the local copy against a hand-written list of filenames.
#
# Both halves passed for the wrong reason. Editing DOC_RE to publish a
# different set left the name present and the hand-written list matching the
# stale copy, so the check silently guarded a set the site no longer used. The
# assertion matched a phrase that co-occurs with the claim, not the claim.
#
# The pattern is now DERIVED from the .mjs, so these test the derivation — and
# the first one cannot pass against a hardcoded copy, which is the point.

def test_the_pattern_is_read_from_the_site_not_copied(tmp_path):
    # A DIFFERENT DOC_RE must produce a DIFFERENT published set. A copy in
    # check_docs_sidebar.py would ignore this file and fail here.
    mjs = tmp_path / "sync-docs.mjs"
    mjs.write_text("const DOC_RE = /^(chapter-.*|glossary)\\.md$/;\n")
    pattern = c.published_pattern(mjs)
    assert pattern.match("chapter-01.md"), "the site's own regex was not used"
    assert pattern.match("glossary.md")
    assert not pattern.match("01-quickstart.md"), (
        "the OLD hardcoded pattern is still in force — the derivation is not wired up")


def test_the_real_sites_pattern_publishes_the_documented_set():
    pattern = c.published_pattern()
    for name in ("01-quickstart.md", "39-run-multiple.md", "parity.md", "engine-matrix.md"):
        assert pattern.match(name), name
    for name in ("README.md", "AUDIT.md", "1-short.md"):
        assert not pattern.match(name), name


def test_a_missing_declaration_fails_loudly(tmp_path):
    # Falling back to a copied default here would rebuild the exact bug this
    # change removes: a check guarding a guessed set, silently.
    mjs = tmp_path / "sync-docs.mjs"
    mjs.write_text("const OTHER_RE = /^x$/;\n")
    with pytest.raises(c.PatternUnavailable, match="DOC_RE"):
        c.published_pattern(mjs)


def test_an_absent_file_fails_loudly(tmp_path):
    with pytest.raises(c.PatternUnavailable, match="cannot read"):
        c.published_pattern(tmp_path / "nope.mjs")


def test_a_regex_python_cannot_compile_fails_loudly(tmp_path):
    # JS and Python share the subset this pattern uses; if DOC_RE ever leaves
    # it, that must be a loud translation decision, not a silent mismatch.
    mjs = tmp_path / "sync-docs.mjs"
    # JS named groups are `(?<name>…)`; Python spells them `(?P<name>…)` and
    # rejects the JS form. (Lookbehind would NOT work as a probe here — both
    # languages accept `(?<=…)`, and the first draft of this test used it and
    # failed to raise, which is the same "assert the substance" lesson again.)
    mjs.write_text("const DOC_RE = /^(?<kind>\\d{2})-.*\\.md$/;\n")
    with pytest.raises(c.PatternUnavailable):
        c.published_pattern(mjs)


def test_main_reports_an_underivable_pattern_instead_of_guessing(monkeypatch, capsys, tmp_path):
    monkeypatch.setattr(c, "SYNC_DOCS", tmp_path / "gone.mjs")
    assert c.main() == 1
    assert "cannot read" in capsys.readouterr().out


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
