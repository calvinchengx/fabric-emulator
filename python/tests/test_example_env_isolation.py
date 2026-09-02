"""The examples' fixtures module must not leak its import-time env into the suite.

This is a test-isolation regression, so what it pins is an ORDER, not a value:
the two node ids below pass individually and, before `python/tests/conftest.py`
restored `os.environ` around each test, failed together. Running them in one
child process is the whole assertion — an in-process check could not make it,
because by the time this test body runs the fixture has already cleaned up.

`examples/contoso-fixtures/common.py` calls `apply_notebook_env()` at import,
which writes NOTEBOOKUTILS_* into the real `os.environ`. `apply_notebook_env`
then declines to overwrite an already-set key, on purpose, so the leaked value
survives into `fabric-target`'s own test of that function and it reads back the
wrong URL. See python/tests/conftest.py.
"""
import os
import pathlib
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parents[2]

LEAKER = "python/tests/test_example_sql_endpoint.py"
VICTIM = ("python/fabric-target/tests/test_fabric_target.py"
          "::test_apply_notebook_env_wires_the_shim_for_this_process")


def test_importing_the_fixtures_module_does_not_break_a_later_test():
    """The minimal reproducer, run as its own process."""
    # The child must start from a clean notebook context or it proves nothing:
    # an ambient NOTEBOOKUTILS_FABRIC_URL in the developer's shell would fail the
    # victim test whatever this conftest does, and an ambient FABRIC_TARGET
    # changes which URL the leak carries.
    env = {k: v for k, v in os.environ.items()
           if not k.startswith("NOTEBOOKUTILS_") and k != "FABRIC_TARGET"}
    run = subprocess.run(
        [sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider", LEAKER, VICTIM],
        cwd=REPO, env=env, capture_output=True, text=True,
    )
    assert run.returncode == 0, (
        "the fixtures module leaked its import-time environment into a later "
        f"test:\n{run.stdout}\n{run.stderr}"
    )
