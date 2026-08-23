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

    # The child is TOLD which descriptor to answer on rather than assuming one.
    # `pass_fds` keeps a descriptor's NUMBER across the fork, it does not
    # renumber it to 3, so a hardcoded 3 is whatever the parent happened to have
    # open there — "Bad file descriptor" if nothing, someone else's stream if
    # something. Renumbering with dup2 in a preexec_fn would work and runs
    # arbitrary code between fork and exec; naming the number does not.
    RESPONSE_FD_ENV = "SPARK_AGENT_RESPONSE_FD"

    def __init__(self, argv=None, env=None, popen=None, grants=None):
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
        self.proc = None
        self._responses = None

    def start(self):
        read_fd, write_fd = os.pipe()
        env = child_env(self.env)
        env.update(self.grants)
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
    wanted = {t["name"]: t for t in tables if t.get("name") and t.get("location")}
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
    ctx = (build or _build)(token)
    _contexts[session] = ctx
    return ctx


def _build(token):
    return UserContext(grants={"AZURE_STORAGE_TOKEN": token}).start()


def close_session(session):
    ctx = _contexts.pop(session, None)
    if ctx is not None:
        ctx.close()


def main():  # pragma: no cover - exercised as a subprocess, not in-process
    import agent

    with os.fdopen(response_fd(), "wb") as responses:
        serve(sys.stdin.buffer, responses, _dispatch, lambda: agent.ns("child"))


if __name__ == "__main__":  # pragma: no cover
    main()
