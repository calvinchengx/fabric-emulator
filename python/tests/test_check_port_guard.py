"""The port-guard contract, driven by pytest as well as by `make check`.

`scripts/check_port_guard.py` asserts the whole contract already. This exists
because a check that only ever runs from a Makefile earns no coverage and can
rot between the two gates — the repo's convention is a `check_*.py` in scripts/
with a `test_check_*.py` here.

The cases live in the script; these tests drive them, and add the property a
reader would otherwise take on trust: that the checker FAILS when the thing it
guards is broken.
"""
import pathlib
import socket
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_port_guard as c  # noqa: E402


def test_the_contract_holds():
    """Every case in the script, as `make check` runs it."""
    c.main()


def test_the_checker_fails_when_the_guard_stops_guarding(monkeypatch):
    """A checker that cannot fail proves nothing. Make the guard permissive and
    this must notice."""
    monkeypatch.setattr(c.pg, "require_free_port", lambda *a, **kw: None)
    with pytest.raises(c.PortGuardError):
        c.main()


def test_the_checker_fails_when_a_harness_grows_its_own_copy(monkeypatch, tmp_path):
    """The consolidation is only true while it stays true. A new harness with
    its own `require_free_port` must be caught, or the improved diagnosis is
    silently absent from it."""
    stray = tmp_path / "e2e" / "newsuite"
    stray.mkdir(parents=True)
    (stray / "run.py").write_text("def require_free_port(port, what):\n    pass\n")
    monkeypatch.setattr(c, "REPO", tmp_path)
    with pytest.raises(c.PortGuardError, match="define their own require_free_port"):
        c.main()


def test_a_busy_port_is_refused_before_anything_starts():
    """The property the whole guard exists for: a listener that is not ours must
    stop the run, not be health-checked and mistaken for it."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as srv:
        srv.bind(("127.0.0.1", 0))
        srv.listen(8)
        port = srv.getsockname()[1]
        with pytest.raises(SystemExit) as exc:
            c.pg.require_free_port(port, "entra")
    assert str(port) in str(exc.value)


def test_the_message_names_the_process_on_the_port():
    """The change that prompted this file. The leaks people actually hit are
    leftover emulators from earlier runs, on which `docker ps` prints nothing —
    so the reader concludes the message is wrong rather than the guess."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as srv:
        srv.bind(("127.0.0.1", 0))
        srv.listen(8)
        port = srv.getsockname()[1]
        holder = c.pg.describe_holder(port)
    if c.pg.shutil.which("lsof"):
        assert "pid" in holder
    else:
        assert "Nothing could be identified" in holder


def test_only_our_own_emulators_are_called_leftovers():
    """The tempting heuristic — PPID 1 means orphaned — is wrong on the exact
    case the guard was originally written for: on macOS the Docker port
    publisher is a launchd daemon and so is parented to init like a real
    orphan. Naming is the test instead."""
    assert c.pg._is_one_of_ours("entra-emulator")
    assert c.pg._is_one_of_ours("azure-keyvault-emulator")
    assert not c.pg._is_one_of_ours("OrbStack Helper")
    assert not c.pg._is_one_of_ours("com.docker.backend")


def test_the_diagnostic_never_raises_on_a_port_with_no_holder():
    """This runs on a harness's failure path. A diagnostic that can itself fail
    reports a second bug in place of the first."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        free = s.getsockname()[1]
    assert isinstance(c.pg.describe_holder(free), str)
