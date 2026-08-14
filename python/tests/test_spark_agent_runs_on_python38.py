"""`python/spark_agent/` must run on Python 3.8, because the JVM image ships it.

WHY. The agent is mounted into `apache/spark:3.5.5-...-python3-ubuntu` by the
JVM overlay, and that image's interpreter is **Python 3.8**. Everything else in
this repo runs on 3.12, ruff targets py312, and nothing anywhere said the agent
is different — so 3.11+ code lands in it and passes every gate.

It already happened. `eventstream_kafka.py` imported `datetime.UTC`, a 3.11
alias, and the JVM eventstream suite died on:

    ImportError: cannot import name 'UTC' from 'datetime'
    (/usr/lib/python3.8/datetime.py)

ruff had actively pushed it there: UP017 rewrites `timezone.utc` to
`datetime.UTC` under a py312 target. The lint was correct about the target it
was told about and wrong about the one that matters, and only a **cron-only**
workflow could see the result — so it sat on main.

TWO CHECKS, because they catch different things. `ast.parse(feature_version=…)`
rejects 3.9+ SYNTAX but accepts `datetime.UTC` happily: that is valid syntax and
a missing attribute at runtime. The denylist covers those.
"""
import ast
import pathlib

import pytest

AGENT = pathlib.Path(__file__).resolve().parents[1] / "spark_agent"

# The interpreter in apache/spark's python3 image. Raise this only when the
# image does, and change docker/spark-runtime with it.
FLOOR = (3, 8)

# Valid syntax on 3.8, missing at runtime. Each entry is (needle, since, use).
DENY = [
    ("from datetime import UTC", "3.11", "from datetime import timezone; timezone.utc"),
    ("datetime.UTC", "3.11", "datetime.timezone.utc"),
    ("from typing import Self", "3.11", "a string annotation"),
    ("itertools.pairwise", "3.10", "zip(x, x[1:])"),
    ("from typing import TypeAlias", "3.10", "a plain assignment"),
]


def _sources():
    files = sorted(AGENT.rglob("*.py"))
    assert files, f"no sources found under {AGENT} — this test would pass vacuously"
    return files


@pytest.mark.parametrize("path", _sources(), ids=lambda p: p.name)
def test_the_agent_parses_on_the_image_interpreter(path):
    """3.9+ SYNTAX would be a SyntaxError inside the container, where the only
    symptom is a job that dies before it logs anything useful."""
    src = path.read_text(encoding="utf-8")
    try:
        ast.parse(src, filename=str(path), feature_version=FLOOR)
    except SyntaxError as e:
        pytest.fail(
            f"{path.name}:{e.lineno} is not valid Python {FLOOR[0]}.{FLOOR[1]}, "
            f"which is what the JVM Spark image runs: {e.msg}")


@pytest.mark.parametrize("path", _sources(), ids=lambda p: p.name)
def test_the_agent_avoids_apis_that_are_newer_than_the_image(path):
    """Valid syntax, absent at runtime — the case ast.parse cannot see."""
    for lineno, line in enumerate(path.read_text(encoding="utf-8").split("\n"), 1):
        if line.lstrip().startswith("#"):
            continue
        for needle, since, instead in DENY:
            assert needle not in line, (
                f"{path.name}:{lineno} uses {needle!r}, added in Python {since}; "
                f"the JVM Spark image runs {FLOOR[0]}.{FLOOR[1]}. Use {instead}.")


def test_ruff_is_told_not_to_reintroduce_the_alias():
    """UP017 rewrites `timezone.utc` into the 3.11 alias under a py312 target.
    Without the per-file ignore this is a fight the linter wins on every edit."""
    cfg = (AGENT.parents[1] / "pyproject.toml").read_text(encoding="utf-8")
    assert '"python/spark_agent/*" = ["UP017"]' in cfg, (
        "the per-file ignore is gone; ruff will rewrite timezone.utc back to "
        "datetime.UTC and break the JVM image again")


@pytest.mark.parametrize("path", _sources(), ids=lambda p: p.name)
def test_the_agent_avoids_pep604_unions_in_annotations(path):
    """`int | None` is valid SYNTAX on 3.8 and a TypeError when the annotation
    is EVALUATED, which is at import time for a def. So ast.parse accepts it and
    the container dies on it — the gap a surviving mutant exposed here.

    Allowed when the module opts into lazy annotations, because then they are
    strings and never evaluated.
    """
    src = path.read_text(encoding="utf-8")
    tree = ast.parse(src, filename=str(path))
    lazy = any(
        isinstance(n, ast.ImportFrom) and n.module == "__future__"
        and any(a.name == "annotations" for a in n.names)
        for n in tree.body)
    if lazy:
        return

    annotations = []
    for node in ast.walk(tree):
        if isinstance(node, (ast.AnnAssign, ast.arg)) and node.annotation:
            annotations.append(node.annotation)
        elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.returns:
            annotations.append(node.returns)

    for ann in annotations:
        for sub in ast.walk(ann):
            if isinstance(sub, ast.BinOp) and isinstance(sub.op, ast.BitOr):
                pytest.fail(
                    f"{path.name}:{sub.lineno} uses PEP 604 `X | Y` in an "
                    f"annotation. That is evaluated at import on Python "
                    f"{FLOOR[0]}.{FLOOR[1]} and raises TypeError. Use "
                    f"typing.Optional/Union, or add "
                    f"`from __future__ import annotations`.")


def test_the_denylist_is_not_empty():
    """A parametrized scan over an empty list passes while checking nothing."""
    assert len(DENY) >= 3
