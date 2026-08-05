"""The pair checker is what keeps the two medallion halves comparable.

`scripts/check_example_parity.py` enforces the promise that within a pair
exactly one step changes — bronze to silver, imperative PySpark against
declarative dbt-fabricspark — because that single difference is the entire
reason for shipping two examples instead of one. If the checker stopped
detecting divergence, the pair would drift and the comparison job downstream
would be comparing two different pipelines while reporting agreement.

The seams tested here are the ones that decide a verdict: `listed`, which
decides whether a divergence was declared; `steps`, which reads step order out
of a pipeline; and `check`, which turns those into divergences.
"""
import importlib.util
import subprocess
import sys
import textwrap
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_example_parity", REPO / "scripts" / "check_example_parity.py")
cp = importlib.util.module_from_spec(spec)
sys.modules["check_example_parity"] = cp
spec.loader.exec_module(cp)


@pytest.fixture(autouse=True)
def restore_module_globals():
    """The checker keeps its configuration in module globals, and the fixtures
    below narrow them. Without restoring, a later test — the one that runs
    against the REAL examples — inherits a stripped CONTENT and reports every
    legitimate exemption as a divergence."""
    saved = {k: getattr(cp, k) for k in ("ROOT", "EXAMPLES", "CONTENT", "ONLY_IN")}
    yield
    for k, v in saved.items():
        setattr(cp, k, v)


# --- listed: exact name, or a child of a declared directory ---------------

def test_exact_name_is_listed():
    assert cp.listed("silver.py", ["silver.py"])


def test_child_of_a_declared_directory_is_listed():
    assert cp.listed("models/silver/a.sql", ["models"])


def test_a_prefix_that_is_not_a_path_boundary_is_not_listed():
    """`models_extra/` must not be covered by a declaration of `models`, or a
    whole undeclared tree hides behind a neighbour's name."""
    assert not cp.listed("models_extra/a.sql", ["models"])


def test_unrelated_file_is_not_listed():
    assert not cp.listed("gold.py", ["silver.py", "models"])


# --- steps: order is part of the contract ---------------------------------

def write_pipeline(tmp_path, body):
    p = tmp_path / "pipeline.py"
    p.write_text(body, encoding="utf-8")
    return p


def test_steps_are_read_in_declaration_order(tmp_path):
    p = write_pipeline(tmp_path, textwrap.dedent("""\
        STEPS = [
            ("bronze", bronze),
            ("silver", silver),
            ("gold", gold),
        ]
        """))
    assert cp.steps(p) == ["bronze", "silver", "gold"]


def test_lines_that_are_not_steps_are_ignored(tmp_path):
    p = write_pipeline(tmp_path, textwrap.dedent("""\
        # ("commented", nope)
        STEPS = [
            ("bronze", bronze),
        ]
        print("not a step")
        """))
    assert cp.steps(p) == ["bronze"]


# --- check: the divergences that matter -----------------------------------

def make_pair(tmp_path, a_files, b_files):
    """Two examples in a throwaway git repo, since `tracked` asks git.

    CONTENT and ONLY_IN are module globals the real pairs share, so they are
    narrowed here too — otherwise the repo's own exemptions (README.md,
    uv.lock, silver_dbt/) would quietly excuse divergences these cases mean
    to provoke.
    """
    examples = tmp_path / "examples"
    for name, files in (("a", a_files), ("b", b_files)):
        for rel, body in files.items():
            f = examples / name / rel
            f.parent.mkdir(parents=True, exist_ok=True)
            f.write_text(body, encoding="utf-8")
    subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
    subprocess.run(["git", "add", "-A"], cwd=tmp_path, check=True)
    cp.ROOT = tmp_path
    cp.EXAMPLES = examples
    cp.CONTENT = {"silver.py": "the step under comparison",
                  "pipeline.py": "docstring + the silver step label"}
    cp.ONLY_IN = {}
    return {"label": "test pair", "a": "a", "b": "b",
            "only_in": {}, "extra_steps_in_b": []}


PIPE = 'STEPS = [\n    ("bronze", bronze),\n    ("silver", silver),\n]\n'


def test_identical_pair_has_no_divergence(tmp_path):
    files = {"pipeline.py": PIPE, "bronze.py": "x = 1\n", "silver.py": "y = 1\n"}
    pair = make_pair(tmp_path, files, dict(files))
    assert cp.check(pair) == []


def test_a_file_differing_outside_the_declared_set_is_a_divergence(tmp_path):
    a = {"pipeline.py": PIPE, "bronze.py": "x = 1\n", "silver.py": "y = 1\n"}
    b = dict(a, **{"bronze.py": "x = 2\n"})
    pair = make_pair(tmp_path, a, b)
    problems = cp.check(pair)
    assert any("bronze.py" in p for p in problems)


def test_the_declared_silver_file_may_differ(tmp_path):
    """The one permitted difference: it is the point of the pair."""
    a = {"pipeline.py": PIPE, "bronze.py": "x = 1\n", "silver.py": "imperative\n"}
    b = dict(a, **{"silver.py": "declarative\n"})
    assert cp.check(make_pair(tmp_path, a, b)) == []


def test_a_file_present_in_only_one_half_is_a_divergence(tmp_path):
    a = {"pipeline.py": PIPE, "bronze.py": "x\n", "silver.py": "y\n"}
    b = dict(a, **{"extra.py": "surprise\n"})
    problems = cp.check(make_pair(tmp_path, a, b))
    assert any("extra.py" in p for p in problems)


def test_a_file_only_in_one_half_may_be_declared(tmp_path):
    a = {"pipeline.py": PIPE, "bronze.py": "x\n", "silver.py": "y\n"}
    b = dict(a, **{"extra.py": "declared\n"})
    pair = make_pair(tmp_path, a, b)
    pair["only_in"] = {"extra.py": "declared: b only"}
    assert cp.check(pair) == []


def test_reordered_steps_are_a_divergence(tmp_path):
    """Same files, different order — the pipelines are no longer comparable."""
    a = {"pipeline.py": PIPE, "bronze.py": "x\n", "silver.py": "y\n"}
    reordered = 'STEPS = [\n    ("silver", silver),\n    ("bronze", bronze),\n]\n'
    b = dict(a, **{"pipeline.py": reordered})
    problems = cp.check(make_pair(tmp_path, a, b))
    assert any("step" in p.lower() for p in problems)


def test_a_declared_extra_step_is_allowed(tmp_path):
    a = {"pipeline.py": PIPE, "bronze.py": "x\n", "silver.py": "y\n"}
    with_extra = PIPE.replace("]\n", '    ("compare", compare),\n]\n')
    b = dict(a, **{"pipeline.py": with_extra})
    pair = make_pair(tmp_path, a, b)
    pair["extra_steps_in_b"] = ["compare"]
    assert cp.check(pair) == []


def test_a_stale_extra_step_exemption_is_reported(tmp_path):
    """If the extra step is gone, the allowance silently starts covering a step
    that drifted out of the other pipeline — so the exemption must be removed."""
    files = {"pipeline.py": PIPE, "bronze.py": "x\n", "silver.py": "y\n"}
    pair = make_pair(tmp_path, files, dict(files))
    pair["extra_steps_in_b"] = ["compare"]
    problems = cp.check(pair)
    assert any("remove the exemption" in p for p in problems)


def test_an_untracked_file_says_so(tmp_path):
    """The list comes from git, so a file written but never added is invisible.
    Without the hint the author re-runs, sees the same failure, and hunts the
    wrong bug."""
    a = {"pipeline.py": PIPE, "bronze.py": "x\n", "silver.py": "y\n"}
    pair = make_pair(tmp_path, a, dict(a))
    (cp.EXAMPLES / "b" / "late.py").write_text("untracked\n", encoding="utf-8")
    (cp.EXAMPLES / "a" / "late.py").write_text("tracked\n", encoding="utf-8")
    subprocess.run(["git", "add", "examples/a/late.py"], cwd=tmp_path, check=True)
    problems = cp.check(pair)
    assert any("untracked" in p and "git add" in p for p in problems)


def test_a_missing_half_is_reported_not_crashed(tmp_path):
    pair = make_pair(tmp_path, {"pipeline.py": PIPE}, {"pipeline.py": PIPE})
    pair["b"] = "does-not-exist"
    problems = cp.check(pair)
    assert problems and any("does-not-exist" in p for p in problems)


# --- the real repository ---------------------------------------------------

def test_the_actual_examples_pass():
    cp.ROOT = REPO
    cp.EXAMPLES = REPO / "examples"
    assert cp.main() == 0
