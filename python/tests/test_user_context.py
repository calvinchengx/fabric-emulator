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

    def run(code, g):
        seen.append((code, g))
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
             out, lambda c, g: {"status": "ok"}, dict)
    assert out.getvalue() == b""


def test_serve_stops_rather_than_spinning_on_a_broken_pipe():
    # The only writer is the parent, so unparseable input means the pipe is
    # broken. Skipping the line instead would spin on a stream that will never
    # parse — a busy loop in a child nobody is watching.
    out = io.BytesIO()
    uc.serve([b"not json\n", uc.frame({"code": "x"})],
             out, lambda c, g: {"status": "ok"}, dict)
    assert out.getvalue() == b""


def test_blank_lines_are_not_requests():
    out = io.BytesIO()
    uc.serve([b"\n", uc.frame({"code": "x"})],
             out, lambda c, g: {"status": "ok", "ran": c}, dict)
    assert len(out.getvalue().splitlines()) == 1


# --- a real child -------------------------------------------------------------

WORKER = textwrap.dedent("""
    import json, os, sys
    sys.path.insert(0, %r)
    import usercontext as uc

    def run(code, g):
        return {"status": "ok", "data": {"text/plain": repr(eval(code, g))}}

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
