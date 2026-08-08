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


def http(method, url, body=None, token=None, form=False, allow_error=False):
    """`allow_error` returns the status instead of raising, for the negative
    cases — asserting a 401 needs the code, not an exception."""
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
    try:
        with urllib.request.urlopen(req, context=_CTX) as r:
            raw = r.read()
            return r.status, r.headers, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        if not allow_error:
            raise
        raw = e.read()
        return e.code, e.headers, (json.loads(raw) if raw else {})



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

    # DISCOVERY, before any query: this is where a real Power BI REST client
    # starts. Everything above used the Fabric item API and its long-running
    # operation, which is how the PUBLISHER learns the id — a consumer has no
    # such handle and lists the workspace instead.
    #
    # Asserting the listed id equals the published one is the whole point. If
    # discovery returned a different id, or omitted the model, every query below
    # would still pass while a real client could not reach the model at all.
    _, _, listed = http("GET", f"{FABRIC}/v1.0/myorg/groups/{ws}/datasets", token=pbi)
    ids = [d["id"] for d in listed["value"]]
    if dataset not in ids:
        raise SystemExit(f"published dataset {dataset} not discoverable; listed {ids}")
    if "@odata.context" not in listed:
        raise SystemExit("datasets list is missing the OData wrapper the swagger defines")
    _, _, one = http("GET", f"{FABRIC}/v1.0/myorg/groups/{ws}/datasets/{dataset}", token=pbi)
    if one["id"] != dataset or not one.get("name"):
        raise SystemExit(f"dataset GET disagrees with the published item: {one}")
    log(f"discovered {len(ids)} dataset(s) by listing; {one['name']} is the published one")

    # A Fabric-audience token must not open the Power BI surface, the same way
    # it cannot open executeQueries.
    code, _, _ = http("GET", f"{FABRIC}/v1.0/myorg/groups/{ws}/datasets", token=ft, allow_error=True)
    if code != 401:
        raise SystemExit(f"datasets list accepted a Fabric-audience token: {code}")
    log("datasets list rejects a non-Power BI audience token (401)")

    # LINEAGE and REFRESHABILITY for this model, which is an INLINE one — its
    # rows are a data.json definition part. Both answers below are the negative
    # branch, and both are the honest answer rather than a gap:
    #
    #   datasources -> []     it genuinely reads nothing
    #   refresh     -> 400    there is nothing to re-read, so a Completed here
    #                         would tell a caller their numbers were brought up
    #                         to date when nothing was.
    #
    # The POSITIVE branch (a Direct Lake model: non-empty datasources, refresh
    # accepted) is witnessed in e2e/data-science-loop, which already builds one.
    _, _, srcs = http("GET", f"{FABRIC}/v1.0/myorg/groups/{ws}/datasets/{dataset}/datasources", token=pbi)
    if srcs.get("value") != []:
        raise SystemExit(f"an inline-data model reported datasources: {srcs}")
    log("datasources: [] — an inline model reads nothing, and says so")

    code, _, body = http("POST", f"{FABRIC}/v1.0/myorg/groups/{ws}/datasets/{dataset}/refreshes",
                         {"notifyOption": "NoNotification"}, token=pbi, allow_error=True)
    if code != 400:
        raise SystemExit(f"refresh of an inline-data model returned {code}, want 400: {body}")
    log("refresh refused (400) — nothing to re-read, and the error says why")

    # And isRefreshable on the dataset must agree with that refusal, or a client
    # that trusts the flag gets contradicted by the endpoint.
    _, _, one = http("GET", f"{FABRIC}/v1.0/myorg/groups/{ws}/datasets/{dataset}", token=pbi)
    if one.get("isRefreshable") is not False:
        raise SystemExit(f"isRefreshable={one.get('isRefreshable')} contradicts the 400 above")
    log("isRefreshable=false agrees with the refusal")

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
    if ran != golden["daxQueryCount"]:
        raise SystemExit(
            f"ran {ran} DAX golden queries, fixture declares {golden['daxQueryCount']}")

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
