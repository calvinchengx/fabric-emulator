#!/usr/bin/env python3
"""e2e: Microsoft's `sempy` driven against THE EMULATOR.

This is the WITNESS. Its predecessor drove sempy against a hand-written stub and
measured what the client demands; those demands are now implemented in
`internal/api/xmla.go` and `internal/xmla`, and unit-tested there. Keeping the
stub too would be two copies of one contract, which is the defect this repo has
a standing rule about — so the stub is gone and this suite proves the real
thing.

What it establishes, none of it inferred:

  1. sempy is redirectable at a host we name, with no Fabric runtime, capacity
     or notebook — `StaticFabricContext(pbi_shared_host=...)` points BOTH its
     transports (REST and `powerbi://` XMLA) at the emulator.
  2. A REAL Microsoft client completes the whole connect sequence against the
     emulator: routing, generateastoken, getDatabaseName, clusterResolve, the
     :443 dial, and the XMLA session handshake.
  3. `evaluate_dax` returns the emulator's own DAX result over XMLA.
  4. The TOM metadata path works: one Execute carrying a <Batch> of ~35
     <Discover RequestType=TMSCHEMA_*>, answered from the published model.
  5. The DataFrames carry the SEEDED MODEL'S content — not merely a frame.

The credential is minted by entra-emulator, because the emulator validates the
issuer: a self-signed token would prove only that our own stub accepted our own
token.

PLATFORM. sempy's XMLA path cannot run natively on macOS arm64 (pythonnet finds
no .NET runtime and `Microsoft.Fabric.SemanticLink.XmlaTools` will not load), so
the client runs in a linux/amd64 container. It reaches the emulator as
`api.fabric.microsoft.com`, which is in the emulator's own TLS SANs, via a
docker alias and a socat publish of :443 — the client dials 443 after routing
whatever port the Data Source named, because the endpoint address is derived.
"""
import base64
import json
import os
import shutil
import socket
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
sys.path.insert(0, os.path.join(REPO, "e2e"))
from entra_install import module_version  # noqa: E402

WORK = tempfile.mkdtemp(prefix="sempy-e2e-")
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18543")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19543")
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
ENTRA = f"https://localhost:{ENTRA_PORT}"
FABRIC = f"https://127.0.0.1:{FABRIC_PORT}"
PBI_AUDIENCE = "https://analysis.windows.net/powerbi/api"
# In the emulator's TLS SANs, so the container can validate the chain without
# any product change. Also what other e2e suites alias.
CLIENT_HOST = "api.fabric.microsoft.com"
FORWARDER = "sempy-e2e-443"
EXE = ".exe" if os.name == "nt" else ""
REQUIRED = os.environ.get("SEMPY_REQUIRE") == "1"

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE
procs = []


def log(m):
    print(f"==> {m}", flush=True)


def skip_or_fail(reason):
    if REQUIRED:
        raise SystemExit(f"FAILED: {reason}\n  SEMPY_REQUIRE=1, so this may not skip.")
    print(f"SKIPPED: {reason}")
    raise SystemExit(0)


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
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, context=_CTX, timeout=30) as r:
        raw = r.read()
        # LOWERCASED keys: `dict(r.headers)` preserves the canonical case Go
        # sends (`X-Ms-Operation-Id`), so a lowercase lookup silently misses and
        # an async create looks synchronous — which then polls
        # `/operations/None` and 404s, naming a route rather than the real
        # mistake.
        hdrs = {k.lower(): v for k, v in r.headers.items()}
        return r.status, hdrs, (json.loads(raw) if raw else {})


def require_free_port(port, what):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.5)
        if s.connect_ex(("127.0.0.1", int(port))) == 0:
            raise SystemExit(
                f"port {port} is in use, so this harness cannot start its own {what};\n"
                f"  a health check would pass against the OTHER service and every token\n"
                f"  would carry the wrong issuer.\n"
                f"  Free it, or: ENTRA_PORT=<free> FABRIC_PORT=<free> python3 {__file__}")


def wait_healthy(url, deadline=90):
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


def start(name, cmd, env):
    """Spawn from WORK, not the caller's cwd.

    `entra-emulator` may resolve through a goenv shim, and goenv picks its Go
    version FROM THE CURRENT DIRECTORY — so the same binary on the same PATH
    runs from /tmp and fails from the repo with
    `goenv: entra-emulator: command not found`. A launched service should not
    depend on where its launcher happened to be standing.
    """
    fh = open(os.path.join(WORK, name + ".log"), "w")
    procs.append((name, subprocess.Popen(cmd, env=env, cwd=WORK,
                                         stdout=fh, stderr=subprocess.STDOUT), fh))


def dump_logs(tail=25):
    """Show the services' own logs on failure.

    Without this the harness reports the CLIENT's error and hides the server's,
    so an emulator 401 reads as a client problem. The logs live in a temp dir
    the harness alone knows.
    """
    for name in ("fabric", "entra"):
        path = os.path.join(WORK, name + ".log")
        if not os.path.exists(path):
            continue
        with open(path) as fh:
            lines = fh.read().splitlines()[-tail:]
        if lines:
            print(f"\n---- {name} log (last {len(lines)}) ----", flush=True)
            for ln in lines:
                print("  " + ln, flush=True)


def cleanup():
    subprocess.run(["docker", "rm", "-f", FORWARDER], capture_output=True)
    for name, p, fh in procs:
        p.terminate()
        try:
            p.wait(timeout=10)
        except subprocess.TimeoutExpired:
            p.kill()
        fh.close()


if not shutil.which("docker"):
    skip_or_fail("docker is not on PATH; sempy's XMLA path needs a linux/amd64 "
                 ".NET runtime and cannot run on this host directly")

require_free_port(ENTRA_PORT, "entra")
require_free_port(FABRIC_PORT, "fabric")

log(f"work dir: {WORK}")
log("building the emulator")
fabric_bin = os.path.join(WORK, "fabric-emulator" + EXE)
subprocess.run(["go", "build", "-C", REPO, "-o", fabric_bin, "./cmd/fabric-emulator"],
               check=True)
# PIN THE VERSION, do not trust PATH. `ensure_entra_emulator` returns a PATH hit
# without checking its version, and a locally-built `dev` entra predates the
# family GUID migration — its tenant is not the one go.mod pins, so every token
# fails as `AADSTS90002: Unknown tenant`. A suite that pins a version in go.mod
# must not silently witness against whatever is installed: "which binary did
# this prove?" has to have one answer.
entra_ver = module_version("github.com/calvinchengx/entra-emulator")
log(f"installing entra-emulator {entra_ver} (ignoring any PATH build)")
subprocess.run(["go", "install",
                f"github.com/calvinchengx/entra-emulator/cmd/entra-emulator@{entra_ver}"],
               check=True, cwd=REPO, env={**os.environ, "GOBIN": WORK})
entra_bin = os.path.join(WORK, "entra-emulator" + EXE)

try:
    log(f"starting entra on :{ENTRA_PORT}, fabric on :{FABRIC_PORT} (TLS ON)")
    start("entra", [entra_bin], {**os.environ, "ORIGIN_MODE": "compat",
          "PORT": ENTRA_PORT, "DB_PATH": os.path.join(WORK, "entra.sqlite"),
          "TLS_CERT_DIR": os.path.join(WORK, "entra-tls")})
    # TLS stays ON: the client will not speak plain HTTP to an XMLA endpoint,
    # and the emulator's own cert already carries api.fabric.microsoft.com.
    start("fabric", [fabric_bin, "-addr", f"0.0.0.0:{FABRIC_PORT}",
                     "-data-dir", os.path.join(WORK, "data"),
                     "-entra-issuer", f"https://localhost:{ENTRA_PORT}/{TENANT}/v2.0",
                     "-entra-tls-insecure"], {**os.environ, "FABRIC_TRACE": "1"})
    wait_healthy(f"{ENTRA}/health")
    wait_healthy(f"{FABRIC}/health")

    log("seeding the Power BI resource app")
    try:
        http("POST", f"{ENTRA}/admin/api/apps",
             {"displayName": "Power BI Service", "appIdUri": PBI_AUDIENCE,
              "isConfidential": False})
    except urllib.error.HTTPError as e:
        # 409 = already seeded. 404 = this entra version has no such admin route
        # and pre-seeds the resource app itself; the token call below is the
        # real check either way, so failing here would be failing on a detail
        # that does not decide anything.
        if e.code not in (404, 409):
            raise

    def token(scope):
        return http("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token",
                    {"grant_type": "client_credentials", "client_id": CLIENT_ID,
                     "client_secret": CLIENT_SECRET, "scope": scope},
                    form=True)[2]["access_token"]

    ft = token("https://api.fabric.microsoft.com/.default")
    ws = http("POST", f"{FABRIC}/v1/workspaces", {"displayName": "sempy-ws"},
              token=ft)[2]["id"]

    def part(path, fname):
        return {"path": path, "payloadType": "InlineBase64",
                "payload": base64.b64encode(open(os.path.join(FIX, fname), "rb").read()).decode()}

    _, hdrs, created = http("POST", f"{FABRIC}/v1/workspaces/{ws}/items", {
        "displayName": "RetailAnalysis", "type": "SemanticModel",
        "definition": {"parts": [part("model.bim", "retail.bim"),
                                 part("data.json", "seed_data.json")]}}, token=ft)
    # BOTH SHAPES: item creation may complete synchronously (the response body
    # carries the item) or return 202 with an operation to poll. Assuming the
    # async one polls `/operations/None` and 404s, which reads as a missing
    # route rather than a wrong assumption about the response.
    opid = hdrs.get("x-ms-operation-id")
    dataset = created.get("id") if isinstance(created, dict) else None
    for _ in range(0 if dataset or not opid else 120):
        if http("GET", f"{FABRIC}/v1/operations/{opid}", token=ft)[2].get("status") == "Succeeded":
            dataset = http("GET", f"{FABRIC}/v1/operations/{opid}/result", token=ft)[2]["id"]
            break
        time.sleep(0.1)
    if not dataset:
        raise SystemExit("FAILED: the SemanticModel item never finished creating")
    log(f"workspace={ws} dataset={dataset}")

    pbi = token(PBI_AUDIENCE + "/.default")

    # The client dials :443 after routing whatever port the Data Source named,
    # so publish 443 into the emulator. Distinct container name from e2e/xmla's,
    # or one suite's cleanup kills the other's forwarder mid-run.
    subprocess.run(["docker", "rm", "-f", FORWARDER], capture_output=True)
    fwd = subprocess.run(
        ["docker", "run", "-d", "--rm", "--name", FORWARDER,
         "--add-host", "host.docker.internal:host-gateway", "-p", "443:443",
         "alpine/socat", "TCP-LISTEN:443,fork,reuseaddr",
         f"TCP:host.docker.internal:{FABRIC_PORT}"], capture_output=True, text=True)
    if fwd.returncode:
        skip_or_fail("could not publish 443; the client dials it after routing, so "
                     "nothing past the token would reach the emulator")
    log("443 forwarder up")

    log("building the sempy image (cached after the first run)")
    if subprocess.run(["docker", "build", "--platform", "linux/amd64", "-q",
                       "-t", "sempy-e2e:local", os.path.join(DIR, "image")],
                      capture_output=True, text=True).returncode:
        raise SystemExit("FAILED: could not build the sempy image")

    cert = os.path.join(WORK, "data", "tls", "cert.pem")
    if not os.path.exists(cert):
        raise SystemExit(f"FAILED: the emulator did not write a TLS cert at {cert}")

    log("running sempy against the emulator (linux/amd64)")
    proc = subprocess.run([
        "docker", "run", "--rm", "--platform", "linux/amd64",
        "--add-host", f"{CLIENT_HOST}:host-gateway",
        "-v", f"{os.path.join(DIR, 'driver.py')}:/driver.py:ro",
        "-v", f"{cert}:/usr/local/share/ca-certificates/fabric-emulator.crt:ro",
        "-e", f"SEMPY_HOST=https://{CLIENT_HOST}/",
        "-e", f"SEMPY_WORKSPACE={ws}", "-e", "SEMPY_DATASET=RetailAnalysis",
        "-e", f"SEMPY_TOKEN={pbi}",
        "sempy-e2e:local", "sh", "-c",
        "update-ca-certificates >/dev/null 2>&1; "
        "REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt "
        "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt /v/bin/python /driver.py",
    ], capture_output=True, text=True, timeout=1800)
finally:
    cleanup()

out = [ln[3:] for ln in proc.stdout.splitlines() if ln.startswith("###")]
if not out:
    raise SystemExit("FAILED: the driver produced no cases — that is a missing "
                     "measurement, not a result.\n" + proc.stderr[-1500:])
print("\n---- driver ----", flush=True)
for ln in out:
    print("  " + ln, flush=True)

results, rows = {}, {}
for ln in out:
    if ln.startswith("RESULT "):
        parts = ln.split(" :: ")
        name = parts[0].split(" ", 1)[1]
        results[name] = parts[1] if len(parts) > 1 else "?"
        for p in parts:
            if p.startswith("rows="):
                rows[name] = int(p[5:])
rowtext = "\n".join(ln for ln in out if ln.startswith("ROW "))

CHECKS = [
    ("the entra-minted token is what sempy resolves",
     any(ln.startswith("AUTH mine=True") for ln in out)),
    ("list_workspaces returns a DataFrame from the emulator",
     results.get("list_workspaces") == "OK"),
    ("list_datasets sees the published SemanticModel",
     results.get("list_datasets") == "OK" and rows.get("list_datasets", 0) > 0),
    ("evaluate_dax returns the emulator's own DAX result over XMLA",
     results.get("evaluate_dax") == "OK"),
    ("the TOM metadata path returns DataFrames",
     all(results.get(f) == "OK" for f in
         ("list_measures", "list_tables", "list_columns", "list_partitions"))),
    ("list_tables carries the SEEDED model's tables, not an empty frame",
     rows.get("list_tables", 0) > 0),
    ("list_measures carries the seeded model's measures",
     rows.get("list_measures", 0) > 0),
    ("list_columns carries the seeded model's columns",
     rows.get("list_columns", 0) > 0),
]

print("\n---- witness ----", flush=True)
failed = [n for n, ok in CHECKS if not ok]
for n, ok in CHECKS:
    print(f"  {'PASS' if ok else 'FAIL'}  {n}", flush=True)
if failed:
    dump_logs()
    raise SystemExit(f"FAILED: {len(failed)} check(s):\n  " + "\n  ".join(failed))
print("\nPASSED: Microsoft's SemPy drives the emulator's XMLA surface.")
