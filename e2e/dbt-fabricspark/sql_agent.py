#!/usr/bin/env python3
"""SQL statement-executor agent for the dbt-fabricspark e2e.

dbt-fabricspark submits Spark **SQL** statements (``kind: sql``) over the
emulator's Livy layer and parses the Livy *SQL* result envelope —
``output.data["application/json"] = {"schema": {...}, "data": [[...]]}``. The
general-purpose Python REPL agent (``e2e/livy/agent.py``) execs Python and
returns ``text/plain``, which dbt cannot parse. This agent runs each statement
as ``spark.sql(code)`` and returns the SQL envelope, so real Spark computes
dbt's models. Delta extensions are enabled so dbt's default ``USING delta``
tables build.

The emulator drives the same private HTTP contract as the Python agent (it
ignores ``kind`` and forwards only ``code``):

  GET  /health                     -> {"state":"idle"} once Spark is up
  POST /statements {session,code}  -> a Livy statement *output* object
  POST /close      {session}       -> no-op (one shared SparkSession/catalog)

Stdlib HTTP + pyspark, mirroring e2e/livy/agent.py.
"""
import json
import sys
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from pyspark.sql import SparkSession

# Delta-enabled so dbt's `create table ... using delta` (the Fabric default)
# works. Local warehouse is fine for the protocol conformance milestone; binding
# the session to the lakehouse's OneLake path (ABFS) so Delta lands in OneLake
# is a follow-up (see README, milestone B).
spark = (
    SparkSession.builder.appName("dbt-fabricspark-agent")
    .config("spark.sql.extensions", "io.delta.sql.DeltaSparkSessionExtension")
    .config(
        "spark.sql.catalog.spark_catalog",
        "org.apache.spark.sql.delta.catalog.DeltaCatalog",
    )
    # Delta is the DEFAULT table format, mirroring Fabric's Spark. dbt-fabricspark
    # deliberately omits `using delta` from its DDL (it assumes the Fabric
    # default), so `create or replace table` must land as Delta for the atomic
    # REPLACE that seed --full-refresh and table materialization rely on.
    .config("spark.sql.sources.default", "delta")
    # ...and make `CREATE TABLE AS SELECT` (no USING clause) honour that default
    # instead of creating a Hive table — Spark's legacy default is Hive, which
    # this container has no support for. Fabric's Spark is Delta-by-default too.
    .config("spark.sql.legacy.createHiveTableByDefault", "false")
    .getOrCreate()
)


def run_sql(code):
    """Execute one Spark SQL statement, returning a Livy statement output.

    A statement whose plan has output columns (SELECT, SHOW, DESCRIBE, …) returns
    the SQL envelope with schema + rows. DDL/DML (CREATE, INSERT, USE, …) has no
    output columns — return an empty envelope, which dbt reads as an empty result
    set."""
    print(f"[sql-agent] SQL: {code}", flush=True)
    try:
        df = spark.sql(code)
        if len(df.schema.fields) == 0:
            return {"status": "ok", "execution_count": 0, "data": {}}
        rows = [list(r) for r in df.collect()]
        return {
            "status": "ok",
            "execution_count": 0,
            "data": {"application/json": {"schema": df.schema.jsonValue(), "data": rows}},
        }
    except Exception as e:
        tb = traceback.format_exc().splitlines()
        # The full Spark message (str(e)) is far more useful to dbt than the last
        # traceback frame; surface it as evalue and log the failing statement.
        msg = str(e).strip() or (tb[-1] if tb else "error")
        print(f"[sql-agent] ERROR on: {code}\n{msg}", flush=True)
        return {
            "status": "error",
            "execution_count": 0,
            "ename": type(e).__name__,
            "evalue": msg,
            "traceback": tb,
        }


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
            self._send(200, run_sql(req.get("code", "")))
        elif self.path == "/close":
            # One shared SparkSession + catalog; nothing per-session to drop.
            self._send(200, {"closed": True})
        else:
            self._send(404, {"error": "not found"})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8099
    print(f"dbt-fabricspark sql-agent ready on :{port}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
