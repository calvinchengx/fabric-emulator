r"""A drift guard that can itself drift silently is worth nothing.

check_comment_drift exists because a Go comment is where this repo keeps its
reasoning and nothing reads it -- the failure that motivated it was a
justification that stayed confidently present-tense for a week after the
measurement retiring it had landed. That argument applies with full force to
the checker, and it has three places it could quietly stop finding anything:

  * the COMMENT SCANNER is hand-walked over four Go states. If it ever mistook
    a string for a comment, every documentation URL in the tree becomes a
    finding; if it mistook a comment for code, the checker goes silent and
    still exits 0. Both directions are asserted below.
  * CLASS 1 is restricted to repo-rooted paths, which is what took it from 84
    false positives to nought. A restriction one notch tighter finds nothing
    and still passes.
  * CLASS 3 is a pin, so it is only as good as the counting. Counting the
    checker's own RETIRED table, or counting code as well as comments, would
    fail on a clean tree; counting nothing passes on a dirty one.

So the tests that matter most are the ones asserting each class FAILS on a
planted defect. A checker that always passes is indistinguishable from a
checker that is working -- the exact failure docs/10 catalogues -- so both
currently-clean classes are tested against synthetic drift rather than trusted
because the tree is green.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_comment_drift as c  # noqa: E402

# --- the comment scanner ------------------------------------------------------

def bodies(source):
    return [body for _, body in c.comment_spans(source)]


def test_a_line_comment_is_read():
    assert bodies("// hello\nvar x = 1\n") == [" hello"]


def test_a_block_comment_is_read_across_lines():
    assert c.comment_spans("/* one\ntwo */\n").__next__() == (1, "/* one")
    assert [line for line, _ in c.comment_spans("/* one\ntwo */\n")] == [1, 2]


def test_a_slash_slash_inside_a_string_is_not_a_comment():
    """The concession the docstring claims: a URL is not a comment.

    Without it every `https://learn.microsoft.com/...` in the tree is read as a
    comment naming a Microsoft Learn slug, and class 1 floods with findings
    about the oracle's own filenames.
    """
    assert bodies('var u = "https://example.com/a/b.go"\n') == []


def test_a_slash_slash_inside_a_raw_literal_is_not_a_comment():
    assert bodies("var u = `see docs/998 // here`\n") == []


def test_an_escaped_quote_does_not_end_an_interpreted_string():
    assert bodies('var s = "a \\" // not a comment"\n') == []


def test_a_backslash_does_not_escape_inside_a_raw_literal():
    """The one asymmetry between Go's two string forms.

    In a raw literal a backslash is an ordinary character, so treating it as an
    escape would swallow the closing backtick and read the rest of the file as
    string -- silencing every comment after it.
    """
    assert bodies("var s = `a \\`\n// a real comment\n") == [" a real comment"]


def test_a_comment_after_a_string_on_the_same_line_is_read():
    assert bodies('var u = "https://x/y.go" // see internal/api/livy.go\n') == [
        " see internal/api/livy.go"
    ]


def test_an_unterminated_interpreted_string_does_not_swallow_the_file():
    """Unbalanced input must not silence the rest of the tree.

    A Go file like this does not compile, but the checker runs on whatever is
    tracked -- including a file mid-edit -- and going quiet is the one failure
    mode it must not have.
    """
    assert bodies('var s = "oops\n// a real comment\n') == [" a real comment"]


def test_line_numbers_are_reported_from_one():
    assert list(c.comment_spans("var x = 1\n\n// third line\n")) == [(3, " third line")]


# --- class 1: dead repo paths -------------------------------------------------

INDEX = ({"internal", "docs"}, {"internal", "internal/api", "internal/api/livy.go"})


def test_a_repo_rooted_path_that_is_missing_is_reported():
    found = list(c.dead_paths("// see internal/api/gone.go\n", INDEX))
    assert found == [(1, "internal/api/gone.go")]


def test_a_repo_rooted_path_that_exists_is_not_reported():
    assert list(c.dead_paths("// see internal/api/livy.go\n", INDEX)) == []


@pytest.mark.parametrize("token", [
    "onelake/onelake-access-api.md",   # a Microsoft Learn slug
    "data.json",                       # a payload filename, no directory
    "entityTypes/Pipeline.json",       # ADF's schema, a foreign tree
    "/jobs/etl.py",                    # a path on a Databricks cluster
])
def test_a_path_that_is_not_this_repos_is_left_alone(token):
    """The 84 false positives the naive form produced, one of each kind.

    Each is a citation of something real that this repository does not contain,
    and reporting them is how a checker teaches people to skim past it.
    """
    assert list(c.dead_paths(f"// see {token}\n", INDEX)) == []


def test_a_go_symbol_is_not_read_as_a_path():
    assert list(c.dead_paths("// internal/tsql.DataFlows is the type\n", INDEX)) == []


def test_trailing_punctuation_is_stripped_before_lookup():
    assert list(c.dead_paths("// defined in internal/api/livy.go.\n", INDEX)) == []


# --- class 2: dead doc references ---------------------------------------------

KNOWN = {"20": "docs/20-livy.md", "43": "docs/43-activity-completion-plan.md"}


def test_a_missing_numbered_doc_is_reported():
    assert list(c.dead_doc_refs("// see docs/998\n", KNOWN)) == [(1, "docs/998")]


def test_an_existing_numbered_doc_is_not_reported():
    assert list(c.dead_doc_refs("// the Livy precedent (docs/20)\n", KNOWN)) == []


def test_a_zero_padded_reference_resolves_to_the_same_document():
    """Both spellings are in the tree's comments, and they mean one document."""
    assert list(c.dead_doc_refs("// see docs/20 and docs/020\n", KNOWN)) == []


def test_a_docs_path_is_not_read_as_a_doc_number():
    assert list(c.dead_doc_refs("// docs/43-activity-completion-plan.md\n", KNOWN)) == []


# --- class 3: retired vocabulary ----------------------------------------------

def test_a_retired_term_is_counted_in_comments():
    assert c.retired_counts("x.go", "// a connector leaf\n") == {"connector leaf": 1}


def test_a_retired_term_is_counted_case_insensitively():
    """The tree writes it as CONNECTOR LEAF for emphasis and lowercase in prose.

    A retirement applies to the idea, not to a capitalisation of it.
    """
    assert c.retired_counts("x.go", "// a CONNECTOR LEAF\n") == {"connector leaf": 1}


def test_a_retired_term_in_code_is_not_counted():
    """Only comments are in scope; an identifier is the code's business."""
    assert c.retired_counts("x.go", 'var s = "connector leaf"\n') == {}


def test_a_file_at_its_pin_is_not_drift():
    assert list(c.retired_drift({"internal/api/pipelines.go": {"connector leaf": 1}})) == []


def test_a_file_over_its_pin_is_drift():
    found = list(c.retired_drift({"internal/api/pipelines.go": {"connector leaf": 2}}))
    assert found == [("connector leaf", "internal/api/pipelines.go", 2, 1)]


def test_a_new_file_using_a_retired_term_is_drift():
    """The reintroduction this class exists to catch."""
    found = list(c.retired_drift({"internal/api/brand_new.go": {"connector leaf": 1}}))
    assert found == [("connector leaf", "internal/api/brand_new.go", 1, 0)]


def test_a_file_under_its_pin_is_not_drift():
    """Removing an occurrence is always allowed, and never fails the build.

    The pin is a ceiling, not an equality: a comment recording the retirement
    may be deleted or reworded without a checker demanding it stay.
    """
    assert list(c.retired_drift({})) == []


def test_every_retired_entry_carries_a_reason():
    """An entry without a written reason is a silencer rather than a record."""
    for term, record in c.RETIRED.items():
        assert record["reason"].strip(), term
        assert record["pinned"], term


# --- the tree itself, and the report ------------------------------------------

def test_the_tree_is_clean():
    found, retired = c.findings()
    assert found == [], found
    assert retired == [], retired


def test_the_checker_excludes_itself():
    """Its RETIRED table names every retired term, as an argument.

    Counting them would count the question as an answer -- and, because the
    table is keyed by file, would report the checker as drifting on every run.
    """
    assert set(c.SELF).isdisjoint(c.go_files())


def test_strict_exits_non_zero_on_drift(monkeypatch, capsys):
    monkeypatch.setattr(c, "findings",
                        lambda: ([("path", "a.go", 7, "internal/gone.go")], []))
    monkeypatch.setattr(sys, "argv", ["check_comment_drift.py", "--strict"])
    assert c.main() == 1
    assert "internal/gone.go" in capsys.readouterr().out


def test_without_strict_drift_is_reported_but_exits_zero(monkeypatch, capsys):
    monkeypatch.setattr(c, "findings",
                        lambda: ([("doc", "a.go", 7, "docs/998")], []))
    monkeypatch.setattr(sys, "argv", ["check_comment_drift.py"])
    assert c.main() == 0
    assert "docs/998" in capsys.readouterr().out


def test_retired_drift_is_reported_with_its_reason(monkeypatch, capsys):
    """The reason is the point: it tells the reader what replaced the idea."""
    monkeypatch.setattr(c, "findings",
                        lambda: ([], [("connector leaf", "a.go", 2, 1)]))
    monkeypatch.setattr(sys, "argv", ["check_comment_drift.py", "--strict"])
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "pinned at 1" in out
    assert "#490" in out


def test_a_clean_tree_prints_what_it_counted(monkeypatch, capsys):
    monkeypatch.setattr(c, "findings", lambda: ([], []))
    monkeypatch.setattr(sys, "argv", ["check_comment_drift.py"])
    assert c.main() == 0
    assert "no drift" in capsys.readouterr().out
