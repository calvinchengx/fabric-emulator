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


def _remember_location(name, location, schema=None):
    """Tell delta_ops where a table we just registered lives.

    Tolerant of delta_ops being absent: it is only imported on the Sail/Connect
    path, and on the JVM overlay Spark resolves names natively so there is
    nothing to record. A missing module here must not fail a registration that
    otherwise succeeded.
    """
    try:
        import delta_ops
    except ImportError:
        return
    delta_ops.remember(name, location, schema)


def register_tables(session, schema, tables):
    """Declare a lakehouse's Delta tables in this REPL's Spark catalog.

    On real Fabric a Lakehouse's `Tables/` already ARE catalog tables — attach a
    notebook and `SELECT * FROM silver_customers` resolves, because Fabric keeps
    a metastore in step with the folder. Nothing in this stack holds a
    metastore: Sail is handed object storage and nothing else. So the emulator
    enumerates the folder when a session opens and calls this, and a client that
    addresses a table by NAME rather than by abfs path works the way it does on
    Fabric.

    Idempotent (CREATE ... IF NOT EXISTS), because a session may be re-opened
    against a lakehouse whose tables are already registered.
    """
    g = ns(session)
    spark = g.get("spark")
    if spark is None:
        return {"registered": 0, "error": "no spark session in this REPL namespace"}
    registered, failed = [], []
    try:
        spark.sql(f"CREATE SCHEMA IF NOT EXISTS `{schema}`")
    except Exception:
        return {"registered": 0, "error": f"could not create schema {schema}: "
                                          f"{traceback.format_exc().splitlines()[-1]}"}
    for t in tables:
        name, loc = t.get("name"), t.get("location")
        if not name or not loc:
            continue
        try:
            spark.sql(f"CREATE TABLE IF NOT EXISTS `{schema}`.`{name}` "
                      f"USING delta LOCATION '{loc}'")
            registered.append(name)
            # Record where it lives so a statement naming this table can be
            # resolved without asking the engine. Sail cannot answer
            # DESCRIBE DETAIL at all, and we already know the answer.
            _remember_location(name, loc, schema)
        except Exception:
            # A folder under Tables/ that is not a readable Delta table is
            # skipped, not fatal — the same tolerance warehouse.Reflect applies.
            failed.append(f"{name}: {traceback.format_exc().splitlines()[-1]}")
    # Make UNQUALIFIED names resolve, the way they do in a Fabric notebook
    # attached to a lakehouse. Two routes, because engines differ:
    #
    #   1. move the session's current database — what Spark does for `USE`;
    #   2. failing that, register the same tables in `default` too.
    #
    # Sail rejects `USE <schema>` and setCurrentDatabase outright, so route 2 is
    # the one that actually fires there. It is a duplicate registration of the
    # same LOCATION, not a copy of data — both names point at one Delta table.
    unqualified = None
    try:
        spark.catalog.setCurrentDatabase(schema)
        unqualified = "current-database"
    except Exception:
        mirrored = []
        for name in registered:
            loc = next((t["location"] for t in tables if t.get("name") == name), None)
            if not loc:
                continue
            try:
                spark.sql(f"CREATE TABLE IF NOT EXISTS `default`.`{name}` "
                          f"USING delta LOCATION '{loc}'")
                mirrored.append(name)
                _remember_location(name, loc, "default")
            except Exception:
                failed.append(f"default.{name}: "
                              f"{traceback.format_exc().splitlines()[-1]}")
        unqualified = f"mirrored-into-default ({len(mirrored)})"

    out = {"registered": len(registered), "tables": registered,
           "unqualified": unqualified}
    if failed:
        out["skipped"] = failed
    return out


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
            if (req.get("kind") or "").lower() == "sql":
                self._send(200, run_sql(req.get("code", ""),
                                        ns(req.get("session", "default"))))
            else:
                self._send(200, run_code(req.get("code", ""),
                                         ns(req.get("session", "default"))))
        elif self.path == "/register":
            self._send(200, register_tables(req.get("session", "default"),
                                            req.get("schema", ""),
                                            req.get("tables") or []))
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
