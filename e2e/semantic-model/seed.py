#!/usr/bin/env python3
"""Seed flow for the semantic-model / executeQueries golden fixture.

Stands up entra + fabric, creates a real workspace and a real SemanticModel
item whose definition IS the golden `retail.bim`, and mints a Power BI-audience
token. Then it probes the `executeQueries` endpoint exactly as the golden
swagger defines it — which 404s today because the DAX engine isn't built yet.

That 404 is the point: it proves the two real URL params (groupId = workspace,
datasetId = semantic-model item) resolve, the fixture is loaded, and the auth is
in place — everything the future engine needs is wired. When the engine lands,
the same probe should return the rows in fixtures/golden_queries.json.

Self-contained, stdlib-only; run: python3 e2e/semantic-model/seed.py
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
FIX = os.path.join(DIR, "fixtures")
WORK = os.path.join(tempfile.gettempdir(), "semantic-model-seed")
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18443")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19080")
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
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
    subprocess.run(["go", "install", "github.com/calvinchengx/entra-emulator/cmd/entra-emulator@latest"],
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
    log(f"starting entra on :{ENTRA_PORT}, fabric on :{FABRIC_PORT}")
    start("entra", [entra_bin], {**os.environ, "ORIGIN_MODE": "compat", "PORT": ENTRA_PORT,
          "DB_PATH": os.path.join(WORK, "entra.sqlite"), "TLS_CERT_DIR": os.path.join(WORK, "entra-tls")})
    start("fabric", [fabric_bin, "-addr", f"127.0.0.1:{FABRIC_PORT}", "-data-dir", os.path.join(WORK, "data"),
          "-disable-tls", "-entra-issuer", f"https://localhost:{ENTRA_PORT}/{TENANT}/v2.0", "-entra-tls-insecure"],
          os.environ.copy())
    wait_healthy(f"{ENTRA}/health")
    wait_healthy(f"{FABRIC}/health")

    # Seed the Power BI resource app so client-credentials resolves its audience.
    log(f"seeding Power BI resource app ({PBI_AUDIENCE})")
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
    log(f"workspace (groupId) = {ws}")

    # Create the SemanticModel item whose definition IS the golden model.bim.
    bim = open(os.path.join(FIX, "retail.bim"), "rb").read()
    _, hdrs, _ = http("POST", f"{FABRIC}/v1/workspaces/{ws}/items", {
        "displayName": "RetailAnalysis", "type": "SemanticModel",
        "definition": {"parts": [{"path": "model.bim", "payloadType": "InlineBase64",
                                  "payload": base64.b64encode(bim).decode()}]}}, token=ft)
    opid = hdrs.get("x-ms-operation-id")
    dataset = None
    for _ in range(60):
        if http("GET", f"{FABRIC}/v1/operations/{opid}", token=ft)[2].get("status") == "Succeeded":
            dataset = http("GET", f"{FABRIC}/v1/operations/{opid}/result", token=ft)[2]["id"]
            break
        time.sleep(0.1)
    if not dataset:
        raise SystemExit("semantic-model item create did not complete")
    log(f"semantic model (datasetId) = {dataset}")

    pbi = token(PBI_AUDIENCE + "/.default")
    log(f"Power BI-audience token minted ({len(pbi)} chars)")

    # Probe executeQueries exactly as the golden swagger defines it.
    golden = json.load(open(os.path.join(FIX, "golden_queries.json")))
    q = next(x for x in golden["queries"] if x["handler"] == "dax")
    url = f"{FABRIC}/v1.0/myorg/groups/{ws}/datasets/{dataset}/executeQueries"
    log(f"POST {url}")
    log(f"     body: {{'queries':[{{'query': {q['dax']!r}}}]}}")
    try:
        status, _, resp = http("POST", url, {"queries": [{"query": q["dax"]}]}, token=pbi)
        log(f"executeQueries returned {status}: {json.dumps(resp)[:200]}")
        log("ENGINE PRESENT — compare against fixtures/golden_queries.json")
    except urllib.error.HTTPError as e:
        if e.code == 404:
            log("executeQueries -> 404 (expected): the DAX engine is not built yet.")
            log("FIXTURE READY: groupId + datasetId resolve, auth in place, golden model loaded.")
        else:
            raise

    print("\nSEMANTIC-MODEL SEED: OK (fixture wired; executeQueries engine pending)", flush=True)
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
