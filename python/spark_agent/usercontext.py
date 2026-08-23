"""The user context: a child process that runs notebook code without the keys.

WHY A PROCESS AND NOT A NAMESPACE. Measured (`e2e/two-context/probe.py`): a cell
in a Viewer's session obtained the agent's storage bearer through
`__import__('storage').token()`, listed `ENTRA_CLIENT_SECRET` out of
`os.environ`, and read a narrowed table's files in full -- 3 rows of 3, both
columns, where the same principal's own identity is refused by OneLake with a
403. The gap is whose identity reads.

Nothing inside one interpreter closes that. User code arrives as text and is
`exec`'d, so `__import__`, `os.environ` and every module global are one
expression away; hiding a name only moves it. A credential the user context must
not have is a credential that must not be IN the user context, and the boundary
that enforces that is the process.

This mirrors what Fabric describes -- a user context that "never has direct,
unfiltered access to secured tables" and a privileged system context that reads
and filters on its behalf. Here the parent agent is the system context: it keeps
the service credential, resolves policy, and prepares what the child may see.
The child gets a token minted for the CALLER, so a path read arrives at OneLake
as that principal and the platform block applies.

THE PROTOCOL DOES NOT SHARE STDOUT. The child's stdout is the agent's log --
pyspark warnings, Delta chatter, anything a library decides to print -- so
responses travel on their own descriptor. Framing the protocol on a stream that
third-party code also writes to is a corruption waiting for the first library
that prints on import.

WHAT IS NOT SOLVED HERE. The child still needs to READ, and a narrowed table
refuses its token by design. Making the filtered rows available is the system
context's other half, and it is stage B2; until it lands, a secured table is
unreadable from the child rather than readable in full. That is the failing
direction, deliberately.
"""
import json
import os
import subprocess
import sys

# Names whose VALUE is a key. Scrubbed from the child's environment: the token
# is the thing being escalated, and the client secret is worse, because a secret
# mints fresh tokens for as long as it is valid.
CREDENTIAL_ENV = (
    "ENTRA_CLIENT_SECRET",
    "AZURE_STORAGE_TOKEN",
    "AZURE_CLIENT_SECRET",
    "FABRIC_TOKEN",
)

# Names that say WHERE to get a token rather than being one. A child stripped of
# these cannot mint, but it also cannot report a useful error, so they stay: the
# child holds the caller's token and nothing that widens it.
_KEEP = ("ENTRA_TOKEN_URL",)


def child_env(env=None):
    """The environment the child gets: the parent's, minus every credential.

    Explicitly a DENY list over a copy, not an allow list. An allow list looks
    safer and is worse here: the agent's runtime needs PATH, PYTHONPATH,
    SPARK_REMOTE, locale and proxy settings, and a child silently missing one of
    those fails in ways that read as an engine bug rather than a missing
    variable. The values that must not travel are enumerable; the values that
    must are not.
    """
    out = dict(os.environ if env is None else env)
    for name in CREDENTIAL_ENV:
        out.pop(name, None)
    return out


def frame(obj):
    """One protocol message: compact JSON, one line, newline-terminated."""
    return (json.dumps(obj) + "\n").encode()


def serve(requests, responses, run, globals_factory):
    """The child's loop: read a request, run it, write the envelope.

    Injected rather than imported so the loop is testable without Spark, a
    subprocess, or a real interpreter's globals -- the same reason
    `fetch_access` takes an opener.

    A request that is not JSON ends the loop rather than being guessed at. The
    only writer is the parent, so malformed input means the pipe is broken, and
    continuing would spin on a stream that will never parse.
    """
    g = globals_factory()
    for line in requests:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except ValueError:
            return
        if req.get("op") == "close":
            return
        envelope = run(req.get("code") or "", g)
        responses.write(frame(envelope))
        responses.flush()


class UserContext:
    """Parent-side handle on one child.

    One per Livy session, because the namespace a notebook accumulates is that
    session's. Sharing a child between sessions would reintroduce exactly the
    cross-session leak that per-session Spark sessions were built to close.
    """

    # The child is TOLD which descriptor to answer on rather than assuming one.
    # `pass_fds` keeps a descriptor's NUMBER across the fork, it does not
    # renumber it to 3, so a hardcoded 3 is whatever the parent happened to have
    # open there — "Bad file descriptor" if nothing, someone else's stream if
    # something. Renumbering with dup2 in a preexec_fn would work and runs
    # arbitrary code between fork and exec; naming the number does not.
    RESPONSE_FD_ENV = "SPARK_AGENT_RESPONSE_FD"

    def __init__(self, argv=None, env=None, popen=None):
        self.argv = argv or [sys.executable, "-m", "spark_agent.usercontext"]
        self.env = env
        self._popen = popen or subprocess.Popen
        self.proc = None
        self._responses = None

    def start(self):
        read_fd, write_fd = os.pipe()
        env = child_env(self.env)
        env[self.RESPONSE_FD_ENV] = str(write_fd)
        try:
            self.proc = self._popen(
                self.argv, env=env, stdin=subprocess.PIPE,
                pass_fds=(write_fd,),
            )
        finally:
            # The parent holds the READ end only. Leaving the write end open
            # here means the read never sees EOF when the child dies, and a
            # crashed child becomes a hang instead of an error.
            os.close(write_fd)
        self._responses = os.fdopen(read_fd, "rb")
        return self

    def run(self, code):
        """Execute one statement in the child, returning Livy's envelope.

        A child that dies mid-statement is reported as an error naming that,
        never as a hang and never as an empty success: EOF on the response pipe
        is unambiguous, and the alternative -- waiting on a process that has
        already gone -- is the failure mode this reports instead.
        """
        if self.proc is None or self._responses is None:
            raise RuntimeError("user context not started")
        try:
            self.proc.stdin.write(frame({"code": code}))
            self.proc.stdin.flush()
        except (BrokenPipeError, ValueError):
            return self._died()
        line = self._responses.readline()
        if not line:
            return self._died()
        return json.loads(line)

    def _died(self):
        code = self.proc.poll() if self.proc else None
        return {"status": "error", "ename": "UserContextLost",
                "evalue": f"the user context exited ({code})",
                "traceback": [f"the user context process exited with {code} "
                              "before answering; the statement did not run"]}

    def close(self):
        if self.proc is None:
            return
        try:
            self.proc.stdin.write(frame({"op": "close"}))
            self.proc.stdin.flush()
            self.proc.stdin.close()
        except (BrokenPipeError, ValueError, AttributeError):
            pass
        try:
            self.proc.wait(timeout=10)
        except Exception:  # noqa: BLE001 - a child that will not go is killed
            self.proc.kill()
        if self._responses is not None:
            self._responses.close()
        self.proc, self._responses = None, None


def response_fd(env=None):
    """The descriptor the parent told this child to answer on."""
    env = os.environ if env is None else env
    return int(env[UserContext.RESPONSE_FD_ENV])


def main():  # pragma: no cover - exercised as a subprocess, not in-process
    import agent

    with os.fdopen(response_fd(), "wb") as responses:
        serve(sys.stdin.buffer, responses, agent.run_code, dict)


if __name__ == "__main__":  # pragma: no cover
    main()
