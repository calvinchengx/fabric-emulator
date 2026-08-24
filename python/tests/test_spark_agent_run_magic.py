"""`%run` splices another notebook's code into THIS namespace.

That is the whole difference from `notebookutils.notebook.run`, which starts a
separate session and hands back an exit value. A `%run` that ran the code
anywhere else would look identical until the caller tried to use what it
defined — which is why the tests below assert on the CALLER's namespace and not
on a return value.

The rewrite is pure and lives in run_magic.py so it can be tested without an
engine, a control plane, or a notebook.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import pytest  # noqa: E402
import run_magic  # noqa: E402

# --- the rewrite --------------------------------------------------------------


def test_a_bare_run_becomes_a_call():
    assert run_magic.expand("%run helpers") == "__fabric_run_notebook__('helpers', {})"


def test_arguments_are_parsed_as_json():
    out = run_magic.expand('%run helpers {"p": 1, "q": "x"}')
    assert out == "__fabric_run_notebook__('helpers', {'p': 1, 'q': 'x'})"


def test_a_quoted_name_keeps_its_spaces():
    assert run_magic.expand('%run "my notebook"') == \
        "__fabric_run_notebook__('my notebook', {})"


def test_indentation_is_preserved_so_a_run_inside_a_block_still_parses():
    """`%run` inside an `if` must stay inside it — a rewrite that moved the
    call to column zero would change the program."""
    out = run_magic.expand("if flag:\n    %run helpers")
    assert out == "if flag:\n    __fabric_run_notebook__('helpers', {})"
    compile(out, "<test>", "exec")


def test_source_without_a_run_is_returned_byte_identical():
    """This runs on EVERY statement. A rewrite that reformatted innocent code
    would be a far worse bug than the feature is a win."""
    code = "x = 1\ndef f():\n    return '%runner'\n"
    assert run_magic.expand(code) is code


def test_a_run_in_a_string_is_not_a_magic():
    """Matching one would rewrite the user's DATA — the same mistake the T-SQL
    dialect layer refuses when it declines to tokenize inside quotes."""
    code = 'msg = "type %run helpers to begin"'
    assert run_magic.expand(code) == code


def test_a_commented_run_is_not_a_magic():
    code = "# %run helpers\nx = 1"
    assert run_magic.expand(code) == code


def test_unparseable_arguments_are_carried_not_guessed():
    """A caller who wrote something this cannot parse should see their own text
    in the error, not a silently empty parameter set."""
    out = run_magic.expand("%run helpers not-json")
    assert "__unparsed__" in out and "not-json" in out


def test_a_json_array_is_not_a_parameter_object():
    out = run_magic.expand('%run helpers [1, 2]')
    assert "__unparsed__" in out


def test_several_runs_in_one_cell_all_rewrite():
    out = run_magic.expand("%run a\nx = 1\n%run b")
    assert out.count("__fabric_run_notebook__") == 2
    compile(out, "<test>", "exec")


# --- picking the code out of a definition -------------------------------------


def _nb(*cells):
    return {"cells": [dict(c) for c in cells]}


def test_only_code_cells_are_spliced():
    definition = _nb(
        {"cell_type": "markdown", "source": "# a heading"},
        {"cell_type": "code", "source": "x = 1"},
    )
    assert run_magic.code_cells(definition) == ["x = 1"]


def test_a_non_python_cell_is_skipped_rather_than_spliced():
    """Splicing Scala into a Python namespace fails with a SyntaxError naming
    the CALLER's cell — the defect internal/notebook/celllang.go exists to
    prevent, one layer down."""
    definition = _nb(
        {"cell_type": "code", "source": "val x = 1",
         "metadata": {"language": "scala"}},
        {"cell_type": "code", "source": "y = 2"},
    )
    assert run_magic.code_cells(definition) == ["y = 2"]


def test_a_source_list_is_joined_the_way_ipynb_stores_it():
    definition = _nb({"cell_type": "code", "source": ["x = 1\n", "y = 2\n"]})
    assert run_magic.code_cells(definition) == ["x = 1\ny = 2\n"]


def test_a_definition_arriving_as_json_text_is_parsed():
    import json
    definition = json.dumps(_nb({"cell_type": "code", "source": "x = 1"}))
    assert run_magic.code_cells(definition) == ["x = 1"]


def test_a_plain_script_is_treated_as_one_cell():
    """`# CELL ****` source format is not ipynb JSON, and refusing it would
    make %run work only for notebooks stored one particular way."""
    assert run_magic.code_cells("x = 1\ny = 2") == ["x = 1\ny = 2"]


def test_an_empty_definition_yields_nothing_to_run():
    assert run_magic.code_cells(_nb()) == []
    assert run_magic.code_cells(None) == []


# --- the semantic that matters ------------------------------------------------


def test_the_referenced_code_lands_in_the_callers_namespace():
    """THE WHOLE POINT. After `%run helpers`, what helpers defined must be
    usable by the cell that ran it."""
    g = {}
    exec(compile(run_magic.expand("%run helpers"), "<c>", "exec"),  # noqa: S102
         {**g, "__fabric_run_notebook__": lambda n, a: g.update(helper=lambda: 42)})
    assert g["helper"]() == 42


def test_the_fast_path_skips_source_that_cannot_contain_a_magic():
    """`expand` runs on every statement in every session. The substring check
    is what keeps that free for the overwhelming majority of cells that have no
    `%run` in them at all."""
    code = "x = 1"
    assert run_magic.expand(code) is code


# --- the runner: where the code actually lands --------------------------------


def _runner(namespace, notebooks):
    return run_magic.make_runner(namespace, lambda name: notebooks[name])


def test_the_referenced_notebook_defines_things_in_the_callers_namespace():
    """THE SEMANTIC that separates %run from notebookutils.notebook.run. A
    runner that executed the code anywhere else would look identical until the
    caller tried to use what it defined."""
    g = {}
    _runner(g, {"helpers": {"cells": [
        {"cell_type": "code", "source": "def double(n):\n    return n * 2"},
    ]}})("helpers")
    assert g["double"](21) == 42


def test_parameters_are_bound_before_the_code_runs():
    """The referenced notebook reads them as ordinary globals — the way a
    parameters cell works."""
    g = {}
    _runner(g, {"helpers": {"cells": [
        {"cell_type": "code", "source": "result = scale * 2"},
    ]}})("helpers", {"scale": 5})
    assert g["result"] == 10


def test_the_caller_keeps_what_it_already_had():
    """%run splices INTO the namespace; it does not replace it."""
    g = {"existing": "kept"}
    _runner(g, {"helpers": {"cells": [{"cell_type": "code", "source": "added = 1"}]}})("helpers")
    assert g["existing"] == "kept" and g["added"] == 1


def test_every_code_cell_runs_in_order():
    g = {}
    _runner(g, {"helpers": {"cells": [
        {"cell_type": "code", "source": "order = []"},
        {"cell_type": "code", "source": "order.append('first')"},
        {"cell_type": "code", "source": "order.append('second')"},
    ]}})("helpers")
    assert g["order"] == ["first", "second"]


def test_a_nested_run_is_expanded_too():
    """A helper notebook may pull in another. Without expanding the referenced
    cells, a nested `%run` would reach Python as a syntax error."""
    notebooks = {
        "outer": {"cells": [{"cell_type": "code", "source": "%run inner"}]},
        "inner": {"cells": [{"cell_type": "code", "source": "deep = 'reached'"}]},
    }
    g = {}
    g[run_magic.HELPER] = run_magic.make_runner(g, lambda n: notebooks[n])
    g[run_magic.HELPER]("outer")
    assert g["deep"] == "reached"


def test_a_notebook_with_nothing_to_run_says_so():
    """Rather than succeeding silently, which reads as 'the helpers loaded'."""
    g = {}
    with pytest.raises(ValueError, match="no runnable Python cells"):
        _runner(g, {"empty": {"cells": [
            {"cell_type": "markdown", "source": "# just prose"}]}})("empty")


def test_unparseable_arguments_name_the_callers_own_text():
    g = {}
    with pytest.raises(ValueError, match="not-json"):
        _runner(g, {"helpers": {"cells": []}})("helpers", {"__unparsed__": "not-json"})


def test_the_error_names_the_documented_syntax():
    """A caller who got the arguments wrong needs the shape, not just a
    complaint."""
    g = {}
    with pytest.raises(ValueError, match=r'%run notebook \{"param": value\}'):
        _runner(g, {"h": {"cells": []}})("h", {"__unparsed__": "oops"})


def test_the_target_and_the_rest_cannot_compete_for_the_same_characters():
    """The ReDoS fix (CodeQL, high), asserted STRUCTURALLY — because a timing
    test here would be vacuous and I checked.

    The first pattern was `\\s+(?P<target>…|\\S+)(?P<rest>.*)$`: `\\S+` and `.*`
    can both match the same characters, so every split between them is a
    candidate the engine may have to try. That ambiguity is real and is what
    the scanner flagged.

    What I could NOT do is reproduce it as slowness. On CPython's engine the
    match succeeds greedily and returns in microseconds for every shape I
    tried — long runs of `!`, of quotes, of spaces, and the exact
    `'%run !' + '!!'…` case reported. So a `time.monotonic()` guard would have
    passed on the VULNERABLE pattern too, and a test that cannot fail on the
    bug it names is worse than no test.

    Asserted instead: the separator is whitespace, and the target cannot
    contain whitespace. That is the property that makes the split
    deterministic, and it is checkable.
    """
    pattern = run_magic._RUN.pattern
    assert r"[ \t]+(?P<rest>" in pattern, \
        "the rest must be reached through a whitespace separator"
    assert r"\s+(?P<target>" not in pattern, \
        r"an unbounded \s+ before the target reintroduces the ambiguity"
    # And the property that separator implies: a target stops at whitespace.
    m = run_magic._RUN.match("%run one two three")
    assert m.group("target") == "one"
    assert m.group("rest") == "two three"


def test_a_target_and_arguments_are_split_on_whitespace_only():
    """The boundary that makes the pattern linear must not change behaviour:
    a target still ends at the first space, and the rest still starts after
    it."""
    out = run_magic.expand('%run helpers   {"p": 1}')
    assert out == "__fabric_run_notebook__('helpers', {'p': 1})"


def test_a_run_with_no_target_is_not_a_magic():
    """`%run` alone has nothing to run. Rewriting it to a call with an empty
    name would fail later, further from the cause."""
    assert run_magic.expand("%run") == "%run"
    assert run_magic.expand("%run   ") == "%run   "


def test_a_newline_cannot_be_the_separator():
    """This matches ONE LINE at a time. If the separator matched a newline, a
    bare `%run` would swallow the statement below it as its arguments."""
    out = run_magic.expand("%run helpers\nx = 1")
    assert out.splitlines()[1] == "x = 1"
