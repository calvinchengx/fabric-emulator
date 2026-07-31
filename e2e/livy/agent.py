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
import sys
import traceback
from contextlib import redirect_stdout
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from pyspark.sql import SparkSession

# SPARK_REMOTE (e.g. sc://sail:50051) makes this a Spark Connect client —
# the Sail/no-JVM path (docs/20-lakesail-engine.md). Unset = classic JVM.
import os
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
namespaces = {}  # Livy session id -> its persistent globals dict (a REPL)


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


def ns(session):
    if session not in namespaces:
        apply_connect_confs()  # survive an engine restart between sessions
        namespaces[session] = {"spark": spark}
        try:
            # JVM sessions only — Spark Connect (Sail) has no sparkContext.
            namespaces[session]["sc"] = spark.sparkContext
        except Exception:
            namespaces[session]["sc"] = _NoSparkContext()
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
    except Exception:
        tb = traceback.format_exc().splitlines()
        return {"status": "error", "ename": "Error", "evalue": tb[-1] if tb else "error", "traceback": tb}


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
            self._send(200, run_code(req.get("code", ""), ns(req.get("session", "default"))))
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
