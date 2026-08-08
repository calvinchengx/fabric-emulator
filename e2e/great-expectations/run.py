#!/usr/bin/env python3
"""e2e: real Great Expectations validates Fabric semantic-model data.

The tutorial's subject, adapted to the emulator: stands up entra + fabric,
publishes the golden model, mints a Power BI-audience token, then runs the GX
suites against the emulator's executeQueries endpoint — asserting
the same pass/fail pattern as the tutorial (Store/Measure pass, the YoY-ratio
DAX asset fails). See driver.py + README for the documented adaptations.

Run with `uv run --frozen --group great-expectations python
e2e/great-expectations/run.py`.
"""
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
FIX = os.path.join(REPO, "e2e", "semantic-model", "fixtures")
WORK = os.path.join(tempfile.gettempdir(), "great-expectations-e2e")
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18443")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19080")
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
ENTRA = f"https://localhost:{ENTRA_PORT}"
FABRIC = f"http://127.0.0.1:{FABRIC_PORT}"
PBI_AUDIENCE = "https://analysis.windows.net/powerbi/api"
EXE = ".exe" if os.name == "nt" else ""

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE


def log(m):
    print(f"==> {m}", flush=True)


def http(method, url, body=None, token=None, form=False):
    headers, data = {}, None
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
        return r.status, r.headers, (json.loads(raw) if raw else {})



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


shutil.rmtree(WORK, ignore_errors=True)
os.makedirs(os.path.join(WORK, "data"))

entra_bin = shutil.which("entra-emulator")
if not entra_bin:
    log("installing entra-emulator")
    # Pinned to the entra-emulator version in go.mod — bump this together with go.mod.
    subprocess.run(["go", "install", "github.com/calvinchengx/entra-emulator/cmd/entra-emulator@v0.3.0"],
                   check=True, env={**os.environ, "GOBIN": WORK})
    entra_bin = os.path.join(WORK, "entra-emulator" + EXE)

log("building fabric-emulator")
fabric_bin = os.path.join(WORK, "fabric-emulator" + EXE)
subprocess.run(["go", "build", "-C", REPO, "-o", fabric_bin, "./cmd/fabric-emulator"], check=True)

procs, logs = [], {}


def start(name, cmd, env):
    path = os.path.join(WORK, name + ".log")
    with open(path, "wb") as f:
        procs.append(subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT, env=env))
    logs[name] = path


try:
    require_free_port(ENTRA_PORT, "entra")
    require_free_port(FABRIC_PORT, "fabric")
    log(f"starting entra on :{ENTRA_PORT}, fabric on :{FABRIC_PORT}")
    start("entra", [entra_bin], {**os.environ, "ORIGIN_MODE": "compat", "PORT": ENTRA_PORT,
          "DB_PATH": os.path.join(WORK, "entra.sqlite"), "TLS_CERT_DIR": os.path.join(WORK, "entra-tls")})
    start("fabric", [fabric_bin, "-addr", f"127.0.0.1:{FABRIC_PORT}", "-data-dir", os.path.join(WORK, "data"),
          "-disable-tls", "-entra-issuer", f"https://localhost:{ENTRA_PORT}/{TENANT}/v2.0", "-entra-tls-insecure"],
          os.environ.copy())
    wait_healthy(f"{ENTRA}/health")
    wait_healthy(f"{FABRIC}/health")

    try:
        http("POST", f"{ENTRA}/admin/api/apps",
             {"displayName": "Power BI Service", "appIdUri": PBI_AUDIENCE, "isConfidential": False})
    except urllib.error.HTTPError as e:
        if e.code != 409:
            raise

    def token(scope):
        return http("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token",
                    {"grant_type": "client_credentials", "client_id": CLIENT_ID,
                     "client_secret": CLIENT_SECRET, "scope": scope}, form=True)[2]["access_token"]

    ft = token("https://api.fabric.microsoft.com/.default")
    ws = http("POST", f"{FABRIC}/v1/workspaces", {"displayName": "retail-ws"}, token=ft)[2]["id"]

    def part(path, fname):
        return {"path": path, "payloadType": "InlineBase64",
                "payload": base64.b64encode(open(os.path.join(FIX, fname), "rb").read()).decode()}
    _, hdrs, _ = http("POST", f"{FABRIC}/v1/workspaces/{ws}/items", {
        "displayName": "RetailAnalysis", "type": "SemanticModel",
        "definition": {"parts": [part("model.bim", "retail.bim"), part("data.json", "seed_data.json")]}},
        token=ft)
    opid = hdrs.get("x-ms-operation-id")
    dataset = None
    for _ in range(60):
        if http("GET", f"{FABRIC}/v1/operations/{opid}", token=ft)[2].get("status") == "Succeeded":
            dataset = http("GET", f"{FABRIC}/v1/operations/{opid}/result", token=ft)[2]["id"]
            break
        time.sleep(0.1)
    if not dataset:
        raise SystemExit("semantic-model item create did not complete")
    pbi = token(PBI_AUDIENCE + "/.default")
    log(f"workspace={ws} dataset={dataset}")

    log("running Great Expectations")
    subprocess.run([sys.executable, "-u", os.path.join(DIR, "driver.py")], check=True, env={
        **os.environ, "FABRIC_URL": FABRIC, "WS": ws, "DATASET": dataset, "PBI_TOKEN": pbi})
except Exception:
    for name, path in logs.items():
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
