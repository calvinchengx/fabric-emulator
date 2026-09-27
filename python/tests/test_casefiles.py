"""scripts/casefiles.py: a case file no runner should trust is refused by name.

Each rule here exists because the file is read by more than one runner. A case
with no id cannot be selected or witnessed; two with one id make `-k` and a
witness ambiguous; a case with no `why` loses the reasoning a comment used to
carry; and an `executed_by` naming a runner that is gone reads as executed when
nothing runs it.
"""
import json

import casefiles  # on the path via conftest.py
import pytest


def write(tmp_path, stem="suite-a", **overrides):
    doc = {"suite": stem, "about": "what one case means",
           "cases": [{"id": "one", "why": "the break it guards"}]}
    doc.update(overrides)
    path = tmp_path / f"{stem}.json"
    path.write_text(json.dumps(doc), encoding="utf-8")
    return path


def test_a_valid_file_has_no_problems_and_loads(tmp_path):
    write(tmp_path)
    assert casefiles.problems(tmp_path / "suite-a.json", tmp_path) == []
    assert casefiles.load("suite-a", tmp_path, tmp_path) == [
        {"id": "one", "why": "the break it guards"}]


@pytest.mark.parametrize("overrides, fragment", [
    ({"suite": "other"}, "want 'suite-a'"),
    ({"about": "  "}, "\"about\""),
    ({"cases": []}, "non-empty list"),
    ({"extra": 1}, "unknown top-level key"),
    ({"cases": ["not an object"]}, "must be an object"),
    ({"cases": [{"id": "Not_Kebab", "why": "x"}]}, "kebab-case"),
    ({"cases": [{"id": "one", "why": "x"}, {"id": "one", "why": "y"}]}, "duplicate id"),
    ({"cases": [{"id": "one"}]}, "\"why\""),
    ({"cases": [{"id": "one", "why": "x", "executed_by": "run.py"}]}, "list of paths"),
    ({"cases": [{"id": "one", "why": "x", "executed_by": ["e2e/gone/run.py"]}]},
     "does not exist"),
])
def test_each_rule_names_what_is_wrong(tmp_path, overrides, fragment):
    path = write(tmp_path, **overrides)
    found = casefiles.problems(path, tmp_path)
    assert any(fragment in p for p in found), found


def test_unreadable_json_and_a_non_object_are_refused(tmp_path):
    bad = tmp_path / "broken.json"
    bad.write_text("{", encoding="utf-8")
    assert "not readable JSON" in casefiles.problems(bad, tmp_path)[0]
    listy = tmp_path / "listy.json"
    listy.write_text("[]", encoding="utf-8")
    assert "must be an object" in casefiles.problems(listy, tmp_path)[0]


def test_load_raises_with_every_problem_not_only_the_first(tmp_path):
    write(tmp_path, about="", cases=[{"id": "BAD"}])
    with pytest.raises(casefiles.CaseFileError) as exc:
        casefiles.load("suite-a", tmp_path, tmp_path)
    message = str(exc.value)
    assert "\"about\"" in message and "kebab-case" in message and "\"why\"" in message


def test_executed_by_selects_the_cases_that_name_a_runner(tmp_path):
    (tmp_path / "run.py").write_text("", encoding="utf-8")
    cases = [{"id": "a", "why": "x", "executed_by": ["run.py"]},
             {"id": "b", "why": "x"}]
    assert casefiles.executed_by(cases, "run.py") == {"a"}


def test_text_joins_lines_exactly():
    # A verbatim statement keeps its blank lines and adds no trailing newline.
    assert casefiles.text(["select", "", "  1"]) == "select\n\n  1"
    assert casefiles.text("select 1") == "select 1"
    with pytest.raises(TypeError):
        casefiles.text(["select", 1])


def test_check_reports_every_file_and_fails_on_any(tmp_path, capsys):
    write(tmp_path, "good")
    assert casefiles.check(tmp_path, tmp_path) == 0
    assert "1 suite(s), 1 case(s)" in capsys.readouterr().out
    write(tmp_path, "bad", about="")
    assert casefiles.check(tmp_path, tmp_path) == 1
    assert "bad.json" in capsys.readouterr().err


def test_the_repositorys_own_case_files_pass():
    assert casefiles.main(["--check"]) == 0


def test_main_without_check_prints_usage_and_refuses(capsys):
    assert casefiles.main([]) == 2
    assert "--check" in capsys.readouterr().out
