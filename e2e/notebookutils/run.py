#!/usr/bin/env python3
"""e2e R4: the notebook developer loop. Brings up the whole emulator family
(entra + fabric + azure-keyvault) and runs a real Fabric notebook that drives
the functional `notebookutils` shim — fs over OneLake, credentials tokens,
Key Vault secret brokering, and the lakehouse control plane — unchanged.

Run with `uv run --frozen --group test python e2e/notebookutils/run.py`; uv
installs the workspace shim from python/ before this orchestrator starts."""
import base64
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

# Pinned to the entra-emulator version in go.mod — bump this together with go.mod.
entra_bin = install("entra-emulator", "github.com/calvinchengx/entra-emulator/cmd/entra-emulator@v0.3.0")
# azure-keyvault-emulator is not in go.mod; bump this pin manually as needed.
kv_bin = install("azure-keyvault-emulator", "github.com/calvinchengx/azure-keyvault-emulator/cmd/azure-keyvault-emulator@v0.3.0")

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
    require_free_port(KV_PORT, "kv")
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
    # child-nb carries a definition, and it is markdown only. Both halves
    # matter. A Notebook with NO definition fails its RunNotebook job — an item
    # nobody gave content to has not "run with nothing to do", it was never
    # runnable — so `notebook.run` below would raise rather than return
    # Completed. Give it code instead and the job waits for a Spark engine this
    # e2e does not start, so `notebook.run` would poll to its timeout. Nothing
    # executable is the one shape that reaches Completed on its own.
    http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items", {
        "displayName": "child-nb", "type": "Notebook",
        "definition": {"parts": [{
            "path": "notebook-content.py",
            "payload": "IyBGYWJyaWMgbm90ZWJvb2sgc291cmNlCgojIE1BUktET1dOICoqKioqKioqKioqKioqKioqKioqCiMgTUFHSUMgIyMgbm90aGluZyB0byBleGVjdXRlCg==",
            "payloadType": "InlineBase64",
        }]},
    }, ft)

    # Two more markdown-only notebooks, so a runMultiple DAG has something to
    # order. Same reasoning as child-nb: nothing executable is the one shape
    # that reaches a terminal state without a Spark engine this e2e never
    # starts, which keeps the DAG's ORDERING and RESULT SHAPE under test
    # without dragging an engine into a control-plane suite.
    for name in ("dag-a", "dag-b"):
        http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items", {
            "displayName": name, "type": "Notebook",
            "definition": {"parts": [{
                "path": "notebook-content.py",
                "payload": "IyBGYWJyaWMgbm90ZWJvb2sgc291cmNlCgojIE1BUktET1dOICoqKioqKioqKioqKioqKioqKioqCiMgTUFHSUMgIyMgbm90aGluZyB0byBleGVjdXRlCg==",
                "payloadType": "InlineBase64",
            }]},
        }, ft)

    # A SECOND lakehouse, and a notebook bound to it. Fabric blocks a referenced
    # child whose default lakehouse differs from its parent's, and proving that
    # needs a child genuinely bound elsewhere — the mismatch is the test.
    other = http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses",
                 {"displayName": "other-lake"}, ft)
    mismatched = (
        "# Fabric notebook source\n"
        "# METADATA ********************\n"
        "# META {\n"
        '# META   "dependencies": {\n'
        '# META     "lakehouse": {"default_lakehouse":"' + other["id"] + '",'
        ' "default_lakehouse_workspace_id":"' + ws["id"] + '"}\n'
        "# META   }\n"
        "# META }\n"
        "# MARKDOWN ********************\n"
        "# MAGIC ## bound to the other lakehouse\n"
    )
    http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items", {
        "displayName": "other-lake-nb", "type": "Notebook",
        "definition": {"parts": [{
            "path": "notebook-content.py",
            "payload": base64.b64encode(mismatched.encode()).decode(),
            "payloadType": "InlineBase64",
        }]},
    }, ft)

    log("seeding a Key Vault secret")
    vt = token("https://vault.azure.net/.default")
    http("PUT", f"{KV}/secrets/db-password?api-version=7.4", {"value": "s3cr3t-value"}, vt)

    log("running the notebook")
    subprocess.run([sys.executable, "-u", os.path.join(DIR, "notebook.py")], check=True, env={
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
