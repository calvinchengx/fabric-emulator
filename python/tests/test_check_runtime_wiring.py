"""The wiring checker, and the ways it can be quietly useless.

`scripts/check_runtime_wiring.py` exists because four defects in this
repository shared one shape: a member implemented, unit-tested, green, and
non-functional — because the test supplied a value the product never produced.

A checker for that class has two failure modes, and both are silent:

  * crediting a WRITER that is not one, so a real orphan reads as wired. Two
    real instances are pinned below — a test file, and the checker's own waiver
    list, each of which made it green over a genuine gap;
  * failing to find a READ, so a name is never examined at all.

The tests are written against those, not against the current output — a test
asserting today's counts would fail on every legitimate change and tell nobody
anything.
"""
import importlib.util
import sys
import textwrap
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_runtime_wiring", REPO / "scripts" / "check_runtime_wiring.py")
assert spec and spec.loader
wiring = importlib.util.module_from_spec(spec)
sys.modules["check_runtime_wiring"] = wiring
spec.loader.exec_module(wiring)


def write(tmp_path, name, body):
    path = tmp_path / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(textwrap.dedent(body), encoding="utf-8")
    return path


# --- finding what is READ -----------------------------------------------------

@pytest.mark.parametrize("source, expected", [
    ('import os\nx = os.environ.get("A_NAME")', {"A_NAME"}),
    ('import os\nx = os.getenv("A_NAME")', {"A_NAME"}),
    ('import os\nx = os.environ["A_NAME"]', {"A_NAME"}),
    ('from os import environ\nx = environ.get("A_NAME")', {"A_NAME"}),
    # A WRITE is not a read. Counting it would make any module that exports a
    # value look like one that depends on it.
    ('import os\nos.environ["A_NAME"] = "v"', set()),
    ('x = {}.get("A_NAME")', set()),
])
def test_environment_reads_are_found_exactly(tmp_path, source, expected):
    assert wiring.env_reads(write(tmp_path, "m.py", source)) == expected


@pytest.mark.parametrize("source, expected", [
    ('x = context.get("aKey")', {"aKey"}),
    ('x = ctx.get("aKey")', {"aKey"}),
    ('x = runtime.context.get("aKey")', {"aKey"}),
    ('x = other.get("aKey")', set()),
])
def test_context_reads_are_found_exactly(tmp_path, source, expected):
    assert wiring.context_reads(write(tmp_path, "m.py", source)) == expected


# --- refusing to credit a WRITER that is not one ------------------------------

@pytest.mark.parametrize("name", [
    "internal/api/thing_test.go",     # Go tests live BESIDE their package...
    "python/tests/test_thing.py",     # ...and Python tests under tests/
    "python/thing_test.py",
    "python/conftest.py",
])
def test_a_test_file_is_never_a_writer(name):
    """This is the bug that made the first version useless, and it is worth
    pinning: `rootNotebookId` was credited to
    `internal/api/notebook_reference_run_test.go`, so deleting the agent's
    wiring left the check green. A test supplying what the product does not is
    the exact defect this file exists to find — being fooled by one is the
    worst available failure."""
    assert wiring._is_test(REPO / name) is True


def test_product_code_is_a_writer():
    assert wiring._is_test(REPO / "internal" / "api" / "notebookdrive.go") is False
    assert wiring._is_test(REPO / "docker-compose.yml") is False


def test_the_checker_never_credits_itself():
    """It names every name it checks, in WAIVED and in its docstring, and
    `"NAME":` is one of the writer patterns — so it reported its own waiver
    list as the thing that set them, and every waiver read as stale."""
    assert wiring.writers("SPARK_AGENT_URL", set()) != [str(
        Path("scripts") / "check_runtime_wiring.py")]
    assert str(wiring.SELF).endswith("check_runtime_wiring.py")


def test_a_context_key_is_not_wired_by_an_e2e_harness():
    """An env var is CONFIGURATION — a compose file demonstrating one is a real
    answer. A context key is RUNTIME STATE, and only the product can produce
    it, so a harness setting one proves nothing about a real run."""
    assert wiring._is_test(REPO / "e2e" / "conformance" / "live.py") is True
    assert wiring._is_test(REPO / "e2e" / "conformance" / "live.py",
                           allow_e2e=True) is False


# --- the report ---------------------------------------------------------------

def test_the_committed_tree_is_fully_wired():
    """The property this check asserts about THIS repository. It is the
    assertion that would have caught session.stop and nbResPath before either
    shipped."""
    assert wiring.main(["--strict"]) == 0


def test_a_waiver_that_is_no_longer_true_fails(monkeypatch):
    """Waivers expire. Two of `check_notebookutils_surface`'s had gone stale
    before anyone reread them — one excusing a gap that had been closed — so a
    waiver nobody rechecks is how a defect becomes furniture."""
    monkeypatch.setitem(wiring.WAIVED, "A_NAME_NOBODY_READS", "invented")
    assert wiring.main(["--strict"]) == 1


def test_an_empty_read_set_is_reported_as_vacuous(monkeypatch, tmp_path):
    """A check that finds nothing to check must say so rather than pass. This
    is how a moved directory turns a gate into a decoration."""
    monkeypatch.setattr(wiring, "READERS", [tmp_path])
    (tmp_path / "m.py").write_text("x = 1", encoding="utf-8")
    with pytest.raises(wiring.Unreadable, match="vacuous"):
        wiring.collect()


def test_a_missing_reader_tree_is_an_error(monkeypatch, tmp_path):
    monkeypatch.setattr(wiring, "READERS", [tmp_path / "gone"])
    with pytest.raises(wiring.Unreadable, match="missing"):
        wiring.collect()
