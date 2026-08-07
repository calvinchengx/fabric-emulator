#!/usr/bin/env python3
"""e2e: Microsoft's real fabric-cicd Python tool publishes into fabric-emulator,
authenticated by entra-emulator. Self-contained and OS-agnostic (Linux, macOS,
Windows): builds fabric-emulator from this repo, installs entra-emulator if
missing, and runs fabric-cicd from the locked uv dependency group."""

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
WORK = os.path.join(tempfile.gettempdir(), "fabric-cicd-e2e")
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18443")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19443")
TENANT = "11111111-1111-1111-1111-111111111111"
EXE = ".exe" if os.name == "nt" else ""


def log(msg):
    print(f"==> {msg}", flush=True)



def require_free_port(port, what):
    """Refuse to start when something else already owns `port`.

    Without this, a bind failure is INDISTINGUISHABLE FROM SUCCESS: the child
    process dies, wait_healthy then health-checks whatever else is listening,
    gets its 200, and the harness proceeds against a stranger's service. That is
    exactly what happened — an unrelated compose project published its own entra
    on 18443, so tokens were minted by that issuer and the emulator correctly
    refused them with a 401 at workspace create. The failure looked like a bug
    in the code under test and cost a day before someone checked `docker ps`.

    Checked before anything starts, so the message names the real problem rather
    than a symptom three steps downstream.
    """
    import socket
    # CONNECT, do not bind. A bind test is wrong twice over: SO_REUSEADDR lets a
    # 127.0.0.1 bind succeed on macOS while another socket holds 0.0.0.0 (which
    # is exactly how a docker -p publish looks), and without it the test races
    # its own TIME_WAIT. Asking "does anything answer here" has neither problem
    # and is the actual question.
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.5)
        if s.connect_ex(("127.0.0.1", int(port))) == 0:
            raise SystemExit(
                f"port {port} is already in use, so this harness cannot start its own "
                f"{what}.\n"
                f"  A health check would then pass against the OTHER service and every\n"
                f"  token would carry the wrong issuer.\n"
                f"  Free the port (`docker ps | grep {port}`) or override it:\n"
                f"    ENTRA_PORT=<free> FABRIC_PORT=<free> python3 <this harness>")

def wait_healthy(url, deadline=60):
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # self-signed harness certs
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


shutil.rmtree(WORK, ignore_errors=True)
os.makedirs(os.path.join(WORK, "data"))

# entra-emulator: PATH first, go install otherwise.
entra_bin = shutil.which("entra-emulator")
if not entra_bin:
    log("installing entra-emulator")
    # Pinned to the entra-emulator version in go.mod — bump this together with go.mod.
    subprocess.run(
        ["go", "install", "github.com/calvinchengx/entra-emulator/cmd/entra-emulator@v0.3.0"],
        check=True, env={**os.environ, "GOBIN": WORK})
    entra_bin = os.path.join(WORK, "entra-emulator" + EXE)

log("building fabric-emulator")
fabric_bin = os.path.join(WORK, "fabric-emulator" + EXE)
subprocess.run(["go", "build", "-C", REPO, "-o", fabric_bin, "./cmd/fabric-emulator"], check=True)

procs = []
logfiles = {}


def start(name, cmd, env):
    path = os.path.join(WORK, name + ".log")
    with open(path, "wb") as f:
        procs.append(subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT, env=env))
    logfiles[name] = path


try:
    require_free_port(ENTRA_PORT, "entra")
    require_free_port(FABRIC_PORT, "fabric")
    log(f"starting entra-emulator on :{ENTRA_PORT}")
    start("entra", [entra_bin], {
        **os.environ, "ORIGIN_MODE": "compat", "PORT": ENTRA_PORT,
        "DB_PATH": os.path.join(WORK, "entra.sqlite"),
        "TLS_CERT_DIR": os.path.join(WORK, "entra-tls")})

    log(f"starting fabric-emulator on :{FABRIC_PORT}")
    start("fabric", [
        fabric_bin, "-addr", f":{FABRIC_PORT}", "-data-dir", os.path.join(WORK, "data"),
        "-entra-issuer", f"https://localhost:{ENTRA_PORT}/{TENANT}/v2.0",
        "-entra-tls-insecure"], os.environ.copy())

    wait_healthy(f"https://localhost:{ENTRA_PORT}/health")
    wait_healthy(f"https://localhost:{FABRIC_PORT}/health")

    log("running fabric-cicd against the emulator")
    subprocess.run([sys.executable, "-u", os.path.join(DIR, "driver.py")], check=True, env={
        **os.environ,  # FABRIC_CICD_DEBUG passes through when set
        "ENTRA_PORT": ENTRA_PORT, "FABRIC_PORT": FABRIC_PORT,
        "REQUESTS_CA_BUNDLE": os.path.join(WORK, "data", "tls", "cert.pem"),
        "FABRIC_API_ROOT_URL": f"https://api.fabric.microsoft.com:{FABRIC_PORT}",
        "DEFAULT_API_ROOT_URL": f"https://api.fabric.microsoft.com:{FABRIC_PORT}",
        "FABRIC_CICD_RETRY_DELAY_OVERRIDE_SECONDS": "0"})
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
