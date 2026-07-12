#!/usr/bin/env python3
"""e2e: Microsoft's real fabric-cicd Python tool publishes into fabric-emulator,
authenticated by entra-emulator. Self-contained and OS-agnostic (Linux, macOS,
Windows): builds fabric-emulator from this repo, installs entra-emulator +
fabric-cicd if missing, runs the driver. This orchestrator is stdlib-only;
fabric-cicd and its dependencies live in the venv it creates."""

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
    subprocess.run(
        ["go", "install", "github.com/calvinchengx/entra-emulator/cmd/entra-emulator@latest"],
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

    log("installing fabric-cicd")
    venv = os.path.join(WORK, "venv")
    subprocess.run([sys.executable, "-m", "venv", venv], check=True)
    venv_py = os.path.join(venv, "Scripts" if os.name == "nt" else "bin", "python" + EXE)
    subprocess.run([venv_py, "-m", "pip", "install", "-q", "fabric-cicd"], check=True)

    log("running fabric-cicd against the emulator")
    subprocess.run([venv_py, "-u", os.path.join(DIR, "driver.py")], check=True, env={
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
