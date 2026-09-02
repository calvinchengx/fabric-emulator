"""Process-global state this suite must not carry between tests.

`examples/contoso-fixtures/common.py` calls `apply_notebook_env(_T)` at MODULE
IMPORT time — deliberately, because a Fabric notebook step imports it and must
get the notebook runtime context wired for the resolved target before it calls
anything. Three tests here import that module, so three tests write real
NOTEBOOKUTILS_* keys into the real `os.environ`.

`monkeypatch` cannot undo that. Those tests do call `monkeypatch.delenv(k,
raising=False)` for the NOTEBOOKUTILS_* keys first, but delenv records an undo
entry only for a key that WAS set; keys the import then creates from nothing are
invisible to it and survive the test, the module, and the session.

What they broke was not another example test but `fabric-target`'s own
`test_apply_notebook_env_wires_the_shim_for_this_process`, which passed alone and
failed in the full run. `apply_notebook_env` does not overwrite an already-set
value (`if override or not os.environ.get(k)`) — documented, intentional, and the
whole reason an explicit export or compose file wins. Given a leaked key it
correctly declines to set the value the later test asserts. The production code
is right; the leak was the defect.

So the environment is snapshotted and restored around EVERY test in this
directory. It is not scoped to the three importers on purpose: the leak reached a
test in a different package, which is exactly the kind of coupling a per-file fix
leaves in place for the next module-level side effect to rediscover.
"""
import os

import pytest


@pytest.fixture(autouse=True)
def _restore_os_environ():
    """Snapshot os.environ, hand the test the real one, then put it back.

    Restored in place rather than by rebinding `os.environ`: os.environ is a
    live mapping whose __setitem__ also calls putenv, and code under test holds
    references to it (and to `os.getenv`). Rebinding would leave the real
    process environment — the one a subprocess inherits — untouched.
    """
    snapshot = dict(os.environ)
    try:
        yield
    finally:
        for key in [k for k in os.environ if k not in snapshot]:
            del os.environ[key]
        for key, value in snapshot.items():
            if os.environ.get(key) != value:
                os.environ[key] = value
