#!/usr/bin/env python3
"""Livy statement-executor agent: a persistent Spark REPL over HTTP.

The emulator's Go layer terminates the Livy REST protocol and drives this agent.
The agent holds one long-lived SparkSession and execs code snippets (statements)
in per-session namespaces, returning the REPL result of the last expression —
exactly the piece Apache Livy's Spark-side interpreter used to provide. This is
how a Livy session becomes *real* without the retired Apache Livy server: our Go
layer speaks the Livy REST contract, this agent is the interpreter behind it.

Stdlib-only HTTP + pyspark. Endpoints (private, emulator-internal):
  GET  /health                 -> {"state":"idle"} once Spark is up
  POST /statements {session,code} -> {"status":"ok","data":{"text/plain":...}}
  POST /close      {session}    -> drop a session's namespace
"""
import ast
import io
import json

# SPARK_REMOTE (e.g. sc://sail:50051) makes this a Spark Connect client —
# the Sail/no-JVM path (docs/20-lakesail-engine.md). Unset = classic JVM.
import os
import sys
import traceback
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import httpjson
import jvmconf  # no Spark; JVM session configs (see jvmconf.py)
import session_recovery
import statement_fields
import task_scope
import usercontext
from pyspark.sql import SparkSession

# Before any statement can bind a scope, and before this module's own
# `os.environ` reads below — with nothing bound both attributes resolve exactly
# as they did before (task_scope.py).
task_scope.install()


def build_spark():
    """The one place a shared SparkSession is created, so recovery can re-run it.

    Was inline at import until issue #312: when the engine dropped the session
    there was no second caller of getOrCreate(), so the agent stayed broken
    until the container restarted. Naming it changes nothing at boot.
    """
    b = SparkSession.builder.appName("livy-agent")
    if os.environ.get("SPARK_REMOTE"):
        return b.remote(os.environ["SPARK_REMOTE"]).getOrCreate()
    # Classic JVM: Delta + OneLake ABFS must be on the session that created
    # it. Without the extension, saveAsTable dies on
    # DELTA_CONFIGURE_SPARK_SESSION_WITH_EXTENSION_AND_CATALOG.
    return jvmconf.configure(b, os.environ).getOrCreate()


spark = build_spark()


def _normalise_connect_confs():
    """Ask the engine how it spells its byte-size confs, and act on the answer.

    Connect only. Not a constant preset: this image is consumed by more than one
    emulator, pointed at different Sail builds that disagree on the spelling, and
    a preset that is right for one caps user code on the other. See connectconf.
    """
    if not os.environ.get("SPARK_REMOTE"):
        return
    try:
        import connectconf
    except ImportError:  # pragma: no cover - runtime without the agent module
        return
    connectconf.apply(spark)


_normalise_connect_confs()


def _install_delta_ops(target=None):
    """Route OPTIMIZE/VACUUM to delta-rs when the engine cannot run them.

    Wrapping `spark.sql` rather than scanning statement text: user code reaches
    these through arbitrary Python (`spark.sql("OPTIMIZE ...")`), so the SQL
    entry point is the only place that catches every path. Anything unmatched
    is handed to Spark untouched.

    Only installed on the Sail/Connect path. On the JVM overlay Spark runs
    these natively and interception would be a downgrade — the JVM supports the
    full syntax (ZORDER, WHERE) that the delta-rs path refuses.

    Credentials come from `storage.options` — passed as the callable, so each
    statement resolves a current bearer rather than one frozen at startup.
    Without this the interception reaches OneLake unauthenticated and fails on
    every `abfss://` table, which is exactly what it is installed to handle.
    """
    if not os.environ.get("SPARK_REMOTE"):
        return
    try:
        import delta_ops
        import storage
    except ImportError:  # pragma: no cover - runtime without deltalake
        return

    delta_ops.install(target if target is not None else spark, storage.options)


_install_delta_ops()


def _install_eventstream_kafka():
    """Fabric Eventstream notebook API and OSS format("kafka") on both engines.

    JVM: rewrite eventstream.* to the OSS Kafka source; native kafka uses the
    jar. Sail: consume (emulator HTTP or kafka-python) and createDataFrame
    the Kafka-schema rows into Sail. foreachBatch runs here — Sail has no
    Kafka source and rejects the pickle. Never mapped onto `rate`.
    """
    try:
        import eventstream_kafka
    except ImportError:  # pragma: no cover - runtime without the agent module
        return
    try:
        if eventstream_kafka.install(spark):
            kind = "Connect" if os.environ.get("SPARK_REMOTE") else "JVM"
            print(f"agent: eventstream kafka adapter installed ({kind})")
    except Exception as exc:  # noqa: BLE001 — a broken wrap must not kill the agent
        print(f"agent: eventstream kafka adapter NOT installed: {exc}")


_install_eventstream_kafka()


def _install_input_file_name():
    """Reconstruct `input_file_name()` on engines that lack it (Sail does).

    Same placement rationale as delta_ops: the control plane relays statement
    text and never sees a Spark plan, so the only place the emulator can act on
    an *expression* is here, where the plan is built. `input_file.install` is a
    no-op when the engine implements the function itself, so the JVM overlay
    keeps Spark's native answer.
    """
    if not os.environ.get("SPARK_REMOTE"):
        return
    try:
        import input_file
    except ImportError:  # pragma: no cover - runtime without the agent module
        return
    try:
        if input_file.install(spark):
            print("agent: input_file_name shim installed (engine lacks it)")
    except Exception as exc:  # noqa: BLE001, a broken shim must not kill the agent
        print(f"agent: input_file_name shim NOT installed: {exc}")


_install_input_file_name()


def _install_packages(specs, source):
    """Install a list of wheels or package specs into the runtime.

    Generalised from a `/opt/wheels` glob so an ENVIRONMENT ITEM can drive it:
    on real Fabric an Environment's libraries are installed before any user code
    runs, and the emulator parsed that item for a long time without anything
    reading the answer (docs/37 §1). The bind-mount is now the fallback, not the
    mechanism.

    Loud on failure but not fatal: an agent that dies here helps nobody, and a
    missing dependency resurfaces in the first notebook with a clear
    ModuleNotFoundError naming the package.

    Returns (ok, detail) so a caller answering an HTTP request can report the
    outcome rather than guess at it.
    """
    import subprocess

    if not specs:
        return True, "nothing to install"
    cmd = [sys.executable, "-m", "pip", "install", "--no-warn-script-location", *specs]
    if os.path.exists("/bin/uv"):  # the runtime image manages its venv with uv
        cmd = ["/bin/uv", "pip", "install", "--python", sys.executable, *specs]
    r = subprocess.run(cmd, capture_output=True, text=True)
    shown = ", ".join(os.path.basename(s) for s in specs)
    if r.returncode == 0:
        print(f"agent: installed from {source}: {shown}")
        return True, f"installed {len(specs)} package(s)"
    detail = (r.stderr or r.stdout)[-800:]
    print(f"agent: WARNING could not install from {source} ({shown}):\n{detail}")
    return False, detail


def _install_custom_wheels():
    """The `/opt/wheels` fallback, for consumers who do not model an Environment.

    Kept because it needs no control plane: a compose overlay bind-mounts a
    directory and the agent installs it at startup. An Environment item, when
    one is bound, drives installs through /environment instead.
    """
    import glob as _glob

    wheels = sorted(_glob.glob("/opt/wheels/*.whl"))
    if wheels:
        _install_packages(wheels, "/opt/wheels")

_install_custom_wheels()

import catalog  # noqa: E402 — after the engine is up; see catalog.py for why it is split out
import notebook_display  # noqa: E402 — pure rendering, tested without an engine
import rddfacade  # noqa: E402 — same split: importable with no session, and unit-tested
import run_magic  # noqa: E402 — a pure source rewrite, tested without an engine
import sqlrun  # noqa: E402 — same split, same reason: importable without a session

# The Environment item this process has installed, if any: id -> the request that
# installed it. ONE per agent, deliberately.
#
# Fabric gives each session its own container, so two sessions binding different
# Environments is ordinary there. This emulator runs one long-lived process with
# many session namespaces (docs/37), so it CANNOT isolate them. Letting the last
# bind win would corrupt a dependency tree for a session that never asked, so a
# conflicting bind is REFUSED and says so. That is the honest answer available
# to a single process.
_environment_applied = {}


def apply_environment(req):
    """Install an Environment item's packages and apply its Spark config.

    Answers `{applied, reason}` rather than raising: a session bind must not die
    because a package failed to install, and the caller logs what happened.
    """
    env_id = (req.get("environment") or "").strip()
    packages = list(req.get("packages") or [])
    spark_config = dict(req.get("sparkConfig") or {})
    jars = list(req.get("jars") or [])
    session = req.get("session") or "?"

    if not env_id:
        return {"applied": False, "reason": "no environment named"}

    if _environment_applied and env_id not in _environment_applied:
        other = ", ".join(sorted(_environment_applied))
        return {"applied": False, "reason":
                f"this agent already has environment {other} installed and cannot "
                f"isolate a second one — Fabric gives each session its own "
                f"container and the emulator runs one process (docs/37). Bind the "
                f"same environment, or run the sessions against separate agents."}

    if env_id in _environment_applied:
        # Idempotent: a second session binding the SAME environment is fine and
        # must not pay for a reinstall.
        return {"applied": True, "reason": "already installed", "packages": packages}

    ok, detail = _install_packages(packages, f"environment {env_id}")

    applied_config = {}
    if spark_config:
        try:
            for key, value in spark_config.items():
                spark.conf.set(key, str(value))
                applied_config[key] = str(value)
        except Exception as err:  # noqa: BLE001 - config must not kill the bind
            print(f"agent: WARNING could not apply Spark config from environment "
                  f"{env_id}: {err}", file=sys.stderr, flush=True)

    if jars:
        # JARs need the classpath at JVM start, which a running Connect session
        # cannot change. Stated rather than silently ignored: a notebook that
        # needs one will otherwise fail far from here.
        print(f"agent: environment {env_id} declares {len(jars)} jar(s); the "
              f"classpath is fixed at engine start, so they are NOT applied "
              f"(docs/37)", file=sys.stderr, flush=True)

    _environment_applied[env_id] = {"session": session, "packages": packages}
    return {"applied": ok, "reason": detail, "packages": packages,
            "sparkConfig": applied_config, "jarsSkipped": len(jars)}


namespaces = {}  # Livy session id -> its persistent globals dict (a REPL)
session_isolated = {}  # Livy session id -> did it get a private SparkSession
# THE TWO-CONTEXT SPLIT, on by default.
#
# A statement against an item carrying OneLake security roles runs in a child
# process with a token minted for the CALLER, on an engine of that caller's own.
# That is what makes a path read arrive as the caller and be refused by OneLake
# (docs/54), and it is the shape Fabric has: it starts a Spark session per
# notebook and shares one only within a single-user boundary.
#
# IT ENGAGES ONLY WHERE THERE IS POLICY. `usercontext.is_secured` requires the
# statement to name a principal, a workspace AND an item, which the emulator
# sends only when the item HAS data access roles. A stack with no roles anywhere
# starts no engines and behaves exactly as before -- the cost lands on the
# workloads that asked for the enforcement.
#
# `FABRIC_TWO_CONTEXT=0` opts out. This is no longer the staging flag it started
# as -- that one guarded an incomplete feature and was meant to be deleted. This
# one is an escape hatch for a resource decision: enforcement now costs an
# engine per user (~66 MiB), and a consumer who cannot afford that should be
# able to say so and get the previous, weaker behaviour knowingly rather than by
# running an old image.
TWO_CONTEXT = os.environ.get("FABRIC_TWO_CONTEXT", "1") != "0"

session_route = {}     # Livy session id -> HOW (catalog.ROUTE_*), which says
                       # whether its CATALOG is private too — see
                       # onelake_security.apply(catalog_private=...)
# Per-session notebook identity (docs/38 §1). /mount and /statements remember
# it; each statement binds it into notebookutils.runtime so two notebooks in
# one agent cannot read each other's workspace.
session_context = {}

# Set in __main__ from the port actually bound. Module-scope default keeps
# `import agent` (tests, embedding) working without one.
AGENT_URL = os.environ.get("SPARK_AGENT_URL") or "http://127.0.0.1:8099"
# Per-session argv and environment (task_scope.py). A task's parameters and its
# resolved secrets arrive as writes to `sys` and `os`, one object each per
# interpreter: without a per-session view, two tasks dispatched in the same wave
# overwrite each other and both read the winner's, reporting SUCCESS either way.
session_scopes = {}
catalog_claims = catalog.Claims()  # only consulted when isolation is unavailable


def scope_for(session):
    """This session's argv/env view, made on first use and held until /close.

    Per SESSION rather than per statement, because a session stands in for the
    task process: a second statement must still see the argv the first was given.
    """
    scope = session_scopes.get(session)
    if scope is None:
        scope = session_scopes[session] = task_scope.TaskScope()
    return scope


def _notebookutils():
    """The `notebookutils` real Fabric notebooks import, or None.

    This image ships one under /app/python, but the agent runs as
    `python3 /app/python/spark_agent/agent.py`, so only the *script's* directory
    lands on sys.path and `import notebookutils` fails inside executed notebooks.
    Frameworks that resolve workspace/lakehouse context through it then fall
    through to environment-variable fallbacks, or raise. Putting the package
    directory on sys.path is what makes the emulator match Fabric here.
    """
    pkg_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    if pkg_root not in sys.path:
        sys.path.insert(0, pkg_root)
    try:
        import notebookutils

        return notebookutils
    except Exception:  # pragma: no cover - image built without the package
        return None


def ns(session):
    if session not in namespaces:
        _normalise_connect_confs()  # survive an engine restart between sessions
        # A PRIVATE SparkSession per Livy session. The agent holds one engine
        # connection and serves concurrent requests, so a shared session makes
        # current-database and temp views process-wide: two notebooks bound to
        # different lakehouses used to fight over `setCurrentDatabase`, and the
        # loser read the other's tables under an unqualified name. `newSession`
        # keeps the engine and splits that state. Engines that lack it fall back
        # to the shared session, where catalog.py detects the collision instead.
        session_spark, route = catalog.isolate(spark)
        isolated = bool(route)
        session_isolated[session] = isolated
        session_route[session] = route
        # A per-session engine session is a DIFFERENT SparkSession object, so
        # the OPTIMIZE/VACUUM interception installed on the root at import does
        # not reach it — `spark.sql` there is the unwrapped one and Sail answers with
        # "found OPTIMIZE at 0:8". Install per session, which the e2e caught the
        # moment isolation started actually working.
        if isolated:
            _install_delta_ops(session_spark)
        namespaces[session] = {"spark": session_spark}
        try:
            # A JVM session has the real thing; never shadow it.
            namespaces[session]["sc"] = session_spark.sparkContext
        except Exception:
            # Spark Connect (Sail) exposes no SparkContext on any engine. Bind
            # the measured-usage facade instead of refusing outright, and bind
            # it BOTH ways real Fabric offers it: the bare `sc` global and
            # `spark.sparkContext`, which is the spelling Microsoft's own
            # samples use. See rddfacade.py and docs/50-rdd-usage-capture.md.
            namespaces[session]["sc"] = rddfacade.attach(session_spark)
        # Real Fabric notebooks get `notebookutils`/`mssparkutils` as globals and
        # as importable modules; mirror both so notebook code runs unchanged.
        nbu = _notebookutils()
        if nbu is not None:
            namespaces[session]["notebookutils"] = nbu
            mssparkutils = getattr(nbu, "mssparkutils", None)
            if mssparkutils is not None:
                namespaces[session]["mssparkutils"] = mssparkutils
        # `%run` splices another notebook's code into THIS namespace, so the
        # helper it rewrites to has to close over this namespace specifically.
        namespaces[session][run_magic.HELPER] = _make_run_helper(
            namespaces[session])
        # `display` and `displayHTML` are notebook BUILTINS on Fabric — not
        # imports. A notebook writes `display(df)` with nothing above it, so
        # they have to be in the namespace or that line is a NameError. They
        # were absent entirely, which docs/56 carried as "unverified" until it
        # was measured.
        namespaces[session]["display"] = notebook_display.display
        namespaces[session]["displayHTML"] = notebook_display.displayHTML
    return namespaces[session]


def _make_run_helper(g):
    """Bind `%run`'s callable to this namespace. The SEMANTIC is in
    run_magic.make_runner, which is importable without an engine and therefore
    tested; what stays here is only the notebookutils lookup."""
    def get_definition(name):
        nbu = _notebookutils()
        if nbu is None:
            raise RuntimeError(
                "%run needs notebookutils to fetch the referenced notebook, "
                "and it is not importable in this session")
        return nbu.notebook.getDefinition(name)

    return run_magic.make_runner(g, get_definition)


def recover_lost_session():
    """Rebuild the shared session after the engine dropped it, and rebind.

    Returns the note to show the user, or None if the rebuild itself failed —
    in which case the original error stands, because reporting a recovery that
    did not happen is worse than the failure it replaces.
    """
    global spark
    try:
        spark = build_spark()
    except Exception:  # noqa: BLE001 — engine still down; the caller reports the real error
        print("agent: rebuilding the Spark session failed:\n" + traceback.format_exc(),
              file=sys.stderr, flush=True)
        return None
    _normalise_connect_confs()
    _install_delta_ops()
    def _record_route(sid, route):
        session_isolated[sid] = bool(route)
        session_route[sid] = route

    rebound = session_recovery.rebind(
        namespaces, spark, catalog.isolate,
        attach_sc=(None if os.environ.get("SPARK_REMOTE") is None else rddfacade.attach),
        on_route=_record_route)
    note = session_recovery.note(rebound)
    print(note, file=sys.stderr, flush=True)
    return note


def remember_context(session, req):
    """Merge identity fields from a /mount or /statements body into this session."""
    cur = session_context.setdefault(session, {})
    mapping = (
        ("currentWorkspaceId", "workspaceId"),
        ("defaultLakehouseId", "lakehouseId"),
        ("currentNotebookId", "notebookId"),
        ("currentJobId", "jobId"),
        # The ROOT of a reference run — the notebook a human started.
        # `notebookutils.nbResPath` resolves `builtin/` against it, never
        # against the running child, so a referenced notebook sees its
        # parent's resources rather than files that change with how it was
        # invoked.
        ("rootNotebookId", "rootNotebookId"),
        ("rootWorkspaceId", "rootWorkspaceId"),
    )
    for dest, src in mapping:
        if req.get(src):
            cur[dest] = req[src]
    # /mount uses the lakehouse-bind names, not the statement ones.
    if req.get("workspace"):
        cur["currentWorkspaceId"] = req["workspace"]
    if req.get("lakehouse"):
        cur["defaultLakehouseId"] = req["lakehouse"]
    if "isForPipeline" in req:
        cur["isForPipeline"] = bool(req["isForPipeline"])


@contextmanager
def runtime_scope(session, req):
    remember_context(session, req)
    nbu = ns(session).get("notebookutils")
    bind = getattr(getattr(nbu, "runtime", None), "bind", None)
    unbind = getattr(getattr(nbu, "runtime", None), "unbind", None)
    if not bind or not unbind:
        yield
        return
    token = bind(session_context.get(session) or {})
    # `notebookutils.session` needs THIS session's id and this agent's address.
    # Bound per statement rather than exported to os.environ, because the agent
    # is one process behind many concurrent sessions and an environment
    # variable would give every one of them the same id — `stop()` in one
    # notebook would then end another's session.
    sbind = getattr(getattr(nbu, "session", None), "bind", None)
    sunbind = getattr(getattr(nbu, "session", None), "unbind", None)
    stoken = sbind(AGENT_URL, session) if sbind and sunbind else None
    try:
        yield
    finally:
        unbind(token)
        if sunbind is not None and stoken is not None:
            sunbind(stoken)


def run_code(code, g):
    """Exec the block; if its last statement is an expression, eval that and
    return its repr as the REPL result (Livy semantics). Capture stdout too."""
    out = io.StringIO()
    # `%run` is a LINE magic inside an ordinary Python cell, so the cell parser
    # correctly leaves it alone — but it is a syntax error to Python, and has
    # to become a call before ast.parse sees it. See run_magic.py.
    code = run_magic.expand(code)
    try:
        tree = ast.parse(code, mode="exec")
    except SyntaxError:
        return {"status": "error", "ename": "SyntaxError",
                "evalue": "invalid syntax", "traceback": traceback.format_exc().splitlines()}
    last_expr = None
    if tree.body and isinstance(tree.body[-1], ast.Expr):
        last_expr = ast.Expression(tree.body.pop().value)
    try:
        # NOT redirect_stdout. That assigns `sys.stdout`, one attribute on one
        # module per interpreter, and this server runs statements concurrently:
        # measured (#346), one task's response carried another's output and two
        # came back empty. `task_scope.capturing` binds the buffer in a
        # ContextVar instead, so each statement resolves its own and neither
        # restores over the other.
        with task_scope.capturing(out):
            if tree.body:
                exec(compile(tree, "<statement>", "exec"), g)
            result = eval(compile(last_expr, "<statement>", "eval"), g) if last_expr is not None else None
        text = out.getvalue()
        if result is not None:
            text += repr(result)
        return {"status": "ok", "execution_count": 0, "data": {"text/plain": text}}
    except Exception as exc:
        # A graceful notebook exit surfaces here as an exception named
        # _NotebookExit (raised by the driver prelude's patched
        # notebookutils.notebook.exit). Stash its value in THIS session's
        # globals, the prelude cannot: each session's prelude re-patches the
        # one shared notebookutils module, so under concurrent notebook runs
        # the raising function belongs to whichever session ran its prelude
        # last, and its `global __nb_exit__` writes into that session's
        # namespace, not the caller's. Observed both ways: SUCCESS exits
        # recorded Failed, and the dual, a real failure inheriting another
        # run's exit value, would read as a false green. Matching by type
        # NAME, not identity, for the same reason: every session defines its
        # own _NotebookExit class.
        if type(exc).__name__ == "_NotebookExit":
            g["__nb_exit__"] = str(exc)
        tb = traceback.format_exc().splitlines()
        return {"status": "error", "ename": "Error", "evalue": tb[-1] if tb else "error", "traceback": tb}


# The last /register payload per session, kept so a catalog the ENGINE lost can
# be rebuilt without asking the control plane again. Sail's credential refresh
# restarts the engine (docker/sail/launcher.py) and the restart takes the
# session catalog with it, silently — see session_recovery.forgotten_table_in.
# This is the only state needed to put the registrations back.
session_registration = {}


def register_tables(session, schema, tables, schemas=None):
    """Declare a lakehouse's Delta tables in this REPL's Spark catalog.

    The work is in catalog.py, which is importable without starting Spark and
    therefore testable; this resolves the namespace and hands over the session's
    isolation state so a degraded engine reports collisions instead of silently
    resolving an unqualified name against another lakehouse.
    """
    g = ns(session)
    session_registration[session] = (schema, tables, schemas)
    return catalog.register(g.get("spark"), session, schema, tables,
                            schemas=schemas, claims=catalog_claims,
                            isolated=session_isolated.get(session, True))


def _table_should_exist(name):
    """Thin glue: hand session_recovery this agent's three registries.

    The DECISION is in session_recovery.table_exists_somewhere, which is
    importable without Spark and therefore actually tested. What stays here is
    only the wiring — which registries this process happens to have.
    """
    def recorded(bare):
        import delta_ops

        return delta_ops.known_location(bare)

    def declared():
        for _schema, tables, _schemas in session_registration.values():
            for t in tables or []:
                yield t.get("name", "")

    def in_storage(bare):
        import delta_ops
        import storage

        try:
            return delta_ops.derive_location(bare, storage.options)
        except delta_ops.DeltaOpError:
            # Ambiguity is not an answer to THIS question: a name in two
            # lakehouses is still one the engine should have resolved.
            return True

    return session_recovery.table_exists_somewhere(
        name, recorded_location=recorded, declared_tables=declared,
        in_storage=in_storage)


def restart_python(session):
    """Rebuild this session's namespace, keeping its engine handle.

    What a Python restart costs on Fabric is the interpreter's STATE: imported
    modules, user variables, anything held in memory. What it keeps is the
    Spark context. So the engine handle is carried across and everything else
    is dropped — and the caller is told what went, because a restart that
    quietly preserved half the namespace would be harder to reason about than
    one that cleared it all.
    """
    g = namespaces.get(session)
    if g is None:
        return {"restarted": False, "reason": f"no live session {session!r}"}
    keep = {k: g[k] for k in ("spark", "sc") if k in g}
    dropped = sorted(k for k in g
                     if not k.startswith("__") and k not in keep)
    g.clear()
    g.update(keep)
    # THE NAMESPACE IS NOT REBUILT BY ns(), deliberately. Popping it and
    # letting ns() run again would build a NEW isolated engine session — which
    # is precisely what this method must not do. So the globals a fresh
    # namespace gets are re-bound here around the engine handle that survived.
    nbu = _notebookutils()
    if nbu is not None:
        g["notebookutils"] = nbu
        mssparkutils = getattr(nbu, "mssparkutils", None)
        if mssparkutils is not None:
            g["mssparkutils"] = mssparkutils
    # THE NOTEBOOK BUILTINS COME BACK TOO. On Fabric `display`, `displayHTML`
    # and `%run` are provided by the kernel, not by the user's namespace, so a
    # Python restart cannot take them away. Rebinding only notebookutils left a
    # session where `display(df)` raised NameError and `%run` was a SyntaxError
    # for the rest of its life — a restart that costs the user variables is the
    # contract, one that costs the notebook its builtins is a defect.
    g[run_magic.HELPER] = _make_run_helper(g)
    g["display"] = notebook_display.display
    g["displayHTML"] = notebook_display.displayHTML
    return {"restarted": True, "kept": sorted(keep), "dropped": dropped}


def recover_forgotten_session(session):
    """Re-register this session's tables after the engine forgot them.

    Returns the number of tables put back, or None if there was nothing
    recorded to put back — in which case the caller still reports the cause,
    because "the engine restarted" is the useful half even when the agent has
    no registration to replay.
    """
    recorded = session_registration.get(session)
    if not recorded:
        return None
    schema, tables, schemas = recorded
    try:
        out = register_tables(session, schema, tables, schemas)
    except Exception:  # noqa: BLE001 — the note still names the cause
        print("agent: re-registering after an engine restart failed:\n"
              + traceback.format_exc(), file=sys.stderr, flush=True)
        return None
    return out.get("registered") if isinstance(out, dict) else None


def _apply_onelake_security(req, session):
    """Reshape the session for the statement's principal, if we know one.

    SILENT WHEN UNCONFIGURED, and deliberately: an agent driven by something
    other than this emulator's Livy layer gets no principal, and must keep
    working rather than refuse every statement. It is not a security hole — the
    unconfigured case is the one where nothing has been secured either.

    A FAILURE TO READ POLICY DOES NOT RUN THE CELL UNFILTERED. The exception
    propagates: better a statement that errors than one that quietly returns
    rows the caller may not have.
    """
    principal = req.get("principal")
    workspace = req.get("workspace")
    item = req.get("item")
    if not (principal and workspace and item):
        return
    # The emulator sends workspace+item only when the item HAS policy, so
    # arriving here means enforcement is required. An agent image without the
    # module cannot honour that, and running the cell anyway would serve
    # unfiltered rows to someone the policy narrows — so refuse instead.
    try:
        import onelake_security
    except ImportError as exc:  # pragma: no cover - older agent image
        raise RuntimeError(
            "this item has OneLake security roles, and this Spark agent image "
            "cannot apply them (onelake_security missing). Bump the agent "
            "digest, or remove the roles."
        ) from exc
    import storage

    base = (os.environ.get("AZURE_STORAGE_ENDPOINT") or "").rstrip("/")
    if base.endswith("/onelake"):
        base = base[: -len("/onelake")]
    tok = storage.token()
    if not base or not tok:
        return
    access = onelake_security.fetch_access(base, workspace, item, principal, tok)
    # THE SESSION'S SparkSession, never the process-wide one. `ns()` hands each
    # Livy session a private session exactly so temp views are not process-wide,
    # and a filter installed on the shared session is a filter applied to
    # everyone: the first run of this narrowed the OWNER's session to the
    # viewer's rows, which the e2e caught. Same shared-state trap as sys.argv
    # and stdout, one layer up.
    sess_spark = ns(session).get("spark")
    if sess_spark is None:
        return
    try:
        tables = [r[1] for r in sess_spark.sql("SHOW TABLES").collect()]
    except Exception:  # noqa: BLE001 - no catalog yet: nothing to reshape
        return
    # A session whose catalog is shared cannot be secured by editing it, and
    # apply() raises rather than half-doing it. The exception propagates for the
    # same reason a policy-read failure does: an errored statement beats one
    # that quietly returns rows the caller may not have.
    private = catalog.CATALOG_IS_PRIVATE.get(session_route.get(session), False)

    def _known_location(name):
        # delta_ops is imported lazily everywhere in this module (it needs the
        # engine up), so it is imported here rather than referenced as a global
        # that does not exist. A missing registry is "location unknown", which
        # restore() already handles.
        try:
            import delta_ops

            return delta_ops.known_location(name)
        except Exception:  # noqa: BLE001
            return None

    log = lambda m: print(m, flush=True)  # noqa: E731 - one call site, twice
    if TWO_CONTEXT and usercontext.is_secured(req):
        # TWO CONTEXTS. `sess_spark` is the SYSTEM context: privileged, holding
        # the real tables. It reads and filters here, and the user context is
        # told only where the results are. Nothing about the caller's session
        # is reshaped, because there is nothing unfiltered in it to reshape —
        # which is why this path needs no sweep, no restore and no refusal on a
        # shared catalog. Those exist to simulate, inside one namespace, the
        # separation this path actually has.
        permitted = onelake_security.prepare(
            sess_spark, access, tables, _known_location, log=log)
        answer = usercontext.for_session(session, principal).prepare(permitted)
        if answer.get("status") != "ok":
            raise RuntimeError("the user context could not be prepared: "
                               + str(answer.get("evalue")))
        return
    onelake_security.apply(sess_spark, access, tables,
                           log=log,
                           catalog_private=private,
                           location_of=_known_location)


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        code, body = httpjson.encode_response(code, obj)
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            self._send(200, {"state": "idle"})
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0) or 0)
        req = json.loads(self.rfile.read(n) or b"{}")
        if self.path == "/statements":
            statement_fields.check(req)
            # Livy statements carry a `kind`. A `sql` statement is Spark SQL and
            # must come back as a structured result set; anything else is Python
            # and comes back as REPL text. Before this dispatch existed the agent
            # was Python-only, which is why dbt-fabricspark needed a second,
            # SQL-only agent to talk to (e2e/dbt-fabricspark/sql_agent.py).
            # storage.py owns the export: it is the module whose token forge
            # consumes it, and it carries no pyspark import so it stays unit
            # testable. Absent (a runtime without the agent's modules) the
            # statement still runs, just unattributed.
            try:
                from storage import cell_context
            except ImportError:  # pragma: no cover - runtime without storage.py
                from contextlib import nullcontext as cell_context_stub

                def cell_context(*_a, **_k):
                    return cell_context_stub()
            # docs/37 §2a/2b: the Files mount is fresh at every statement, not
            # a bind-time snapshot. Refresh (flush then pull) before the cell
            # so it sees OneLake uploads; flush after so its writes land even
            # if this is the last statement. A mount failure must not fail the
            # statement — the notebook meets a missing file, not a 500.
            try:
                import files_mount
                files_mount.refresh()
            except Exception:  # noqa: BLE001
                print("files_mount: refresh before statement failed:\n"
                      + traceback.format_exc(), flush=True)
            try:
                session = req.get("session", "default")
                # OUTERMOST, so everything inside resolves against this task.
                # cell_context's FABRIC_JOB_ID export is an `os.environ` write
                # too: its save/restore kept one cell's identity off the NEXT
                # statement, but overlapping statements are what the agent
                # actually serves, and there restore-on-exit put the other
                # statement's value back under a still-running one.
                # The noqa stays: combining these needs a parenthesised
                # multi-context `with`, which is 3.9+ syntax, and this file runs
                # on 3.8 in the JVM overlay image
                # (test_spark_agent_runs_on_python38.py) — ruff is right about
                # the target it was told about, not the one that ships.
                #
                # OneLake security first: the SESSION is reshaped so a table
                # this caller may not read is not in it, and one they may read
                # in part is a filtered view. Per statement rather than at bind,
                # so revoking access reaches the next cell.
                _apply_onelake_security(req, session)
                with task_scope.scoped(scope_for(session)):  # noqa: SIM117
                    with cell_context(req.get("jobId"), req.get("cellIndex")):
                        with runtime_scope(session, req):
                            if TWO_CONTEXT and usercontext.is_secured(req):
                                # The user context runs it, with the caller's
                                # identity and none of the agent's credentials.
                                # The context travels WITH the statement: the
                                # parent's runtime_scope cannot reach into
                                # another process, so the child binds it there.
                                result = usercontext.for_session(
                                    session, req.get("principal")).run(
                                        req.get("code", ""), req.get("kind") or "",
                                        context=session_context.get(session) or {},
                                        identity={"agentUrl": AGENT_URL,
                                                  "session": session})
                            elif (req.get("kind") or "").lower() == "sql":
                                result = sqlrun.run_sql(req.get("code", ""), ns(session))
                            else:
                                result = run_code(req.get("code", ""), ns(session))
                        # The engine dropping our session is not this statement's
                        # fault and, until #312, was permanent: nothing re-ran
                        # getOrCreate(), so every later statement failed the same
                        # way. Rebuild now so the NEXT one works, and tell the
                        # user what the rebuild cost. This statement still fails
                        # — re-running arbitrary user code that may already have
                        # written data is not something the agent can do safely.
                        if session_recovery.envelope_is_lost_session(result):
                            session_recovery.annotate(result, recover_lost_session())
                        else:
                            # A DIFFERENT SHAPE OF THE SAME EVENT. A launcher
                            # restart never reports a lost session — sail
                            # re-creates the id and the client is just told its
                            # table is missing, which reads as a typo. Only a
                            # name THIS AGENT REGISTERED counts, so a real typo
                            # is left completely alone.
                            forgotten = session_recovery.forgotten_table_in(
                                result, _table_should_exist)
                            if forgotten:
                                session_recovery.annotate(
                                    result,
                                    session_recovery.forgotten_table_note(
                                        forgotten,
                                        recover_forgotten_session(session)))
            finally:
                try:
                    import files_mount
                    files_mount.flush()
                except Exception:  # noqa: BLE001
                    print("files_mount: flush after statement failed:\n"
                          + traceback.format_exc(), flush=True)
                # A restartPython() from INSIDE this statement can only happen
                # now: the child it restarts is the one that has been answering
                # this request. In `finally` so a statement that raised still
                # gets the restart it asked for.
                try:
                    if usercontext.apply_pending_restart(
                            req.get("session", "default")):
                        print("agent: user context restarted after the statement",
                              flush=True)
                except Exception:  # noqa: BLE001 - a failed restart is not this
                    print("agent: deferred user-context restart failed:\n"
                          + traceback.format_exc(), flush=True)
            self._send(200, result)
        elif self.path == "/register":
            self._send(200, register_tables(req.get("session", "default"),
                                            req.get("schema", ""),
                                            req.get("tables") or [],
                                            req.get("schemas") or []))
        elif self.path == "/mount":
            # Mirror the bound lakehouse's Files/ at /lakehouse/default/Files,
            # the mount a real Fabric runtime provides (files_mount.py).
            remember_context(req.get("session", "default"), req)
            try:
                import files_mount
                self._send(200, files_mount.sync(req.get("workspace", ""),
                                                 req.get("lakehouse", "")))
            except Exception:  # noqa: BLE001 - a failed mount must not kill a session bind
                self._send(200, {"mounted": False,
                                 "error": traceback.format_exc().splitlines()[-1]})
        elif self.path == "/environment":
            self._send(200, apply_environment(req))
        elif self.path == "/restart-python":
            # notebookutils.session.restartPython(). THE SPARK CONTEXT SURVIVES,
            # and that distinction is the whole method: a `pip install` in one
            # cell is not importable in the next until the interpreter restarts,
            # and tearing down the engine as well would cost every cached
            # DataFrame and temp view for no reason at all.
            #
            # A restart here is this SESSION'S namespace, not the process. The
            # agent is shared (docs/38 §5), so restarting the interpreter really
            # would end every other live notebook — the same reason /close drops
            # one namespace rather than exiting.
            sid = req.get("session", "default")
            # A SECURED session executes in a child process, so its interpreter
            # is that child and restarting `namespaces[sid]` here would clear a
            # namespace it never runs in — reporting a successful restart that
            # did nothing. The engine is deliberately not released: Spark
            # survives a Python restart, which is the whole method.
            if usercontext.request_restart(sid):
                self._send(200, {"restarted": True, "userContext": True,
                                 "when": "after this statement"})
            else:
                self._send(200, restart_python(sid))
        elif self.path == "/close":
            try:
                import files_mount
                files_mount.flush()
            except Exception:  # noqa: BLE001
                print("files_mount: flush on close failed:\n"
                      + traceback.format_exc(), flush=True)
            sid = req.get("session", "")
            gone = namespaces.pop(sid, None)
            session_context.pop(sid, None)
            session_scopes.pop(sid, None)
            # A Connect-isolated session is a session ON THE SERVER, not just a
            # dict here: dropping the reference leaves Sail holding it until its
            # timeout (an hour, by our compose). Release it, but only when this
            # session actually owned one — stopping the SHARED session would
            # take every other Livy session down with it.
            if gone is not None and session_isolated.get(sid):
                try:
                    gone["spark"].stop()
                except Exception:  # noqa: BLE001 - already gone, or no stop()
                    pass
            session_isolated.pop(sid, None)
            session_route.pop(sid, None)
            usercontext.close_session(sid)
            self._send(200, {"closed": True})
        else:
            self._send(404, {"error": "not found"})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8099
    # notebookutils.session asks the agent to end or restart THIS session, so
    # it needs the agent's address. Loopback, not the published host: the call
    # comes from inside this process and must not depend on how the outside
    # world reaches it.
    AGENT_URL = f"http://127.0.0.1:{port}"
    print(f"livy-agent ready on :{port}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
