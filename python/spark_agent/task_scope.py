"""Per-session `sys.argv` and `os.environ` — the process boundary a task assumes.

The agent already gives every Livy session its own globals dict, so user
variables are isolated. MODULE state is not: `sys` and `os` are one object each
per interpreter, and the driver preamble writes a task's parameters and
environment straight into them (databricks-emulator `internal/server/jobs.go`,
`pythonPreamble`):

    sys.argv = json.loads("[\\"/task.py\\", \\"AAA\\"]")
    os.environ.update(json.loads("{...}"))

On real Databricks each task is its OWN PROCESS, so both are private to it.
Here one interpreter serves every task of every job, and `jobs.go` dispatches a
wave of independent tasks CONCURRENTLY. Two parameterised `spark_python_task`s
therefore overwrite each other and both read whichever assignment landed last.
Both still report SUCCESS, because nothing failed — the wrong parameters are
simply processed. `SparkEnvVars` carries RESOLVED SECRETS, so the environment
half of this is one task reading another task's secrets.

WHY NOT SAVE/RESTORE AROUND EACH STATEMENT. It is a stack discipline and these
do not stack. The agent is a `ThreadingHTTPServer`: statements genuinely
overlap, so B's assignment lands while A is still running, and A then restores
over B on the way out. It fixes the sequential case, which is not the broken
one. `storage.cell_context` is the worked example — it save/restores
FABRIC_JOB_ID for precisely this reason and is defeated by precisely this
concurrency, which is why it is routed through this module now.

WHY NOT REBIND LOCALLY in the preamble. User code legitimately runs its own
`import sys`, which re-fetches the one module object from `sys.modules` and
walks straight past any name bound in the session's globals.

WHAT THIS DOES instead is give both attributes a per-session VIEW, the shape
`notebookutils/runtime.py` already uses for the session's notebook identity:
truth lives per session, and a ContextVar makes the RUNNING statement's session
the one that resolves. Each `ThreadingHTTPServer` thread gets its own context,
so concurrent statements cannot see each other's binding.

The two are installed differently, and the asymmetry is the point. `sys.argv`
becomes a property on a ModuleType subclass. `os.environ` instead keeps its
IDENTITY and gains a session-aware class, because the stdlib and the wild both
do `from os import environ` at import time and that capture has to stay live —
swapping the `os` MODULE's class would leave every such capture pointing at the
unshimmed mapping, which is a split brain rather than a fix. Keeping the object
also means `os.getenv`, `os.path.expandvars` and `os.environ.copy()` are
session-aware for free: they all go through this one mapping.

Unbound — the agent's own startup, `/environment` installs, anything outside a
statement — both fall through to the real process values, so nothing that ran
before this module existed changes behaviour.

BOUNDARY, deliberately not papered over: a subprocess spawned by a task does
NOT inherit that task's session env. A scoped write is private to the session
and never reaches the C-level environ that `fork_exec` copies, where real
Databricks would inherit it. Passing `env=os.environ` explicitly hands over the
merged view and is exact. Nothing in-tree depends on the difference: the agent
spawns subprocesses only for package installs at startup, never from a
statement.
"""
import os
import sys
import types
from contextlib import contextmanager
from contextvars import ContextVar

# The agent runs on Python 3.8 in the JVM overlay image
# (test_spark_agent_runs_on_python38.py), so: no `X | None` annotations and no
# subscripted ContextVar.
_bound = ContextVar("spark_agent_task_scope", default=None)

# The buffer the RUNNING STATEMENT's prints go to.
#
# ITS OWN ContextVar, not a field on the session scope, and the difference is
# not cosmetic. A scope is per SESSION and lives as long as the session does; a
# capture buffer is per STATEMENT. Two statements of the same session can be in
# flight at once -- the agent is a ThreadingHTTPServer and nothing serialises
# them per session -- and hanging the buffer off the shared scope would put both
# of their output in one place, which is the bug being fixed with a smaller
# blast radius.
_capture = ContextVar("spark_agent_stdout", default=None)

# A scoped `del os.environ[k]` must hide a PROCESS variable from this session
# without unsetting it for the agent. Storing a tombstone keeps the delete
# private; actually deleting it would be the same class of bug this module
# exists to remove.
_DELETED = object()


class TaskScope:
    """One task's private view of argv and environment.

    Created per Livy session and kept for that session's lifetime, because a
    session is the agent's stand-in for the task PROCESS: a second statement in
    the same session must still see the argv the first one was given, the way a
    driver would.
    """

    __slots__ = ("argv", "env")

    def __init__(self):
        self.argv = None  # never assigned; reads fall through to the process
        self.env = {}  # overlay over the process environment


class _AgentSys(types.ModuleType):
    """`sys`, with an `argv` that resolves to the running statement's session."""

    @property
    def argv(self):
        scope = _bound.get()
        if scope is None:
            return sys.__dict__["argv"]
        if scope.argv is None:
            # Copy on first read: a task doing `sys.argv.append(...)` before it
            # ever assigns must not mutate the list every other session reads.
            scope.argv = list(sys.__dict__["argv"])
        return scope.argv

    @argv.setter
    def argv(self, value):
        scope = _bound.get()
        if scope is None:
            sys.__dict__["argv"] = value
            return
        if scope.argv is None:
            scope.argv = []
        # In place, so a caller holding a reference from earlier in the same
        # statement observes the assignment — as it would with the real
        # `sys.argv`, which is one list for the life of the process.
        scope.argv[:] = list(value)


# Named rather than `type(os.environ)`: both resolve to the same class on every
# supported interpreter, but a dynamic base is one a static checker cannot follow
# (ty: "only class objects are supported as class bases"), and this class is the
# whole mechanism -- it should be the checked part, not the excused part.
_EnvironBase = os._Environ  # noqa: SLF001


class _SessionEnviron(_EnvironBase):
    """`os.environ` resolving against the running statement's session.

    Subclasses the real mapping and is installed by swapping the INSTANCE's
    class, so `os.environ` remains the object every earlier `from os import
    environ` already captured.
    """

    def _overlay(self):
        scope = _bound.get()
        return None if scope is None else scope.env

    def __getitem__(self, key):
        overlay = self._overlay()
        if overlay is not None and key in overlay:
            value = overlay[key]
            if value is _DELETED:
                raise KeyError(key)
            return value
        return _EnvironBase.__getitem__(self, key)

    def __setitem__(self, key, value):
        overlay = self._overlay()
        if overlay is None:
            _EnvironBase.__setitem__(self, key, value)
            return
        # Raises the same TypeError the real mapping raises for a non-str, so a
        # scoped write is not quietly more permissive than an unscoped one.
        self.encodekey(key)
        self.encodevalue(value)
        overlay[key] = value

    def __delitem__(self, key):
        overlay = self._overlay()
        if overlay is None:
            _EnvironBase.__delitem__(self, key)
            return
        if key not in self:
            raise KeyError(key)
        overlay[key] = _DELETED

    def __iter__(self):
        overlay = self._overlay() or {}
        for key in overlay:
            if overlay[key] is not _DELETED:
                yield key
        for key in _EnvironBase.__iter__(self):
            if key not in overlay:
                yield key

    def __len__(self):
        return sum(1 for _ in self)


def install():
    """Make `sys.argv` and `os.environ` session-aware. Idempotent.

    Safe to call before anything binds a scope, and safe for code that never
    binds one: with no scope bound both resolve exactly as they did before.
    """
    if not isinstance(sys, _AgentSys):
        sys.__class__ = _AgentSys
    if not isinstance(os.environ, _SessionEnviron):
        os.environ.__class__ = _SessionEnviron


def bind(scope):
    """Resolve argv and environment against `scope` until unbind. Returns a token."""
    return _bound.set(scope)


def unbind(token):
    _bound.reset(token)


class _SessionStdout:
    """The single object in `sys.__dict__["stdout"]`, routed per statement.

    NOT A PROPERTY ON `_AgentSys`, which is what `argv` uses and what the issue
    proposed. Measured: it does not work.

        print("x")            -> the REAL stdout   (property bypassed)
        sys.stdout.write("x") -> the capture       (property honoured)

    `print` reaches stdout through `PySys_GetObject`, which reads the sys
    module's DICT directly; a property lives on the module's TYPE and is never
    consulted. `sys.argv` is safe because user code reads it as an attribute,
    and `print` is the overwhelmingly common way a statement produces output --
    so the property approach would have captured almost nothing while looking
    correct in a `sys.stdout.write` test.

    So the dict entry stays ONE stable object and the routing happens inside
    it. Nothing is saved or restored, which is the property this module exists
    to preserve: a concurrent statement resolves its own buffer through the
    ContextVar and neither can restore over the other.
    """

    def __init__(self, wrapped):
        # The stream to use when no statement is capturing: the agent's own
        # log. Held rather than looked up, because looking it up would find
        # this proxy.
        self._wrapped = wrapped

    def _target(self):
        captured = _capture.get()
        return self._wrapped if captured is None else captured

    def write(self, text):
        return self._target().write(text)

    def writelines(self, lines):
        return self._target().writelines(lines)

    def flush(self):
        return self._target().flush()

    def __getattr__(self, name):
        # `encoding`, `fileno`, `isatty`, everything else a caller may reach
        # for. Delegated to whichever stream is live, so a captured statement
        # sees the buffer's answers -- including `fileno()` raising, exactly as
        # it did under redirect_stdout.
        return getattr(self._target(), name)


def _ensure_proxy():
    """Put the proxy in the sys module dict, over whatever is there now.

    Idempotent, and re-applied on every capture rather than once at install:
    pytest and other harnesses assign `sys.stdout` after import, which replaces
    the dict entry outright. Re-checking here costs one isinstance and means a
    capture cannot silently stop working because something else swapped the
    stream first.
    """
    current = sys.__dict__.get("stdout")
    if not isinstance(current, _SessionStdout):
        sys.__dict__["stdout"] = _SessionStdout(current)


@contextmanager
def capturing(buffer):
    """Send this statement's prints to `buffer` for the duration.

    Replaces `redirect_stdout` at the one call site that captures a statement's
    output. Nothing is saved or restored: the buffer is bound in a ContextVar
    and read by the proxy sitting in `sys.__dict__["stdout"]`, so a concurrent
    statement in another thread resolves its own and neither can restore over
    the other.
    """
    _ensure_proxy()
    token = _capture.set(buffer)
    try:
        yield buffer
    finally:
        _capture.reset(token)


@contextmanager
def scoped(scope):
    token = bind(scope)
    try:
        yield scope
    finally:
        unbind(token)
