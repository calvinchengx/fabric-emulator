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
from contextlib import redirect_stdout
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from pyspark.sql import SparkSession

_b = SparkSession.builder.appName("livy-agent")
spark = (_b.remote(os.environ["SPARK_REMOTE"]) if os.environ.get("SPARK_REMOTE") else _b).getOrCreate()
def apply_connect_confs():
    """Sail reports this limit as "3GB"; pyspark 4.2's createDataFrame does
    int() on it. Overriding with an integer restores local-relation support for
    unmodified user code.

    Re-applied per session, not just once at import: the conf lives on the
    *server*, so if sail restarts while this agent keeps running, the client
    reconnects to a fresh engine where the override is gone and every later
    createDataFrame fails with the '3GB' ValueError — while spark.range keeps
    working, which makes it look like user error rather than a lost setting.
    """
    if not os.environ.get("SPARK_REMOTE"):
        return
    try:
        spark.conf.set("spark.sql.session.localRelationSizeLimit", str(64 * 1024 * 1024))
    except Exception:  # noqa: BLE001 — engine not reachable yet; retried next session
        pass


apply_connect_confs()


def _install_delta_ops():
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

    delta_ops.install(spark, storage.options)


_install_delta_ops()


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
catalog_claims = catalog.Claims()  # only consulted when isolation is unavailable


class _NoSparkContext:
    """Guide-rail for the Sail engine: real Fabric notebooks see `sc`, but
    Spark Connect has no SparkContext/RDD API. Any use fails with a pointer
    instead of a bare NameError/AttributeError (docs/20-lakesail-engine.md)."""

    def __getattr__(self, name):
        raise NotImplementedError(
            f"sc.{name}: the RDD/SparkContext API is not available on the "
            "emulator's Sail (Spark Connect) engine — use the DataFrame/SQL "
            "API instead. See docs/20-lakesail-engine.md."
        )

    def __repr__(self):
        return "<sc unavailable: Spark Connect engine (Sail) — DataFrame/SQL only>"


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
        apply_connect_confs()  # survive an engine restart between sessions
        # A PRIVATE SparkSession per Livy session. The agent holds one engine
        # connection and serves concurrent requests, so a shared session makes
        # current-database and temp views process-wide: two notebooks bound to
        # different lakehouses used to fight over `setCurrentDatabase`, and the
        # loser read the other's tables under an unqualified name. `newSession`
        # keeps the engine and splits that state. Engines that lack it fall back
        # to the shared session, where catalog.py detects the collision instead.
        session_spark, isolated = catalog.isolate(spark)
        session_isolated[session] = isolated
        namespaces[session] = {"spark": session_spark}
        try:
            # JVM sessions only — Spark Connect (Sail) has no sparkContext.
            namespaces[session]["sc"] = session_spark.sparkContext
        except Exception:
            namespaces[session]["sc"] = _NoSparkContext()
        # Real Fabric notebooks get `notebookutils`/`mssparkutils` as globals and
        # as importable modules; mirror both so notebook code runs unchanged.
        nbu = _notebookutils()
        if nbu is not None:
            namespaces[session]["notebookutils"] = nbu
            mssparkutils = getattr(nbu, "mssparkutils", None)
            if mssparkutils is not None:
                namespaces[session]["mssparkutils"] = mssparkutils
    return namespaces[session]


def run_code(code, g):
    """Exec the block; if its last statement is an expression, eval that and
    return its repr as the REPL result (Livy semantics). Capture stdout too."""
    out = io.StringIO()
    try:
        tree = ast.parse(code, mode="exec")
    except SyntaxError:
        return {"status": "error", "ename": "SyntaxError",
                "evalue": "invalid syntax", "traceback": traceback.format_exc().splitlines()}
    last_expr = None
    if tree.body and isinstance(tree.body[-1], ast.Expr):
        last_expr = ast.Expression(tree.body.pop().value)
    try:
        with redirect_stdout(out):
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


def run_sql(code, g):
    """Execute one Spark SQL statement, returning a Livy SQL statement output.

    A statement whose plan has output columns (SELECT, SHOW, DESCRIBE, …)
    returns the SQL envelope with schema + rows, which is what a client like
    dbt-fabricspark reads a result set out of. DDL/DML (CREATE, INSERT, USE, …)
    has no output columns — an empty envelope, which dbt reads as an empty
    result set.
    """
    spark = g.get("spark")
    if spark is None:
        return {"status": "error", "ename": "NoSparkSession",
                "evalue": "no spark session in this REPL namespace", "traceback": []}
    try:
        df = spark.sql(code)
        if len(df.schema.fields) == 0:
            return {"status": "ok", "execution_count": 0, "data": {}}
        rows = [list(r) for r in df.collect()]
        return {"status": "ok", "execution_count": 0,
                "data": {"application/json": {"schema": df.schema.jsonValue(),
                                              "data": rows}}}
    except Exception:
        tb = traceback.format_exc().splitlines()
        # Sail quirk carried over from e2e/dbt-fabricspark/sql_agent.py: DML
        # executes fine but DataFusion reports its row count as uint64, which
        # the Arrow conversion rejects (Spark has no unsigned types). The write
        # has already landed, so treat that one conversion failure as an empty
        # result; every other error still surfaces.
        if "uint64" in tb[-1] or "Unsigned" in tb[-1]:
            return {"status": "ok", "execution_count": 0, "data": {}}
        return {"status": "error", "ename": "SqlError",
                "evalue": tb[-1] if tb else "error", "traceback": tb}


def register_tables(session, schema, tables, schemas=None):
    """Declare a lakehouse's Delta tables in this REPL's Spark catalog.

    The work is in catalog.py, which is importable without starting Spark and
    therefore testable; this resolves the namespace and hands over the session's
    isolation state so a degraded engine reports collisions instead of silently
    resolving an unqualified name against another lakehouse.
    """
    g = ns(session)
    return catalog.register(g.get("spark"), session, schema, tables,
                            schemas=schemas, claims=catalog_claims,
                            isolated=session_isolated.get(session, True))


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
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
            with cell_context(req.get("jobId"), req.get("cellIndex")):
                if (req.get("kind") or "").lower() == "sql":
                    self._send(200, run_sql(req.get("code", ""),
                                            ns(req.get("session", "default"))))
                else:
                    self._send(200, run_code(req.get("code", ""),
                                             ns(req.get("session", "default"))))
        elif self.path == "/register":
            self._send(200, register_tables(req.get("session", "default"),
                                            req.get("schema", ""),
                                            req.get("tables") or [],
                                            req.get("schemas") or []))
        elif self.path == "/mount":
            # Mirror the bound lakehouse's Files/ at /lakehouse/default/Files,
            # the mount a real Fabric runtime provides (files_mount.py).
            try:
                import files_mount
                self._send(200, files_mount.sync(req.get("workspace", ""),
                                                 req.get("lakehouse", "")))
            except Exception:  # noqa: BLE001 - a failed mount must not kill a session bind
                self._send(200, {"mounted": False,
                                 "error": traceback.format_exc().splitlines()[-1]})
        elif self.path == "/environment":
            self._send(200, apply_environment(req))
        elif self.path == "/close":
            namespaces.pop(req.get("session", ""), None)
            self._send(200, {"closed": True})
        else:
            self._send(404, {"error": "not found"})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8099
    print(f"livy-agent ready on :{port}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
