#!/usr/bin/env python3
"""e2e: the official Python `mcp` SDK drives fabric-emulator's Eventhouse MCP
server over Streamable HTTP, as signed-in USERS, against kustainer, Microsoft's
own KQL engine.

The remote Eventhouse server runs KQL against one KQL database "using the
signed-in user's identity"; this suite signs in entra-emulator's seeded users
Alice and Bob with the password grant, builds a table through the emulator's
Kusto endpoint, and drives all four tools against it. Go tests prove the
branches (internal/api/ehmcp_test.go); this suite proves an unmodified MCP host
gets a real engine's answers back through both endpoints.

kustainer is linux/amd64 only (its native layer needs AVX2, which Apple-silicon
emulation does not expose), so this runs in CI; RTI_FORCE=1 tries anyway.
"""
import os
import platform
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
from waiting import wait_for  # noqa: E402

WORK = os.path.join(tempfile.gettempdir(), "mcp-eventhouse-e2e")
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18557")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19557")
KUSTO_PORT = os.environ.get("KUSTO_PORT", "18080")
# The digest docker-compose.yml pins, so this suite and the rti profile run one engine.
KUSTAINER = ("mcr.microsoft.com/azuredataexplorer/kustainer-linux:latest"
             "@sha256:1a488e094be760ebff1204e690a54b1b372decd9730ea0baf2d291b4a488205f")
CONTAINER = "fabric-mcp-eventhouse-kustainer"
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


def kusto_ready():
    """The documented readiness probe: a management command needing no database."""
    probe = urllib.request.Request(f"http://127.0.0.1:{KUSTO_PORT}/v1/rest/mgmt", method="POST",
                                   data=b'{"csl":".show cluster"}', headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(probe, timeout=5) as r:
            return r.status == 200
    except OSError:
        return False


if platform.machine().lower() not in ("x86_64", "amd64") and not os.environ.get("RTI_FORCE"):
    raise SystemExit("kustainer needs linux/amd64 with AVX2; this host cannot run it (RTI_FORCE=1 to try anyway)")

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
    require_free_port(KUSTO_PORT, "kustainer")
    log(f"starting kustainer on :{KUSTO_PORT}")
    subprocess.run(["docker", "rm", "-f", CONTAINER], capture_output=True)
    subprocess.run(["docker", "run", "-d", "--name", CONTAINER, "--platform", "linux/amd64",
                    "-e", "ACCEPT_EULA=Y", "-m", "4g", "-p", f"127.0.0.1:{KUSTO_PORT}:8080", KUSTAINER], check=True)
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
        "-entra-tls-insecure"], {**os.environ, "FABRIC_KQL_URL": f"http://127.0.0.1:{KUSTO_PORT}"})
    wait_healthy(f"https://localhost:{ENTRA_PORT}/health")
    wait_healthy(f"http://127.0.0.1:{FABRIC_PORT}/health")
    log("waiting for kustainer")
    wait_for(600.0, kusto_ready, "kustainer never answered .show cluster")

    log("running the official mcp SDK against /v1/mcp/dataPlane/.../kqlEndpoint")
    subprocess.run([sys.executable, "-u", os.path.join(DIR, "driver.py")], check=True, env={
        **os.environ,
        "ENTRA_BASE": f"https://localhost:{ENTRA_PORT}",
        "FABRIC_BASE": f"http://127.0.0.1:{FABRIC_PORT}",
        "TENANT": TENANT})
except Exception:
    subprocess.run(["docker", "logs", "--tail", "40", CONTAINER])
    for name, path in logfiles.items():
        sys.stderr.write(f"\n==== {name} log ====\n")
        with open(path, errors="replace") as f:
            sys.stderr.write(f.read())
    raise
finally:
    subprocess.run(["docker", "rm", "-f", CONTAINER], capture_output=True)
    for p in procs:
        p.terminate()
    for p in procs:
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill()
