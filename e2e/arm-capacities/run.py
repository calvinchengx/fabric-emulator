#!/usr/bin/env python3
"""Chain e2e: azure-mgmt-fabric creates a capacity on sibling arm-emulator;
fabric-emulator's opt-in ARM feed makes that capacity appear on GET /v1/capacities.

Requires a sibling arm-emulator checkout that serves Microsoft.Fabric/capacities
(the GHCR image does not, until that provider is released). Override the path
with ARM_EMULATOR_REPO.

A missing sibling is a FAILURE, not a skip.
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

WORK = os.path.join(tempfile.gettempdir(), "fabric-arm-capacities-e2e")
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18543")
ARM_PORT = os.environ.get("ARM_PORT", "18545")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19543")
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
SUB = "6082bfda-63d0-46f4-8272-ae9195139feb"
EXE = ".exe" if os.name == "nt" else ""


def log(msg):
    print(f"==> {msg}", flush=True)


def require_free_port(port, what):
    import socket
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.5)
        if s.connect_ex(("127.0.0.1", int(port))) == 0:
            raise SystemExit(
                f"port {port} is already in use, so this harness cannot start its own "
                f"{what}. Free it or override ENTRA_PORT / ARM_PORT / FABRIC_PORT.")


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


def sibling_arm_repo():
    env = os.environ.get("ARM_EMULATOR_REPO")
    if env:
        return env
    return os.path.abspath(os.path.join(REPO, "..", "arm-emulator"))


shutil.rmtree(WORK, ignore_errors=True)
os.makedirs(os.path.join(WORK, "fabric-data"))
os.makedirs(os.path.join(WORK, "armdata"))

arm_repo = sibling_arm_repo()
if not os.path.isfile(os.path.join(arm_repo, "go.mod")):
    raise SystemExit(
        f"arm-emulator checkout not found at {arm_repo}.\n"
        f"  This harness builds the sibling so it can exercise Microsoft.Fabric/capacities,\n"
        f"  which is not in a released arm-emulator image yet.\n"
        f"  Clone it next to this repo, or set ARM_EMULATOR_REPO.")

entra_bin = ensure_entra_emulator(WORK, log=log)

log("building fabric-emulator")
fabric_bin = os.path.join(WORK, "fabric-emulator" + EXE)
subprocess.run(["go", "build", "-C", REPO, "-o", fabric_bin, "./cmd/fabric-emulator"],
               check=True, env={**os.environ, "GOTOOLCHAIN": "auto"})

log(f"building arm-emulator from {arm_repo}")
arm_bin = os.path.join(WORK, "arm-emulator" + EXE)
subprocess.run(["go", "build", "-C", arm_repo, "-o", arm_bin, "./cmd/arm-emulator"],
               check=True, env={**os.environ, "GOTOOLCHAIN": "auto"})

procs = []
logfiles = {}


def start(name, cmd, env):
    path = os.path.join(WORK, name + ".log")
    with open(path, "wb") as f:
        procs.append(subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT, env=env))
    logfiles[name] = path


try:
    require_free_port(ENTRA_PORT, "entra")
    require_free_port(ARM_PORT, "arm")
    require_free_port(FABRIC_PORT, "fabric")

    log(f"starting entra-emulator on :{ENTRA_PORT}")
    start("entra", [entra_bin], {
        **os.environ, "ORIGIN_MODE": "compat", "PORT": ENTRA_PORT,
        "DB_PATH": os.path.join(WORK, "entra.sqlite"),
        "TLS_CERT_DIR": os.path.join(WORK, "entra-tls")})

    wait_healthy(f"https://localhost:{ENTRA_PORT}/health")

    issuer = f"https://localhost:{ENTRA_PORT}/{TENANT}/v2.0"
    log(f"starting arm-emulator on :{ARM_PORT}")
    start("arm", [
        arm_bin, "-addr", f":{ARM_PORT}", "-data-dir", os.path.join(WORK, "armdata"),
        "-entra-issuer", issuer, "-entra-tls-insecure",
        "-subscription-id", SUB, "-tenant-id", TENANT], os.environ.copy())

    wait_healthy(f"https://localhost:{ARM_PORT}/health")

    log(f"starting fabric-emulator on :{FABRIC_PORT} with FABRIC_ARM_URL")
    start("fabric", [
        fabric_bin, "-addr", f":{FABRIC_PORT}",
        "-data-dir", os.path.join(WORK, "fabric-data"),
        "-entra-issuer", issuer, "-entra-tls-insecure",
        "-arm-url", f"https://localhost:{ARM_PORT}",
        "-arm-poll-seconds", "1"], os.environ.copy())

    wait_healthy(f"https://localhost:{FABRIC_PORT}/health")

    pems = []
    for rel in ("entra-tls/cert.pem", "armdata/tls/cert.pem", "fabric-data/tls/cert.pem"):
        path = os.path.join(WORK, rel)
        if not os.path.isfile(path):
            raise RuntimeError(f"missing emulator cert {path}")
        with open(path) as f:
            pems.append(f.read())
    ca = os.path.join(WORK, "emulator-ca.pem")
    with open(ca, "w") as f:
        f.write("\n".join(pems))

    log("running azure-mgmt-fabric → GET /v1/capacities")
    driver = os.path.join(DIR, "driver.py")
    # Resolve wheels against the ambient trust store. The emulator CA bundle
    # trusts the siblings and nothing else, which is right for the client and
    # fatal for a PyPI fetch (arm-emulator's SDK harness does the same split).
    subprocess.run(["uv", "sync", "--script", driver], check=True)
    subprocess.run(
        ["uv", "run", "--offline", "--script", driver],
        check=True, env={
            **os.environ,
            "ARM_URL": f"https://localhost:{ARM_PORT}",
            "ENTRA_URL": f"https://localhost:{ENTRA_PORT}",
            "FABRIC_URL": f"https://localhost:{FABRIC_PORT}",
            "ARM_TENANT_ID": TENANT,
            "ARM_SUBSCRIPTION_ID": SUB,
            "ARM_CLIENT_ID": "00d88624-f0d7-46f6-a641-6232c2608928",
            "ARM_CLIENT_SECRET": "daemon-app-secret",
            "REQUESTS_CA_BUNDLE": ca,
            "SSL_CERT_FILE": ca,
        })
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
