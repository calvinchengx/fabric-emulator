"""An engine per user: what it is given, when it starts, and when it stops.

The registry reference-counts PROCESSES against sessions, which is the kind of
bookkeeping that fails quietly — an engine stopped under a live session, or one
leaked per identity for the life of the agent. Every external effect is
injected so those cases are provoked here rather than found in a stack.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))
import engine as en  # noqa: E402, I001


class FakeProc:
    def __init__(self, argv, env):
        self.argv, self.env = argv, env
        self.returncode = None
        self.terminated = False
        self.killed = False

    def poll(self):
        return self.returncode

    def terminate(self):
        self.terminated = True
        self.returncode = 0

    def wait(self, timeout=None):
        return self.returncode

    def kill(self):
        self.killed = True


def build(token="tok", ready_after=0, dies=False, env=None):
    """An Engine whose process, port, readiness and sleep are all fakes."""
    made = {}
    state = {"probes": 0}

    def popen(argv, env=None):
        made["proc"] = FakeProc(argv, env)
        if dies:
            made["proc"].returncode = 3
        return made["proc"]

    def probe(_host, _port):
        state["probes"] += 1
        return state["probes"] > ready_after

    e = en.Engine("viewer-1", token, env=env or {"PATH": "/bin"}, popen=popen,
                  port=lambda: 51999, probe=probe, sleep=lambda _s: None)
    return e, made, state


# --- what the engine process is given -----------------------------------------

def test_the_engine_holds_the_callers_token_and_no_way_to_mint():
    # An engine that could mint would not be scoped to this principal: it would
    # hold a service credential and be the shared engine again, with more steps.
    e, made, _ = build(token="the-callers", env={
        "PATH": "/bin", "ENTRA_CLIENT_SECRET": "daemon-app-secret",
        "ENTRA_TOKEN_URL": "http://entra/token", "ENTRA_CLIENT_ID": "app",
        "AZURE_STORAGE_ENDPOINT": "http://fabric/onelake"})
    e.start()
    env = made["proc"].env
    assert env["AZURE_STORAGE_TOKEN"] == "the-callers"
    assert "ENTRA_CLIENT_SECRET" not in env
    assert "ENTRA_TOKEN_URL" not in env
    assert "ENTRA_CLIENT_ID" not in env
    # ...and it keeps what it needs to reach OneLake at all.
    assert env["AZURE_STORAGE_ENDPOINT"] == "http://fabric/onelake"


def test_the_engine_binds_the_port_it_was_given():
    e, made, _ = build()
    e.start()
    assert made["proc"].argv == ["sail", "spark", "server", "--ip", "127.0.0.1",
                                 "--port", "51999"]
    assert e.remote == "sc://127.0.0.1:51999"


def test_there_is_no_remote_before_it_starts():
    e, _, _ = build()
    assert e.remote is None


# --- starting -----------------------------------------------------------------

def test_start_waits_until_the_port_accepts():
    e, _, state = build(ready_after=3)
    e.start()
    assert state["probes"] == 4, "it returned before the engine was listening"


def test_an_engine_that_dies_during_start_is_an_error_naming_the_exit():
    # Not a timeout: the process is gone, and waiting 30s to say so would hide
    # the exit code that explains why.
    e, _, _ = build(ready_after=99, dies=True)
    try:
        e.start()
    except RuntimeError as exc:
        assert "exited with 3" in str(exc) and "viewer-1" in str(exc)
    else:
        raise AssertionError("a dead engine was reported as started")


def test_an_engine_that_never_listens_times_out_and_is_cleaned_up():
    e, made, _ = build(ready_after=10_000)
    try:
        e.start(deadline=1.0)
    except RuntimeError as exc:
        assert "did not accept connections" in str(exc)
    else:
        raise AssertionError("a silent engine was reported as started")
    assert made["proc"].terminated, "the timed-out engine was left running"


def test_stop_is_idempotent():
    e, _, _ = build()
    e.start()
    e.stop()
    e.stop()


def test_an_engine_that_ignores_terminate_is_killed():
    e, made, _ = build()
    e.start()

    def stubborn(timeout=None):
        raise TimeoutError("still here")

    made["proc"].wait = stubborn
    e.stop()
    assert made["proc"].killed


# --- the registry: one engine per principal, many sessions --------------------

class Recorder:
    def __init__(self):
        self.built, self.stopped = [], []

    def build(self, principal, token):
        rec = self

        class E:
            def __init__(self):
                self.principal, self.token = principal, token
                self.sessions = set()

            def stop(self):
                rec.stopped.append(principal)

        rec.built.append(principal)
        return E()


def test_two_sessions_of_one_principal_share_an_engine():
    r = Recorder()
    reg = en.Registry(build=r.build)
    a = reg.acquire("viewer-1", "s1", "tok")
    b = reg.acquire("viewer-1", "s2", "tok")
    assert a is b, "a second session started a second engine for the same user"
    assert r.built == ["viewer-1"]


def test_two_principals_never_share_an_engine():
    # The single-user boundary is the whole point. Sharing here would put two
    # identities behind one credential, which is the arrangement being removed.
    r = Recorder()
    reg = en.Registry(build=r.build)
    assert reg.acquire("viewer-1", "s1", "t1") is not reg.acquire("viewer-2", "s2", "t2")
    assert r.built == ["viewer-1", "viewer-2"]


def test_the_engine_outlives_a_session_but_not_the_last_one():
    r = Recorder()
    reg = en.Registry(build=r.build)
    reg.acquire("viewer-1", "s1", "tok")
    reg.acquire("viewer-1", "s2", "tok")
    reg.release("s1")
    assert r.stopped == [], "an engine was stopped under a live session"
    reg.release("s2")
    assert r.stopped == ["viewer-1"], "the last session left an engine running"


def test_releasing_an_unknown_session_changes_nothing():
    r = Recorder()
    reg = en.Registry(build=r.build)
    reg.acquire("viewer-1", "s1", "tok")
    assert reg.release("never-existed") is False
    assert r.stopped == []


def test_a_principal_can_get_a_fresh_engine_after_the_last_one_stopped():
    r = Recorder()
    reg = en.Registry(build=r.build)
    reg.acquire("viewer-1", "s1", "tok")
    reg.release("s1")
    reg.acquire("viewer-1", "s2", "tok")
    assert r.built == ["viewer-1", "viewer-1"]


def test_stop_all_stops_every_engine():
    r = Recorder()
    reg = en.Registry(build=r.build)
    reg.acquire("viewer-1", "s1", "t")
    reg.acquire("viewer-2", "s2", "t")
    reg.stop_all()
    assert sorted(r.stopped) == ["viewer-1", "viewer-2"]
    assert reg.release("s1") is False


def test_a_free_port_is_actually_free():
    port = en.free_port()
    assert 1024 < port < 65536
    assert not en.accepts("127.0.0.1", port, timeout=0.2)
