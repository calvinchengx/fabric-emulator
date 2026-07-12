#!/usr/bin/env python3
"""e2e R4: the notebook developer loop. Brings up the whole emulator family
(entra + fabric + azure-keyvault) and runs a real Fabric notebook that drives
the functional `notebookutils` shim — fs over OneLake, credentials tokens,
Key Vault secret brokering, and the lakehouse control plane — unchanged.

Self-contained, OS-agnostic: stdlib-only orchestrator; the shim installs from
python/ into a venv (it has no dependencies of its own)."""
import json
import os
import shutil
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))
WORK = os.path.join(tempfile.gettempdir(), "notebookutils-e2e")
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18443")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19080")
KV_PORT = os.environ.get("KV_PORT", "18444")
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
CLIENT_SECRET = "daemon-app-secret"
ENTRA = f"https://localhost:{ENTRA_PORT}"
FABRIC = f"http://127.0.0.1:{FABRIC_PORT}"
KV = f"https://127.0.0.1:{KV_PORT}"
EXE = ".exe" if os.name == "nt" else ""

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE


def log(msg):
    print(f"==> {msg}", flush=True)


def http(method, url, body=None, token=None, form=False):
    headers = {}
    data = None
    if body is not None:
        if form:
            data = urllib.parse.urlencode(body).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        else:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    with urllib.request.urlopen(req, context=_CTX) as r:
        raw = r.read()
        return json.loads(raw) if raw else {}


def wait_healthy(url, deadline=60):
    end = time.time() + deadline
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, context=_CTX, timeout=2) as r:
                if r.status == 200:
                    return
        except OSError:
            pass
        time.sleep(0.2)
    raise RuntimeError(f"health never came up at {url}")


def seed_app(app_uri):
    try:
        http("POST", f"{ENTRA}/admin/api/apps",
             {"displayName": app_uri, "appIdUri": app_uri, "isConfidential": False})
    except urllib.error.HTTPError as e:
        if e.code != 409:
            raise


def token(scope):
    return http("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token",
                {"grant_type": "client_credentials", "client_id": CLIENT_ID,
                 "client_secret": CLIENT_SECRET, "scope": scope}, form=True)["access_token"]


def install(exe_name, module):
    found = shutil.which(exe_name)
    if found:
        return found
    log(f"installing {exe_name}")
    subprocess.run(["go", "install", module], check=True, env={**os.environ, "GOBIN": WORK})
    return os.path.join(WORK, exe_name + EXE)


shutil.rmtree(WORK, ignore_errors=True)
os.makedirs(os.path.join(WORK, "data"))

entra_bin = install("entra-emulator", "github.com/calvinchengx/entra-emulator/cmd/entra-emulator@latest")
kv_bin = install("azure-keyvault-emulator", "github.com/calvinchengx/azure-keyvault-emulator/cmd/azure-keyvault-emulator@latest")

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
    issuer = f"https://localhost:{ENTRA_PORT}/{TENANT}/v2.0"
    log(f"starting entra-emulator on :{ENTRA_PORT}")
    start("entra", [entra_bin], {**os.environ, "ORIGIN_MODE": "compat", "PORT": ENTRA_PORT,
          "DB_PATH": os.path.join(WORK, "entra.sqlite"), "TLS_CERT_DIR": os.path.join(WORK, "entra-tls")})
    log(f"starting fabric-emulator on :{FABRIC_PORT}")
    start("fabric", [fabric_bin, "-addr", f"127.0.0.1:{FABRIC_PORT}", "-data-dir", os.path.join(WORK, "data"),
          "-disable-tls", "-entra-issuer", issuer, "-entra-tls-insecure"], os.environ.copy())
    log(f"starting azure-keyvault-emulator on :{KV_PORT}")
    start("keyvault", [kv_bin, "-addr", f"127.0.0.1:{KV_PORT}", "-entra-issuer", issuer,
          "-entra-tls-insecure", "-data-dir", os.path.join(WORK, "kv")],
          {**os.environ, "KV_TLS_CERT_DIR": os.path.join(WORK, "kv-tls")})
    wait_healthy(f"{ENTRA}/health")
    wait_healthy(f"{FABRIC}/health")
    wait_healthy(f"{KV}/health")

    log("seeding entra apps (storage + vault audiences)")
    seed_app("https://storage.azure.com")
    seed_app("https://vault.azure.net")

    log("creating workspace + lakehouse")
    ft = token("https://api.fabric.microsoft.com/.default")
    ws = http("POST", f"{FABRIC}/v1/workspaces", {"displayName": "notebook-ws"}, ft)
    lake = http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, ft)
    http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items", {"displayName": "child-nb", "type": "Notebook"}, ft)

    log("seeding a Key Vault secret")
    vt = token("https://vault.azure.net/.default")
    http("PUT", f"{KV}/secrets/db-password?api-version=7.4", {"value": "s3cr3t-value"}, vt)

    log("installing the notebookutils shim into a venv")
    venv = os.path.join(WORK, "venv")
    subprocess.run([sys.executable, "-m", "venv", venv], check=True)
    venv_py = os.path.join(venv, "Scripts" if os.name == "nt" else "bin", "python" + EXE)
    subprocess.run([venv_py, "-m", "pip", "install", "-q", os.path.join(REPO, "python")], check=True)

    log("running the notebook")
    subprocess.run([venv_py, "-u", os.path.join(DIR, "notebook.py")], check=True, env={
        **os.environ,
        "NOTEBOOKUTILS_FABRIC_URL": FABRIC,
        "NOTEBOOKUTILS_ENTRA_URL": ENTRA,
        "NOTEBOOKUTILS_TENANT": TENANT,
        "NOTEBOOKUTILS_CLIENT_ID": CLIENT_ID,
        "NOTEBOOKUTILS_CLIENT_SECRET": CLIENT_SECRET,
        "NOTEBOOKUTILS_WORKSPACE_ID": ws["id"],
        "NOTEBOOKUTILS_LAKEHOUSE_ID": lake["id"],
        "NOTEBOOKUTILS_VAULT_URL": KV,
        "NOTEBOOKUTILS_INSECURE": "1",
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
