"""Tests for the CI docs-lane classifier.

WHY THIS EXISTS SEPARATELY FROM ITS `--self-test`. The script carries its own
case list and runs it in CI beside every real classification, which is the
right shape for a gate: it proves itself on the machine that depends on it. But
that self-test contributes nothing to the coverage floor and cannot exercise
the parts around `classify` — the `$GITHUB_OUTPUT` write, the stdin path, the
exit status — and those are exactly where a gate fails open. `docs_only=false`
is always safe and always plausible, so a broken classifier does not announce
itself; it just quietly runs the full suite forever, or worse, quietly skips it.

The asymmetry that shapes every case below: a wrong `true` skips ninety jobs on
a change that needed them, and a wrong `false` costs runner minutes. So the
tests press hardest on everything that could produce a `true`.
"""
import io
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import docs_only_change as c  # noqa: E402


@pytest.mark.parametrize(
    "path",
    [
        "docs/01-quickstart.md",
        "docs/parity.md",
        "website/scripts/sync-docs.mjs",
        "website/src/pages/index.astro",
        "README.md",
        "AUDIT.md",
        "SECURITY.md",
    ],
)
def test_documentation_paths(path):
    assert c.is_doc(path)


@pytest.mark.parametrize(
    "path",
    [
        "internal/api/items.go",
        "python/spark_agent/agent.py",
        "scripts/docs_only_change.py",
        ".github/workflows/ci.yml",
        # Named deliberately: examples are EXECUTED by the medallion jobs, and
        # a Go test asserts the links in examples/README.md. A readme is not
        # documentation for this purpose merely because it is a readme.
        "examples/README.md",
        "e2e/two-context/run.py",
        # A path that merely starts with the same letters as a doc tree.
        "docsite/thing.md",
        "websites.txt",
    ],
)
def test_paths_that_are_not_documentation(path):
    assert not c.is_doc(path)


def test_a_documentation_edit_takes_the_cheap_lane():
    docs_only, reason = c.classify("M\tdocs/01-quickstart.md\nA\twebsite/src/pages/index.astro")
    assert docs_only
    assert "2 changed path(s)" in reason


def test_one_code_file_disqualifies_the_whole_change():
    docs_only, reason = c.classify("M\tdocs/01-quickstart.md\nM\tinternal/api/items.go")
    assert not docs_only
    assert "internal/api/items.go" in reason


def test_a_deleted_page_runs_everything():
    """The exception found by looking rather than assumed.

    internal/api/examples_readme_test.go reads examples/README.md and fails
    when a (../docs/<file>) link resolves to nothing. Adding or editing a page
    cannot break that test; removing one can.
    """
    docs_only, reason = c.classify("D\tdocs/55-gone.md")
    assert not docs_only
    assert "deleted" in reason
    assert "examples/README.md" in reason


def test_a_renamed_page_runs_everything():
    docs_only, reason = c.classify("R100\tdocs/a.md\tdocs/b.md")
    assert not docs_only
    assert "renamed" in reason


def test_a_rename_out_of_the_doc_trees_names_the_path_not_the_rename():
    """Both reasons apply; the more specific one is the useful message."""
    docs_only, reason = c.classify("R100\tdocs/a.md\tinternal/a.go")
    assert not docs_only
    assert "internal/a.go" in reason


@pytest.mark.parametrize(
    ("diff", "expected"),
    [
        ("", "no changed files"),
        ("   \n\n", "no changed files"),
        ("M", "unparseable"),
    ],
)
def test_an_unreadable_diff_is_not_a_clean_one(diff, expected):
    """Absence of evidence answers `false`.

    An empty diff is a range we failed to resolve — a shallow checkout, a merge
    commit read the wrong way, a tag. Reading it as "nothing changed, skip
    everything" converts a missing verdict into a green one.
    """
    docs_only, reason = c.classify(diff)
    assert not docs_only
    assert expected in reason


def test_the_scripts_own_self_test_passes():
    assert c.self_test() == 0


def test_main_writes_the_github_output_and_says_why(tmp_path, monkeypatch, capsys):
    out = tmp_path / "gh-output"
    monkeypatch.setenv("GITHUB_OUTPUT", str(out))
    monkeypatch.setattr(sys, "stdin", io.StringIO("M\tdocs/01-quickstart.md\n"))

    assert c.main([]) == 0

    assert out.read_text(encoding="utf-8") == "docs_only=true\n"
    printed = capsys.readouterr().out
    assert "docs_only=true" in printed
    assert "documentation" in printed


def test_main_appends_rather_than_truncating(tmp_path, monkeypatch):
    """$GITHUB_OUTPUT is shared by every step in the job.

    Truncating it would silently drop outputs another step already wrote, and
    the symptom would appear in a different job entirely.
    """
    out = tmp_path / "gh-output"
    out.write_text("something_else=1\n", encoding="utf-8")
    monkeypatch.setenv("GITHUB_OUTPUT", str(out))
    monkeypatch.setattr(sys, "stdin", io.StringIO("M\tinternal/api/items.go\n"))

    assert c.main([]) == 0
    assert out.read_text(encoding="utf-8") == "something_else=1\ndocs_only=false\n"


def test_main_outside_actions_still_answers(monkeypatch, capsys):
    """No $GITHUB_OUTPUT is a laptop, not an error."""
    monkeypatch.delenv("GITHUB_OUTPUT", raising=False)
    monkeypatch.setattr(sys, "stdin", io.StringIO("M\tinternal/api/items.go\n"))

    assert c.main([]) == 0
    assert "docs_only=false" in capsys.readouterr().out


def test_self_test_flag_short_circuits(monkeypatch, capsys):
    """--self-test must not read stdin: the CI step runs it with none."""
    monkeypatch.setattr(sys, "stdin", None)
    assert c.main(["--self-test"]) == 0
    assert "cases pass" in capsys.readouterr().out


def test_the_self_test_would_notice_a_broken_classifier(monkeypatch, capsys):
    """The control.

    A self-test whose cases all agree with a broken rule is decoration. This
    replaces the rule with one that always answers `true` and asserts the case
    list goes red — so the suite proves the self-test can fail, not merely that
    it passes today.
    """
    monkeypatch.setattr(c, "classify", lambda _diff: (True, "everything is docs"))
    assert c.self_test() == 1
    assert "FAILED" in capsys.readouterr().out
