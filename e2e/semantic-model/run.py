#!/usr/bin/env python3
"""e2e: real DAX over a semantic model via the executeQueries REST contract.

Stands up entra + fabric, publishes the golden `retail.bim` model + `data.json`
as a SemanticModel item, mints a Power BI-audience token, then POSTs each golden
DAX query to `executeQueries` (the exact swagger path) and asserts the rows
match the hand-computed oracle in `fixtures/golden_queries.json`.

Self-contained, stdlib-only; run: python3 e2e/semantic-model/run.py
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
WORK = os.path.join(tempfile.gettempdir(), "semantic-model-e2e")
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


def norm_row(row):
    """A comparable key for a result row: numbers folded to float, order-free."""
    out = []
    for k, v in row.items():
        out.append((k, float(v) if isinstance(v, (int, float)) else v))
    return tuple(sorted(out, key=lambda x: x[0]))


def rows_match(got, want):
    return sorted(map(norm_row, got)) == sorted(map(norm_row, want))


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

    # Publish the SemanticModel item with model + data as definition parts.
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
    log(f"workspace={ws} dataset={dataset}")

    pbi = token(PBI_AUDIENCE + "/.default")

    # Run each DAX golden query through executeQueries and check the rows.
    golden = json.load(open(os.path.join(FIX, "golden_queries.json")))
    ran = 0
    for q in golden["queries"]:
        if q["handler"] != "dax":
            continue
        ran += 1
        url = f"{FABRIC}/v1.0/myorg/groups/{ws}/datasets/{dataset}/executeQueries"
        _, _, resp = http("POST", url, {"queries": [{"query": q["dax"]}]}, token=pbi)
        rows = resp["results"][0]["tables"][0]["rows"]
        if not rows_match(rows, q["expected"]["rows"]):
            raise SystemExit(f"{q['name']}: rows mismatch\n got={rows}\nwant={q['expected']['rows']}")
        log(f"{q['name']}: {len(rows)} rows OK")
    if ran != 3:
        raise SystemExit(f"expected 3 DAX golden queries, ran {ran}")

    print("\nSEMANTIC-MODEL E2E: PASS", flush=True)
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
