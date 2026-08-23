"""The user context: a child process that runs notebook code without the keys.

WHY THESE CASES. `e2e/two-context/probe.py` measured what a cell can reach in
today's single-process agent: the storage bearer, `ENTRA_CLIENT_SECRET`, and a
narrowed table's files in full. The tests below are that measurement turned into
assertions plus the failure modes a process boundary introduces and a namespace
does not -- a child that dies, a child that never answers, a pipe shared with
whatever a library prints.

Real subprocesses where the behaviour IS the process (scrubbing, death), and
injected fakes where it is the protocol. A suite that spawned an interpreter for
every case would be slow and would still not prove the loop.
"""
import io
import json
import os
import subprocess
import sys
import textwrap
from pathlib import Path

AGENT = Path(__file__).resolve().parents[1] / "spark_agent"
sys.path.insert(0, str(AGENT))
import usercontext as uc  # noqa: E402, I001


# --- the environment the child gets -------------------------------------------

def test_the_client_secret_does_not_travel_to_the_child():
    # The sharpest of the measured leaks: a token expires, a secret mints new
    # ones for as long as it is valid.
    env = {"PATH": "/usr/bin", "ENTRA_CLIENT_SECRET": "daemon-app-secret",
           "SPARK_REMOTE": "sc://sail:50051"}
    got = uc.child_env(env)
    assert "ENTRA_CLIENT_SECRET" not in got
    assert got["SPARK_REMOTE"] == "sc://sail:50051", "the child lost its engine"
    assert got["PATH"] == "/usr/bin"


def test_every_named_credential_is_scrubbed():
    env = {name: "secret" for name in uc.CREDENTIAL_ENV}
    assert uc.child_env(env) == {}


def test_the_token_url_survives():
    # It says WHERE to get a token, not what one is. A child without it cannot
    # explain why it has no credential, which turns a policy refusal into a
    # mystery.
    assert uc.child_env({"ENTRA_TOKEN_URL": "http://entra/token"}) == {
        "ENTRA_TOKEN_URL": "http://entra/token"}


def test_scrubbing_does_not_mutate_the_parent_environment(monkeypatch):
    # It copies. Scrubbing in place would take the agent's own credential away
    # the first time it started a child — the system context disarming itself.
    monkeypatch.setenv("ENTRA_CLIENT_SECRET", "still-needed")
    uc.child_env()
    assert os.environ["ENTRA_CLIENT_SECRET"] == "still-needed"


# --- the protocol loop --------------------------------------------------------

def test_serve_runs_each_request_and_answers_once():
    seen = []

    def run(code, g, kind=""):
        seen.append((code, g, kind))
        return {"status": "ok", "data": {"text/plain": code.upper()}}

    out = io.BytesIO()
    uc.serve([uc.frame({"code": "a"}), uc.frame({"code": "b"})], out, run, dict)
    answers = [json.loads(line) for line in out.getvalue().splitlines()]
    assert [a["data"]["text/plain"] for a in answers] == ["A", "B"]
    # ONE namespace across statements: a notebook accumulates state, and a
    # fresh globals per cell would silently lose every variable.
    assert seen[0][1] is seen[1][1]


def test_serve_stops_on_close():
    out = io.BytesIO()
    uc.serve([uc.frame({"op": "close"}), uc.frame({"code": "never"})],
             out, lambda c, g, k="": {"status": "ok"}, dict)
    assert out.getvalue() == b""


def test_serve_stops_rather_than_spinning_on_a_broken_pipe():
    # The only writer is the parent, so unparseable input means the pipe is
    # broken. Skipping the line instead would spin on a stream that will never
    # parse — a busy loop in a child nobody is watching.
    out = io.BytesIO()
    uc.serve([b"not json\n", uc.frame({"code": "x"})],
             out, lambda c, g, k="": {"status": "ok"}, dict)
    assert out.getvalue() == b""


def test_blank_lines_are_not_requests():
    out = io.BytesIO()
    uc.serve([b"\n", uc.frame({"code": "x"})],
             out, lambda c, g, k="": {"status": "ok", "ran": c}, dict)
    assert len(out.getvalue().splitlines()) == 1


# --- a real child -------------------------------------------------------------

WORKER = textwrap.dedent("""
    import json, os, sys
    sys.path.insert(0, %r)
    import usercontext as uc

    def run(code, g, kind=""):
        return {"status": "ok", "data": {"text/plain": repr(eval(code, g))},
                "kind": kind}

    with os.fdopen(uc.response_fd(), "wb") as responses:
        uc.serve(sys.stdin.buffer, responses, run, dict)
""")


def spawn(extra=""):
    src = WORKER % str(AGENT) + textwrap.dedent(extra)
    return uc.UserContext(argv=[sys.executable, "-c", src]).start()


def test_a_real_child_answers_over_its_own_descriptor():
    ctx = spawn()
    try:
        assert ctx.run("1 + 1")["data"]["text/plain"] == "2"
    finally:
        ctx.close()


def test_a_child_that_prints_does_not_corrupt_the_protocol():
    # The child's stdout IS the agent's log. If responses shared it, the first
    # library that prints on import would break every statement after it — and
    # it would look like a parsing bug, not a framing one.
    ctx = spawn(extra="")
    try:
        noisy = "[__import__('sys').stdout.write('noise from a library'), 7][1]"
        assert ctx.run(noisy)["data"]["text/plain"] == "7"
        assert ctx.run("2 + 2")["data"]["text/plain"] == "4"
    finally:
        ctx.close()


def test_a_child_that_dies_is_an_error_not_a_hang():
    ctx = spawn()
    try:
        got = ctx.run("__import__('os')._exit(9)")
        assert got["status"] == "error"
        assert got["ename"] == "UserContextLost"
        assert "did not run" in " ".join(got["traceback"])
    finally:
        ctx.close()


def test_a_statement_after_the_child_died_is_also_an_error():
    # The second call takes the broken-pipe branch rather than the EOF one, and
    # both have to answer. A write to a dead child raises, and an unhandled
    # raise here would take down the agent's request thread.
    ctx = spawn()
    try:
        ctx.run("__import__('os')._exit(9)")
        assert ctx.run("1")["status"] == "error"
    finally:
        ctx.close()


def test_the_child_really_cannot_see_the_secret(monkeypatch):
    # End to end, with a real process and a real environment: the assertion the
    # e2e probe failed.
    monkeypatch.setenv("ENTRA_CLIENT_SECRET", "daemon-app-secret")
    ctx = spawn()
    try:
        got = ctx.run("__import__('os').environ.get('ENTRA_CLIENT_SECRET')")
        assert got["data"]["text/plain"] == "None", got
    finally:
        ctx.close()


def test_running_before_start_is_refused():
    ctx = uc.UserContext(argv=[sys.executable, "-c", "pass"])
    try:
        ctx.run("1")
    except RuntimeError as exc:
        assert "not started" in str(exc)
    else:
        raise AssertionError("a statement ran with no child")


def test_close_is_safe_to_call_twice():
    ctx = spawn()
    ctx.close()
    ctx.close()


def test_close_kills_a_child_that_will_not_leave():
    # A child ignoring `close` must not hold the agent's shutdown open.
    ctx = uc.UserContext(argv=[
        sys.executable, "-c",
        "import signal, time\n"
        "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
        "time.sleep(300)\n"])
    ctx.start()
    ctx.proc.wait = lambda timeout=None: (_ for _ in ()).throw(
        subprocess.TimeoutExpired("child", 10.0))
    ctx.close()
    assert ctx.proc is None


def test_the_childs_stdout_is_inherited_and_not_a_pipe():
    # Caught by mutation, not by design: capturing the child's stdout passed
    # every other test here, because responses travel on their own descriptor
    # either way. It is still a defect twice over — the agent loses the child's
    # log, and an undrained pipe blocks the child once ~64KB of library chatter
    # fills it, which presents as a statement that never returns.
    ctx = spawn()
    try:
        assert ctx.proc.stdout is None, "the child's stdout was captured, not inherited"
    finally:
        ctx.close()


# --- the caller's identity ----------------------------------------------------
#
# The point of the split. A child holding the SERVICE token is the escalation
# with extra steps; a child holding the CALLER's token is a read that OneLake
# can refuse, which is what stage A already does for that principal.

class _Resp:
    def __init__(self, payload):
        self._payload = json.dumps(payload).encode()

    def read(self):
        return self._payload

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


def test_the_caller_token_is_minted_for_the_named_principal():
    seen = {}

    def opener(req):
        seen["url"] = req.full_url
        seen["body"] = json.loads(req.data)
        return _Resp({"access_token": "for-the-viewer"})

    got = uc.caller_token("viewer-1",
                          env={"ENTRA_TOKEN_URL": "https://entra:8443/tid/oauth2/v2.0/token"},
                          opener=opener)
    assert got == "for-the-viewer"
    # The admin issuer on the SAME origin as the configured token URL — derived,
    # not a second setting that can drift out of step with it.
    assert seen["url"] == "https://entra:8443/admin/api/tokens"
    assert seen["body"]["extraClaims"] == {"oid": "viewer-1", "sub": "viewer-1"}
    assert seen["body"]["audience"] == "https://storage.azure.com"


def test_no_principal_means_no_token():
    assert uc.caller_token("", env={"ENTRA_TOKEN_URL": "https://entra/t"}) is None


def test_no_issuer_means_no_token():
    assert uc.caller_token("viewer-1", env={}) is None


def test_a_mint_that_fails_yields_no_token_rather_than_raising():
    # Fails CLOSED: a child with no storage token cannot read OneLake, which is
    # a refusal. Raising here would fail the statement with a mint error, and
    # falling back to the service token would be the leak.
    def opener(_req):
        raise OSError("issuer unreachable")

    assert uc.caller_token("viewer-1", env={"ENTRA_TOKEN_URL": "https://entra/t"},
                           opener=opener) is None


def test_a_grant_survives_the_scrub():
    # AZURE_STORAGE_TOKEN is scrubbed, and the caller's token is handed over
    # under that same name. Merging it before the scrub would delete it.
    ctx = uc.UserContext(argv=["true"],
                         env={"AZURE_STORAGE_TOKEN": "the-service-one"},
                         grants={"AZURE_STORAGE_TOKEN": "the-callers-one"},
                         popen=lambda *a, **kw: _FakeProc(kw))
    ctx.start()
    assert ctx.proc.kwargs["env"]["AZURE_STORAGE_TOKEN"] == "the-callers-one"
    assert uc.UserContext.RESPONSE_FD_ENV in ctx.proc.kwargs["env"]


class _FakeProc:
    def __init__(self, kwargs):
        self.kwargs = kwargs
        self.stdin = io.BytesIO()

    def poll(self):
        return None


# --- which sessions get a user context ----------------------------------------

def test_only_a_statement_naming_all_three_is_secured():
    # The emulator sends workspace+item alongside the principal only when the
    # item HAS policy. Any one missing means there is nothing to enforce, and
    # treating that as secured would put every session in a child.
    assert uc.is_secured({"principal": "v", "workspace": "w", "item": "i"})
    assert not uc.is_secured({"principal": "v", "workspace": "w"})
    assert not uc.is_secured({"workspace": "w", "item": "i"})
    assert not uc.is_secured({})


def test_one_child_per_session_reused_across_statements(monkeypatch):
    built = []
    monkeypatch.setattr(uc, "_contexts", {})
    def build(token):
        built.append(token)
        return object()

    a1 = uc.for_session("s1", "viewer", mint=lambda p: "tok-" + p, build=build)
    a2 = uc.for_session("s1", "viewer", mint=lambda p: "tok-" + p, build=build)
    b = uc.for_session("s2", "viewer", mint=lambda p: "tok-" + p, build=build)
    assert a1 is a2, "a second statement started a second child"
    assert b is not a1, "two sessions shared one namespace"
    assert built == ["tok-viewer", "tok-viewer"]


def test_a_session_with_no_mintable_identity_is_refused(monkeypatch):
    # NOT a fallback to in-process execution: that would run the cell with the
    # service credential, which is the whole thing being closed.
    monkeypatch.setattr(uc, "_contexts", {})
    try:
        uc.for_session("s1", "viewer", mint=lambda p: None, build=lambda t: object())
    except RuntimeError as exc:
        assert "viewer" in str(exc) and "OneLake security" in str(exc)
    else:
        raise AssertionError("a secured statement ran without a caller identity")


def test_closing_a_session_closes_its_child(monkeypatch):
    monkeypatch.setattr(uc, "_contexts", {})
    closed = []

    class Fake:
        def close(self):
            closed.append(True)

    uc.for_session("s1", "v", mint=lambda p: "t", build=lambda t: Fake())
    uc.close_session("s1")
    assert closed == [True]
    uc.close_session("s1")  # idempotent: /close can arrive twice
    assert closed == [True]
