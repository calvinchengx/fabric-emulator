#!/usr/bin/env python3
"""e2e: the official Python `mcp` SDK drives fabric-emulator's Fabric Data
Warehouse MCP server over Streamable HTTP, as signed-in USERS, against a real
SQL Server.

Fabric's server runs T-SQL "using the signed-in user's identity", so this suite
signs in entra-emulator's seeded users Alice and Bob with the password grant and
shows the answer depending on who asks: row-level security filters Bob's rows,
and his Viewer session is read-only. Go tests prove the branches
(internal/api/dwmcp_test.go, internal/server/sqlexec_test.go); this suite
proves an unmodified MCP host can reach both endpoints and read the CSV back.

Needs WAREHOUSE_MSSQL_DSN: a reachable SQL Server, ADO-style
(server=localhost,1433;user id=sa;password=...;encrypt=disable).
"""
import os
import shutil
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.request

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))

sys.path.insert(0, os.path.join(REPO, "e2e"))
from entra_install import ensure_entra_emulator  # noqa: E402
from port_guard import require_free_port  # noqa: E402

WORK = os.path.join(tempfile.gettempdir(), "mcp-datawarehouse-e2e")
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18555")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19555")
TDS_PORT = os.environ.get("TDS_PORT", "19556")
DSN = os.environ.get("WAREHOUSE_MSSQL_DSN", "")
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
EXE = ".exe" if os.name == "nt" else ""


def log(msg):
    print(f"==> {msg}", flush=True)


def wait_healthy(url, deadline=60):
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    end = time.time() + deadline
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, context=ctx, timeout=2) as r:
                if r.status == 200:
                    return
        except OSError:
            pass
        time.sleep(0.2)
    raise RuntimeError(f"health never came up at {url}")


if not DSN:
    raise SystemExit("set WAREHOUSE_MSSQL_DSN to a reachable SQL Server: this suite runs T-SQL on a real engine")

shutil.rmtree(WORK, ignore_errors=True)
os.makedirs(os.path.join(WORK, "data"))

entra_bin = ensure_entra_emulator(WORK, log=log)

log("building fabric-emulator")
fabric_bin = os.path.join(WORK, "fabric-emulator" + EXE)
subprocess.run(["go", "build", "-C", REPO, "-o", fabric_bin, "./cmd/fabric-emulator"], check=True)

procs, logfiles = [], {}


def start(name, cmd, env):
    path = os.path.join(WORK, name + ".log")
    with open(path, "wb") as f:
        procs.append(subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT, env=env))
    logfiles[name] = path


try:
    require_free_port(ENTRA_PORT, "entra")
    require_free_port(FABRIC_PORT, "fabric")
    require_free_port(TDS_PORT, "fabric TDS")
    log(f"starting entra-emulator on :{ENTRA_PORT}")
    start("entra", [entra_bin], {
        **os.environ, "ORIGIN_MODE": "compat", "PORT": ENTRA_PORT,
        "DB_PATH": os.path.join(WORK, "entra.sqlite"),
        "TLS_CERT_DIR": os.path.join(WORK, "entra-tls")})
    log(f"starting fabric-emulator on :{FABRIC_PORT}")
    start("fabric", [
        fabric_bin, "-addr", f"127.0.0.1:{FABRIC_PORT}",
        "-data-dir", os.path.join(WORK, "data"),
        "-disable-tls",
        "-entra-issuer", f"https://localhost:{ENTRA_PORT}/{TENANT}/v2.0",
        "-entra-tls-insecure"], {
            **os.environ,
            # The SQL endpoint and its engine: execute_query runs where a TDS
            # client's batch would, so both are configured.
            "FABRIC_SQL_TDS_ADDR": f"127.0.0.1:{TDS_PORT}",
            "FABRIC_WAREHOUSE_SQL_URL": DSN})
    wait_healthy(f"https://localhost:{ENTRA_PORT}/health")
    wait_healthy(f"http://127.0.0.1:{FABRIC_PORT}/health")

    log("running the official mcp SDK against /v1/mcp/dataPlane/.../sqlEndpoint")
    subprocess.run([sys.executable, "-u", os.path.join(DIR, "driver.py")], check=True, env={
        **os.environ,
        "ENTRA_BASE": f"https://localhost:{ENTRA_PORT}",
        "FABRIC_BASE": f"http://127.0.0.1:{FABRIC_PORT}",
        "TENANT": TENANT})
except Exception:
    for name, path in logfiles.items():
        sys.stderr.write(f"\n==== {name} log ====\n")
        with open(path, errors="replace") as f:
            sys.stderr.write(f.read())
    raise
finally:
    for p in procs:
        p.terminate()
    for p in procs:
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill()
