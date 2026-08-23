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

THE PROTOCOL GETS A DESCRIPTOR OF ITS OWN. pyspark warnings, Delta chatter and
any library that writes on import would otherwise land in the middle of a
response, which is a corruption waiting for the first one that does. So stdout
stays the child's log, inherited by the agent, and responses travel on a private
pipe.

TWO WAYS TO HAND ONE OVER, because the platforms do not share a mechanism:

  * POSIX: `pass_fds`. fork copies the descriptor table and exec keeps whatever
    is not close-on-exec, so the child sees the same fd NUMBER. The parent says
    which number in the environment.
  * Windows: there is no descriptor table to inherit -- `CreateProcess` builds a
    fresh process, `STARTUPINFO` has slots for exactly stdin/stdout/stderr, and
    a descriptor is a C-runtime notion layered over a kernel HANDLE. So the
    handle is marked inheritable and named in
    `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` (`lpAttributeList={"handle_list": ...}`),
    the parent passes the HANDLE value, and the child turns it back into a
    descriptor with `msvcrt.open_osfhandle`. Python appends the std handles to
    that list itself when any stream is redirected, so a stdin pipe alongside an
    inherited stdout still works.

`subprocess` refuses `pass_fds` on Windows outright -- `assert not pass_fds,
"pass_fds not supported on Windows."` -- and it is an ASSERT, so under `python
-O` the flag is silently dropped rather than raising. Neither platform's path is
guessed at: the POSIX one is exercised by real subprocesses below, and the
Windows one is covered on POSIX through injected platform calls and then proved
by the Windows job.

WHAT IS NOT SOLVED HERE. The child still needs to READ, and a narrowed table
refuses its token by design. Making the filtered rows available is the system
context's other half, and it is stage B2; until it lands, a secured table is
unreadable from the child rather than readable in full. That is the failing
direction, deliberately.
"""
import json
import os
import ssl
import subprocess
import sys
import urllib.parse
import urllib.request

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


def caller_token(principal, env=None, opener=None):
    """A storage-audience token for the CALLER — the emulator's stand-in for OBO.

    Real Fabric's user context runs with the user's own identity; there the
    platform mints that. Here the agent asks entra-emulator to issue one for the
    principal the statement names, which is the same shape of claim and the same
    consequence: a read arrives at OneLake as the caller, so a grant that
    narrows them refuses it.

    Returns None rather than raising. A child with no storage token cannot read
    OneLake at all, which is the failing-closed direction; a child holding the
    SERVICE token would be the other one.
    """
    env = os.environ if env is None else env
    url = env.get("ENTRA_TOKEN_URL")
    if not url or not principal:
        return None
    parts = urllib.parse.urlsplit(url)
    admin = urllib.parse.urlunsplit(
        (parts.scheme, parts.netloc, "/admin/api/tokens", "", ""))
    body = json.dumps({
        "audience": env.get("ENTRA_STORAGE_AUDIENCE", "https://storage.azure.com"),
        "extraClaims": {"oid": principal, "sub": principal},
    }).encode()
    req = urllib.request.Request(
        admin, data=body, method="POST",
        headers={"Content-Type": "application/json"})
    # Same self-signed-TLS allowance as storage._mint, for the same reason: the
    # issuer is named by our own config, not by anything user-supplied.
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    do = opener or (lambda r: urllib.request.urlopen(r, timeout=30, context=ctx))
    try:
        with do(req) as response:
            payload = json.loads(response.read() or b"{}")
    except Exception:  # noqa: BLE001 - no token is a refusal, not a crash
        return None
    return payload.get("access_token") or payload.get("token")


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
        if req.get("op") == "prepare":
            envelope = register(g, req.get("tables") or [])
        else:
            envelope = run(req.get("code") or "", g, req.get("kind") or "")
        responses.write(frame(envelope))
        responses.flush()


class UserContext:
    """Parent-side handle on one child.

    One per Livy session, because the namespace a notebook accumulates is that
    session's. Sharing a child between sessions would reintroduce exactly the
    cross-session leak that per-session Spark sessions were built to close.
    """


    def __init__(self, argv=None, env=None, popen=None, grants=None,
                 windows=None, winapi=None):
        # BY PATH, not `-m`. The agent image puts these modules flat on
        # sys.path rather than in a `spark_agent` package, so `-m
        # spark_agent.usercontext` resolves on a developer checkout and fails in
        # the image with "No module named 'spark_agent'" — measured. Running the
        # file also puts its own directory on sys.path[0], which is what lets
        # the child `import agent` the same way the parent does.
        self.argv = argv or [sys.executable, os.path.abspath(__file__)]
        self.env = env
        # Applied AFTER scrubbing, so the child can be given the caller's own
        # token by the same name the scrub removed the service one under.
        # Merging before would scrub what we just granted, which reads as "the
        # child has no credential" and is indistinguishable from a mint that
        # failed.
        self.grants = dict(grants or {})
        self._popen = popen or subprocess.Popen
        # Both injectable so the branch POSIX cannot execute is still covered.
        self.windows = (os.name == "nt") if windows is None else windows
        self.winapi = winapi or WinApi()
        self.proc = None
        self._responses = None

    def start(self):
        env = child_env(self.env)
        env.update(self.grants)
        read_fd, write_fd = os.pipe()
        try:
            # stdout and stderr are INHERITED: they are the child's log, and
            # capturing either would risk an undrained pipe blocking the child
            # once ~64KB of library chatter fills it. Only stdin is a pipe, for
            # requests; responses come back on the private one below.
            if self.windows:
                handle = self.winapi.get_osfhandle(write_fd)
                self.winapi.set_handle_inheritable(handle, True)
                env[RESPONSE_HANDLE_ENV] = str(handle)
                self.proc = self._popen(
                    self.argv, env=env, stdin=subprocess.PIPE,
                    startupinfo=self.winapi.startupinfo([handle]))
            else:
                env[RESPONSE_FD_ENV] = str(write_fd)
                self.proc = self._popen(
                    self.argv, env=env, stdin=subprocess.PIPE,
                    pass_fds=(write_fd,))
        finally:
            # The parent keeps the READ end only. Leaving the write end open
            # here means the read never sees EOF when the child dies, and a
            # crashed child becomes a hang instead of an error. On Windows this
            # closes the HANDLE too -- after CreateProcess, so the child already
            # has its own copy.
            os.close(write_fd)
        self._responses = os.fdopen(read_fd, "rb")
        return self

    def prepare(self, tables):
        """Tell the child which tables it may name, and where each one is."""
        return self._exchange({"op": "prepare", "tables": tables})

    def run(self, code, kind=""):
        """Execute one statement in the child, returning Livy's envelope.

        A child that dies mid-statement is reported as an error naming that,
        never as a hang and never as an empty success: EOF on the response pipe
        is unambiguous, and the alternative -- waiting on a process that has
        already gone -- is the failure mode this reports instead.
        """
        return self._exchange({"code": code, "kind": kind})

    def _exchange(self, request):
        """One request, one answer, or a reported death. Never a hang."""
        if self.proc is None or self._responses is None:
            raise RuntimeError("user context not started")
        try:
            self.proc.stdin.write(frame(request))
            self.proc.stdin.flush()
        except (OSError, ValueError):
            # OSError, not BrokenPipeError. Writing to a dead child's pipe is
            # EPIPE on POSIX and EINVAL (`[Errno 22] Invalid argument`) on
            # Windows, which is an OSError but NOT a BrokenPipeError -- found by
            # the Windows job with the narrower except in place. ValueError
            # covers a stdin already closed by `close()`.
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
        except (OSError, ValueError, AttributeError):
            pass  # same portability reason as _exchange
        try:
            self.proc.wait(timeout=10)
        except Exception:  # noqa: BLE001 - a child that will not go is killed
            self.proc.kill()
        if self._responses is not None:
            self._responses.close()
        self.proc, self._responses = None, None


RESPONSE_FD_ENV = "SPARK_AGENT_RESPONSE_FD"          # POSIX: a descriptor number
RESPONSE_HANDLE_ENV = "SPARK_AGENT_RESPONSE_HANDLE"  # Windows: a kernel HANDLE


class WinApi:
    """The three Windows calls the private-handle route needs.

    A seam, not an abstraction. Injecting it is what lets POSIX CI cover the
    Windows branch: without it that code would first execute on a machine none
    of us can step through, which is how the last two portability defects
    reached CI instead of a test.
    """

    def get_osfhandle(self, fd):  # pragma: no cover - Windows only
        import msvcrt

        return msvcrt.get_osfhandle(fd)

    def set_handle_inheritable(self, handle, flag):  # pragma: no cover
        os.set_handle_inheritable(handle, flag)

    def startupinfo(self, handles):  # pragma: no cover
        return subprocess.STARTUPINFO(lpAttributeList={"handle_list": handles})


def protocol_stream(env=None, windows=None):
    """The child's end of the response pipe, as a binary file it owns.

    On Windows the descriptor is manufactured from the inherited handle, and
    O_BINARY is not decoration: the CRT would otherwise open it in text mode and
    rewrite every newline on the way out, and newlines are the frame delimiter.
    """
    env = os.environ if env is None else env
    windows = (os.name == "nt") if windows is None else windows
    if windows:  # pragma: no cover - the child half runs only on Windows
        import msvcrt

        handle = int(env[RESPONSE_HANDLE_ENV])
        fd = msvcrt.open_osfhandle(handle, os.O_WRONLY | os.O_BINARY)
    else:
        fd = int(env[RESPONSE_FD_ENV])
    return os.fdopen(fd, "wb")


def _dispatch(code, g, kind):  # pragma: no cover - runs in the child
    """SQL and Python are different entry points, and the child serves both.

    Routing on `kind` in the child rather than sending pre-dispatched code keeps
    one protocol: the parent forwards the request it received instead of knowing
    which of the agent's two runners applies.
    """
    import agent

    if (kind or "").lower() == "sql":
        import sqlrun

        return sqlrun.run_sql(code, g)
    return agent.run_code(code, g)


def _bind_arrow(spark, name, encoded):
    """Bind `name` to rows the system context sent, without touching storage."""
    import base64
    import io

    import pyarrow.ipc as ipc

    with ipc.open_stream(io.BytesIO(base64.b64decode(encoded))) as reader:
        table = reader.read_all()
    spark.createDataFrame(table.to_pandas()).createOrReplaceTempView(name)


def register(g, tables):
    """Make the permitted tables nameable in the child, and nothing else.

    The parent decided WHAT and WHERE (`onelake_security.prepare`); this only
    binds names. Keeping the decision on the privileged side is the point of the
    split -- a child that chose its own sources could choose the unfiltered one.

    REPLACE, not create-if-missing. A statement may follow a policy change, and
    a table that was narrowed further must not keep resolving to last
    statement's snapshot. Registrations the parent no longer lists are dropped
    for the same reason: a revoked table has to stop being nameable.
    """
    spark = g.get("spark")
    if spark is None:
        return {"status": "error", "ename": "NoSession",
                "evalue": "the user context has no Spark session"}
    wanted = {t["name"]: t for t in tables
              if t.get("name") and (t.get("location") or t.get("arrow"))}
    bound = g.setdefault("__onelake_bound__", set())
    errors = []
    for name in sorted(bound - set(wanted)):
        try:
            spark.sql(f"DROP VIEW IF EXISTS `{name}`")
        except Exception as exc:  # noqa: BLE001
            errors.append(f"{name}: {exc}")
    bound.clear()
    for name, t in wanted.items():
        try:
            if t.get("arrow"):
                # A relation the system context FILTERED and sent. It is bound
                # as a local relation, so nothing here reaches OneLake -- which
                # is the point: the caller's own identity is refused on this
                # table, and rightly.
                _bind_arrow(spark, name, t["arrow"])
            else:
                spark.sql(f"CREATE OR REPLACE TEMP VIEW `{name}` AS "
                          f"SELECT * FROM delta.`{t['location']}`")
            bound.add(name)
        except Exception as exc:  # noqa: BLE001
            # A name we cannot bind must not be left bound to something older.
            try:
                spark.sql(f"DROP VIEW IF EXISTS `{name}`")
            except Exception:  # noqa: BLE001
                pass
            errors.append(f"{name}: {exc}")
    if errors:
        return {"status": "error", "ename": "PrepareFailed",
                "evalue": "; ".join(errors)}
    return {"status": "ok", "bound": sorted(bound)}


# --- which sessions get one, and their lifetimes ------------------------------
#
# Here rather than in agent.py for the reason catalog.py and sqlrun.py are here:
# agent.py calls getOrCreate() at import, so nothing defined in it can be unit
# tested. A decision about WHO runs with WHICH identity is the last thing that
# should live where its tests cannot reach.

_contexts = {}  # Livy session id -> UserContext, for SECURED sessions only


def is_secured(req):
    """Does this statement's item carry OneLake security roles?

    The emulator sends `workspace` and `item` alongside the principal ONLY when
    the item has policy, so their presence IS the signal -- the same one
    `_apply_onelake_security` already acts on. Asking the store again would be a
    second answer to a question already answered, and two answers drift.
    """
    return bool(req.get("principal") and req.get("workspace") and req.get("item"))


_engines = None  # set lazily: engine.Registry, one Sail per principal


def engines():
    """The per-principal engine registry, created on first use.

    Lazy because importing it is cheap but CREATING one is a decision: an agent
    that never serves a secured statement should never own an engine registry,
    and tests that never touch this path should not have to reset one.
    """
    global _engines
    if _engines is None:
        import engine

        _engines = engine.Registry()
    return _engines


def for_session(session, principal, mint=None, build=None):
    """The child that runs this session's code, started on first use.

    Per Livy session, because the namespace a notebook accumulates is that
    session's. One child shared across sessions would reintroduce exactly the
    cross-session leak that per-session Spark sessions exist to close.
    """
    ctx = _contexts.get(session)
    if ctx is not None:
        return ctx
    token = (mint or caller_token)(principal)
    if not token:
        # No caller identity, no user context. Falling back to in-process
        # execution here would run the cell with the SERVICE credential, which
        # is the thing being closed, so this refuses instead -- the same
        # fail-closed direction as a policy read that errors.
        raise RuntimeError(
            "this item has OneLake security roles, so its statements run in a "
            "user context with the caller's own identity, and no token could "
            f"be minted for {principal!r}")
    ctx = (build or _build)(token, principal, session)
    _contexts[session] = ctx
    return ctx


def _build(token, principal, session):
    """A user context, pointed at an engine that belongs to this principal.

    SPARK_REMOTE is a GRANT rather than inherited, because the value being
    replaced is the shared engine every other session uses. A child that
    inherited it would run the caller's code on the engine holding the service
    credential, which is the arrangement the split exists to end -- and it would
    look like it was working.
    """
    eng = engines().acquire(principal, session, token)
    return UserContext(grants={"AZURE_STORAGE_TOKEN": token,
                               "SPARK_REMOTE": eng.remote}).start()


def close_session(session):
    ctx = _contexts.pop(session, None)
    if ctx is not None:
        ctx.close()
    # The child first, then its engine: releasing the engine while the child is
    # still connected would take the engine out from under a live statement.
    if _engines is not None:
        _engines.release(session)


def main():  # pragma: no cover - exercised as a subprocess, not in-process
    # Opened BEFORE importing agent, which brings up Spark: the descriptor must
    # be claimed while we still know it is ours, not after a library has had a
    # chance to touch the table.
    responses = protocol_stream()
    import agent

    with responses:
        serve(sys.stdin.buffer, responses, _dispatch, lambda: agent.ns("child"))


if __name__ == "__main__":  # pragma: no cover
    main()
