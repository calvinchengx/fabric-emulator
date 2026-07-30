#!/usr/bin/env python3
"""e2e T0: the fabric_target toggle resolves the emulator profile and drives
the real control plane through it — target() -> credential -> session ->
workspace-by-name -> item -> LRO poll -> guards. Self-contained and
OS-agnostic like the other suites: builds fabric-emulator from this repo,
installs entra-emulator if missing, runs the driver in a venv with the
fabric-target package installed from source."""

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
WORK = os.path.join(tempfile.gettempdir(), "fabric-target-e2e")
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18445")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19445")
TENANT = "11111111-1111-1111-1111-111111111111"
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


shutil.rmtree(WORK, ignore_errors=True)
os.makedirs(os.path.join(WORK, "data"))

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
try:
    log(f"starting entra-emulator on :{ENTRA_PORT}")
    procs.append(subprocess.Popen(
        [entra_bin],
        env={**os.environ, "ORIGIN_MODE": "compat", "PORT": ENTRA_PORT,
             "DB_PATH": os.path.join(WORK, "entra.sqlite"),
             "TLS_CERT_DIR": os.path.join(WORK, "entra-tls")},
        stdout=open(os.path.join(WORK, "entra.log"), "w"), stderr=subprocess.STDOUT))
    wait_healthy(f"https://localhost:{ENTRA_PORT}/health")

    log(f"starting fabric-emulator on :{FABRIC_PORT}")
    procs.append(subprocess.Popen(
        [fabric_bin, "-addr", f"127.0.0.1:{FABRIC_PORT}",
         "-entra-issuer", f"https://localhost:{ENTRA_PORT}/{TENANT}/v2.0",
         "-entra-tls-insecure"],
        stdout=open(os.path.join(WORK, "fabric.log"), "w"), stderr=subprocess.STDOUT))
    wait_healthy(f"https://localhost:{FABRIC_PORT}/health")

    log("creating venv with fabric-target from source")
    venv = os.path.join(WORK, "venv")
    subprocess.run([sys.executable, "-m", "venv", venv], check=True)
    vpy = os.path.join(venv, "Scripts" if os.name == "nt" else "bin", "python" + EXE)
    subprocess.run([vpy, "-m", "pip", "install", "-q",
                    os.path.join(REPO, "python", "fabric-target"),
                    "requests", "pytest"], check=True)

    env = {**os.environ,
           "FABRIC_TARGET": "emulator",
           "FABRIC_EMULATOR_URL": f"https://localhost:{FABRIC_PORT}",
           "ENTRA_EMULATOR_URL": f"https://localhost:{ENTRA_PORT}"}

    log("running driver")
    subprocess.run([vpy, os.path.join(DIR, "driver.py")], check=True, env=env)

    # T1: the dual-target conformance suite, emulator leg. The same tests run
    # against real Fabric via .github/workflows/real-fabric.yml — only
    # FABRIC_TARGET and credentials differ. The driver above left the
    # "target-e2e" workspace in place; conformance scopes to it, as real
    # mode always must.
    log("running conformance suite (FABRIC_TARGET=emulator)")
    subprocess.run(
        [vpy, "-m", "pytest", "-m", "target", "-q",
         os.path.join(REPO, "python", "fabric-target", "conformance")],
        check=True, env={**env, "FABRIC_WORKSPACE": "target-e2e"})
    log("PASS")
finally:
    for p in procs:
        p.terminate()
    for p in procs:
        try:
            p.wait(timeout=10)
        except subprocess.TimeoutExpired:
            p.kill()
