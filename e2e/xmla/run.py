#!/usr/bin/env python3
"""e2e: Microsoft's real ADOMD.NET client, on Linux, aimed at a host we name.

This is a CLIENT-CONTRACT oracle, not an emulator test. The emulator implements
no XMLA surface (docs/24 defers it on cost). What this pins down is what a real
XMLA client *demands* — the exact first call any future implementation would
have to answer — and the platform facts that make such an implementation
testable at all.

It exists because those facts were believed, wrongly, for months. docs/18
recorded XMLA as blocked because ADOMD.NET was "native .NET, not
endpoint-overridable" and had no CI oracle. Both were false, and the only thing
that established that was pointing the client at a listener and reading what
came out. A claim that costs a quarter of roadmap when wrong should be a check,
not a memory.

What it asserts, all of it measured off the wire:

  1. ADOMD.NET loads and runs on Linux/.NET 8 (`PLATFORM Unix/...`).
  2. `Data Source=powerbi://<host>:<port>/...` sends TLS to whatever host is
     named — the endpoint override that was said not to exist.
  3. It trusts an ordinary `update-ca-certificates` CA, the same route every
     other e2e here uses for localhost TLS. (Implied by 2: a rejected chain
     shows up as a completed TCP connect with no HTTP request, which this
     harness reports as such rather than as "nothing connected".)
  4. The bearer comes from the connection string, so nothing in the credential
     path is interactive or Windows-only.
  5. The first call is plain JSON REST, not SOAP:
         GET /powerbi/databases/v201606/workspaces?PreferClientRouting=true
         User-Agent: ASClient/...
         Authorization: Bearer <token from the connection string>
     This is the one that can change under us when ADOMD.NET ships a new
     version, and the reason this runs weekly rather than once.
  6. The `https://<host>/xmla` and bare `host:port` Data Source forms remain
     Windows-only on .NET Core, so a Linux oracle must use `powerbi://`.

What it does NOT establish: the client never reaches XMLA/SOAP — it is still in
workspace *routing* when the capture stub refuses it. So this says nothing about
how much of [MS-SSAS-T] a useful implementation needs, and the `L` sizing in
docs/24 is unchanged. Feasibility is measured here; cost is not.

Runs the .NET client in a container (the NuGet package is
`...retail.amd64`, hence `--platform linux/amd64` — native on CI, QEMU
elsewhere). stdlib-only orchestrator.
"""

import http.server
import json
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading

DIR = os.path.dirname(os.path.abspath(__file__))
WORK = os.path.join(tempfile.gettempdir(), "xmla-e2e")
# 18080 was the ad-hoc choice while this was a one-off; it is already taken by
# another suite here, and the guard below caught the collision on first run.
PORT = int(os.environ.get("XMLA_PORT", "18446"))
# The value asserted back out of the Authorization header. Not a credential —
# nothing validates it; it is a marker proving the connection string is what
# fed the header.
TOKEN = "xmla-e2e-marker-8f21"
# Reachable from inside the container; the cert is issued for it.
CONTAINER_HOST = "host.docker.internal"
SDK_IMAGE = "mcr.microsoft.com/dotnet/sdk:8.0"

# Set by CI. Without it a machine with no Docker skips, which is right locally
# and would be a lie in CI — a skipped suite that reports success is the exact
# failure this repo keeps producing (docs/10, entry eight).
REQUIRED = os.environ.get("XMLA_REQUIRE") == "1"


def log(msg):
    print(f"==> {msg}", flush=True)


def skip_or_fail(reason):
    if REQUIRED:
        raise SystemExit(f"FAILED: {reason}\n  XMLA_REQUIRE=1, so this may not skip.")
    print(f"SKIPPED: {reason}")
    raise SystemExit(0)


def require_free_port(port, what):
    """Refuse to start when something else already owns `port`.

    Without this, a bind failure is INDISTINGUISHABLE FROM SUCCESS: our listener
    dies and the probe's requests are captured by a stranger's service — or not
    captured at all, which here reads as "the client did not connect" and would
    retract a finding that is actually true.
    """
    # CONNECT, do not bind: SO_REUSEADDR lets a 127.0.0.1 bind succeed on macOS
    # while another socket holds 0.0.0.0 (exactly how a docker -p publish looks).
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.5)
        if s.connect_ex(("127.0.0.1", int(port))) == 0:
            raise SystemExit(
                f"port {port} is already in use, so this harness cannot start its own "
                f"{what}.\n"
                f"  Free the port (`docker ps | grep {port}`) or override it:\n"
                f"    XMLA_PORT=<free> python3 e2e/xmla/run.py")


# ---------------------------------------------------------------------------
# The capture listener: a TLS endpoint that records what arrives and refuses it.
#
# Its responses are deliberately unhelpful (404 to the routing GET, a SOAP fault
# to anything POSTed). That is not laziness — it is what was measured. Answering
# the routing call would send the client down a different path and the recorded
# first-call contract would no longer be the one a fresh client performs.
# ---------------------------------------------------------------------------

# PHASE 0 (docs/32-xmla-plan.md): answering the routing call is a SEPARATE run,
# never a change to the refusing one. The refusal is what pins the first-call
# contract — answer it in the same pass and the recorded contract becomes the
# one a client performs against a server that replies, which is a different
# fact. So the suite runs the probe TWICE: once refusing (the regression
# witness), once answering (the measurement).
ANSWER_ROUTING = False
WORKSPACE = "ws"   # the workspace the probe names in its Data Source

# CANDIDATE ROUTING SHAPES, screened one per request.
#
# The first Phase 0 run established that ADOMD.NET CONSUMES the reply — it
# stopped complaining about the response and started complaining about the
# CONTENT ("The specified Power BI workspace ('ws') is not found"). So the
# remaining unknown is narrow: which field does it match the Data Source's
# workspace name against, and does it expect the list wrapped?
#
# The probe opens three powerbi:// connections per run, each making exactly one
# routing call once answered. That is three shapes per five-minute run instead
# of one, and every shape is reported by its own connection's error — so this
# is a SCREEN, not a guess. If one advances, the next run narrows it; if none
# does, four hypotheses die at once.
def _shapes(host):
    """SCREEN 4 — NOT THE BODY. Screens 2 and 3 exhausted body content: every
    top-level JSON object returned the identical not-found, across envelope
    present/absent, empty/populated, ten field spellings, PascalCase and
    nesting. Only the top-level TYPE ever changed anything. So this screen
    leaves the JSON alone and varies what no body edit can reach.

    Each shape is (label, status, extra headers, body bytes).

      A 200 + EMPTY body -> the sharpest discriminator available. If not-found
                            still appears, the body is not consulted for this
                            decision AT ALL, and screens 2-3 were varying
                            something the client never read. If instead a parse
                            error appears, the body IS read and those shapes
                            were parsed-but-unmatched.
      B 307 redirect     -> `PreferClientRouting=true` may mean "tell me where
                            to go" via a redirect rather than a payload. Points
                            at this same listener, so following it is visible
                            as a second captured request.
      C text/plain       -> does Content-Type gate parsing? Same JSON that
                            produced not-found under application/json.
    """
    cluster = f"https://{host}/"
    ok_body = json.dumps({
        "@odata.context": f"{cluster}$metadata#workspaces",
        "value": [{"name": WORKSPACE, "clusterUri": cluster}],
    }).encode()
    return [
        ("A-200-empty-body", 200, {"Content-Type": "application/json; charset=utf-8"}, b""),
        ("B-307-redirect", 307, {"Location": f"{cluster}powerbi/databases/v201606/workspaces"}, b""),
        ("C-text-plain", 200, {"Content-Type": "text/plain; charset=utf-8"}, ok_body),
    ]


SHAPE_LOG = []   # [(shape name, request path)] — which shape answered which call

REQUESTS = []          # [{method, path, headers: {lower: value}, body}]
HANDSHAKE_ERRORS = []  # TCP connected, TLS refused — a distinct diagnosis
LOCK = threading.Lock()


class Capture(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _record(self):
        n = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(n) if n else b""
        with LOCK:
            REQUESTS.append({
                "method": self.command,
                "path": self.path,
                "headers": {k.lower(): v for k, v in self.headers.items()},
                "body": body.decode("utf-8", "replace"),
            })
        print(f"    <- {self.command} {self.path}", flush=True)

    def do_GET(self):
        self._record()
        if ANSWER_ROUTING and self.path.startswith("/powerbi/databases/v201606/workspaces"):
            # The routing reply, as a HYPOTHESIS rather than a known contract.
            # Nobody in this project has seen a real one; what is documented is
            # only that the client asks. The cluster is pointed back at this
            # same listener so that a client which follows routing returns here
            # and its NEXT request is captured — which is the entire deliverable
            # of Phase 0. If the client rejects this shape, its exception text
            # is the measurement instead, and just as useful.
            shapes = _shapes(self.headers.get("Host", ""))
            with LOCK:
                label, status, headers, body = shapes[len(SHAPE_LOG) % len(shapes)]
                SHAPE_LOG.append((label, self.path))
            print(f"    -> answering {status} with shape {label!r}", flush=True)
            self.send_response(status)
            for k, v in headers.items():
                self.send_header(k, v)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            if body:
                self.wfile.write(body)
            return
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        self._record()
        b = (b'<?xml version="1.0"?><soap:Envelope '
             b'xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>'
             b'<soap:Fault><faultcode>capture</faultcode>'
             b'<faultstring>capture only</faultstring></soap:Fault>'
             b'</soap:Body></soap:Envelope>')
        self.send_response(500)
        self.send_header("Content-Type", "text/xml")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def log_message(self, *a):
        pass


class TLSServer(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def get_request(self):
        try:
            return super().get_request()
        except ssl.SSLError as e:
            # The TCP connect landed and the handshake did not. Recorded rather
            # than swallowed, so "no requests captured" can be told apart from
            # "the client would not trust our CA" — two findings with opposite
            # meanings for whether XMLA is testable at all.
            with LOCK:
                HANDSHAKE_ERRORS.append(str(e))
            raise OSError(str(e))


def make_cert(dest):
    """Self-signed leaf valid for the container's view of the host."""
    if not shutil.which("openssl"):
        skip_or_fail("openssl is not on PATH, so no TLS cert can be issued")
    subprocess.run([
        "openssl", "req", "-x509", "-newkey", "rsa:2048",
        "-keyout", os.path.join(dest, "key.pem"),
        "-out", os.path.join(dest, "cert.pem"),
        "-days", "2", "-nodes", "-subj", f"/CN={CONTAINER_HOST}",
        "-addext",
        f"subjectAltName=DNS:{CONTAINER_HOST},DNS:localhost,IP:127.0.0.1",
    ], check=True, capture_output=True)


# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

FAILURES = []


def check(ok, what, detail=""):
    if ok:
        print(f"  PASS  {what}", flush=True)
    else:
        FAILURES.append(f"{what}{chr(10) + '        ' + detail if detail else ''}")
        print(f"  FAIL  {what}{'  — ' + detail if detail else ''}", flush=True)


def parse_cases(stdout):
    """`CASE <label> :: <outcome> [:: <detail>]` -> {label: (outcome, detail)}."""
    out = {}
    for line in stdout.splitlines():
        if not line.startswith("CASE "):
            continue
        parts = [p.strip() for p in line[len("CASE "):].split("::")]
        out[parts[0]] = (parts[1] if len(parts) > 1 else "",
                         parts[2] if len(parts) > 2 else "")
    return out


def assert_contract(stdout):
    cases = parse_cases(stdout)

    # 1. It runs here at all.
    plat = next((l for l in stdout.splitlines() if l.startswith("PLATFORM ")), "")
    check("Unix" in plat,
          "ADOMD.NET loads and runs on Linux/.NET 8",
          f"probe reported {plat!r}")

    # 2/3. Bytes reached the host we named, over a chain it accepted.
    if not REQUESTS:
        detail = (f"{len(HANDSHAKE_ERRORS)} TLS handshake(s) failed after connecting — "
                  f"the client did not accept our CA: {HANDSHAKE_ERRORS[:1]}"
                  if HANDSHAKE_ERRORS else
                  "nothing connected to the listener at all")
        check(False, "powerbi:// sends TLS to the host named in the Data Source", detail)
        return
    check(True, "powerbi:// sends TLS to the host named in the Data Source")
    check(True, "an update-ca-certificates CA is trusted (the handshake completed)")

    first = REQUESTS[0]

    # 5. The contract a future implementation must answer.
    check(first["method"] == "GET" and
          first["path"].startswith("/powerbi/databases/v201606/workspaces"),
          "first call is GET /powerbi/databases/v201606/workspaces",
          f"got {first['method']} {first['path']}")
    check("PreferClientRouting=true" in first["path"],
          "first call carries PreferClientRouting=true",
          f"query was {first['path']}")
    ua = first["headers"].get("user-agent", "")
    check(ua.startswith("ASClient"),
          "identifies itself as ASClient",
          f"User-Agent was {ua!r}")

    # 4. The credential path is non-interactive.
    check(first["headers"].get("authorization") == f"Bearer {TOKEN}",
          "bearer token comes from the connection string",
          f"Authorization was {first['headers'].get('authorization')!r}")

    # The powerbi:// forms must stay usable on Linux — a NotSupportedException
    # here would mean the oracle has no remaining connection form.
    for label in ("powerbi-userid", "powerbi-bare", "powerbi-claimstoken"):
        outcome = cases.get(label, ("<missing>", ""))
        check(outcome[0] not in ("NotSupportedException", "<missing>"),
              f"Data Source form {label} still reaches the network on Linux",
              f"probe reported {outcome[0]} {outcome[1]}")

    # 6. The documented Windows-only constraint, kept checkable.
    for label in ("http-xmla", "hostport"):
        outcome = cases.get(label, ("<missing>", ""))
        check(outcome[0] == "NotSupportedException",
              f"Data Source form {label} is still Windows-only",
              f"probe reported {outcome[0]} {outcome[1]} — if this form now works "
              f"on Linux, docs/18 and this harness should say so")


# ---------------------------------------------------------------------------

if not shutil.which("docker"):
    skip_or_fail("docker is not on PATH; the ADOMD.NET probe needs the .NET 8 SDK image")

require_free_port(PORT, "TLS capture listener")

shutil.rmtree(WORK, ignore_errors=True)
os.makedirs(WORK)
log("issuing a self-signed CA for the container's view of this host")
make_cert(WORK)

ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(os.path.join(WORK, "cert.pem"), os.path.join(WORK, "key.pem"))
srv = TLSServer(("0.0.0.0", PORT), Capture)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
threading.Thread(target=srv.serve_forever, daemon=True).start()
log(f"TLS capture listener on :{PORT}")

target = f"{CONTAINER_HOST}:{PORT}"
log(f"running ADOMD.NET against {target} (linux/amd64, {SDK_IMAGE})")
def run_probe():
    """One ADOMD.NET run against the listener. Identical both phases — only the
    listener's behaviour differs, so any change in what the client does is
    attributable to the routing reply and to nothing else."""
    return subprocess.run([
        "docker", "run", "--rm", "--platform", "linux/amd64",
        "--add-host", f"{CONTAINER_HOST}:host-gateway",
        "-v", f"{os.path.join(DIR, 'probe')}:/probe:ro",
        "-v", f"{os.path.join(WORK, 'cert.pem')}:/usr/local/share/ca-certificates/xmla-e2e.crt:ro",
        "-e", f"XMLA_TARGET={target}",
        "-e", f"XMLA_TOKEN={TOKEN}",
        "-w", "/work", SDK_IMAGE,
        "sh", "-c",
        # Each step announces itself and update-ca-certificates keeps its
        # stderr. The suppressed version produced a probe that exited 2 with
        # BOTH streams empty — a failure with no evidence in it, which cost two
        # runs to not-diagnose. Silencing the one command whose failure would
        # otherwise be invisible is exactly backwards.
        "set -e; echo 'STEP copy'; cp -r /probe/. /work/; "
        "echo 'STEP ca'; update-ca-certificates >/dev/null; "
        "echo 'STEP dotnet'; dotnet run --nologo",
    ], capture_output=True, text=True, timeout=900)


try:
    proc = run_probe()
except subprocess.TimeoutExpired:
    srv.shutdown()
    raise SystemExit("FAILED: the ADOMD.NET probe did not finish within 15 minutes")
if proc.returncode != 0 and not proc.stdout.strip() and not proc.stderr.strip():
    # Both streams empty is not a test result, it is a missing measurement.
    # Say so rather than letting it read as a probe verdict.
    log(f"probe exited {proc.returncode} with NO output on either stream — "
        f"the last STEP line above (if any) is where it stopped; no STEP line "
        f"at all means it never started, which is an environment fault rather "
        f"than a client contract change")

print("\n---- probe stdout ----", flush=True)
print(proc.stdout.strip() or "(empty)", flush=True)
if proc.returncode != 0:
    sys.stderr.write("\n---- probe stderr ----\n" + proc.stderr + "\n")
    raise SystemExit(f"FAILED: the probe exited {proc.returncode}")

print(f"\n---- captured {len(REQUESTS)} request(s), "
      f"{len(HANDSHAKE_ERRORS)} handshake failure(s) ----", flush=True)
for r in REQUESTS:
    print(f"  {r['method']} {r['path']}", flush=True)

print("\n---- client contract ----", flush=True)
assert_contract(proc.stdout)

if FAILURES:
    srv.shutdown()
    raise SystemExit("\nFAILED: the ADOMD.NET client contract changed:\n  - " +
                     "\n  - ".join(FAILURES))

# ---------------------------------------------------------------------------
# PHASE 0 — answer the routing call and record what the client does next.
#
# docs/32-xmla-plan.md: "The deliverable is not the feature. It is measurement."
# Nothing below is asserted as a contract, because no contract is known: the
# routing reply here is a HYPOTHESIS about a shape nobody in this project has
# observed. Three outcomes are all informative and all recorded:
#
#   * the client advances     -> its next request is printed; that is the thing
#                                the roadmap could not price
#   * the client rejects it   -> its exception names what the shape lacked
#   * nothing changes         -> the doubled routing call is unconditional
#                                rather than a 404 fallback, which the README
#                                flags as unestablished
#
# A future phase turns whichever of these happened into an assertion. Today it
# is a printed observation, so the suite cannot pass by confirming a guess.
# ---------------------------------------------------------------------------
phase1 = [(r["method"], r["path"]) for r in REQUESTS]

ANSWER_ROUTING = True
with LOCK:
    REQUESTS.clear()
log("PHASE 0: answering the routing call, recording what follows")
try:
    proc2 = run_probe()
except subprocess.TimeoutExpired:
    srv.shutdown()
    raise SystemExit("FAILED: the phase-0 probe did not finish within 15 minutes")
srv.shutdown()

phase2 = [(r["method"], r["path"]) for r in REQUESTS]
print(f"\n---- PHASE 0: {len(phase2)} request(s) with routing ANSWERED ----", flush=True)
for m, path in phase2:
    print(f"  {m} {path}", flush=True)
beyond = [x for x in phase2 if not x[1].startswith("/powerbi/databases/v201606/workspaces")]
print("\n---- PHASE 0 verdict ----", flush=True)
if beyond:
    print("ADVANCED — requests past routing, never before observed:", flush=True)
    for m, path in beyond:
        print(f"    {m} {path}", flush=True)
    body = next((r["body"] for r in REQUESTS
                 if not r["path"].startswith("/powerbi/databases/v201606/workspaces")), "")
    if body:
        print(f"    first body (first 400 chars): {body[:400]}", flush=True)
elif phase2 == phase1:
    print("UNCHANGED — the sequence is identical to the refused run, so the "
          "doubled routing call is unconditional rather than a 404 fallback.", flush=True)
else:
    print("STOPPED AT ROUTING — the reply was rejected. The probe's own error "
          "text below is what the shape lacked:", flush=True)
print("\n---- phase 0 probe stdout ----", flush=True)
print(proc2.stdout.strip() or "(empty)", flush=True)

# Attribute each connection's outcome to the shape that answered its routing
# call. Without this pairing the screen is three unlabelled results and proves
# nothing about which hypothesis died.
print("\n---- shape screen ----", flush=True)
outcomes = [ln for ln in proc2.stdout.splitlines() if ln.startswith("CASE powerbi")]
if len(SHAPE_LOG) != len(outcomes):
    print(f"  NOT PAIRED: {len(SHAPE_LOG)} shape(s) served, {len(outcomes)} powerbi "
          f"case(s) reported — the 1:1 assumption does not hold, so nothing below "
          f"is attributable.", flush=True)
for (label, _), line in zip(SHAPE_LOG, outcomes):
    # DIFFERENT means "the client changed its mind", NOT "this shape is
    # closer". A shape can differ by failing EARLIER — which is what
    # bare-array does — so the label states the fact and leaves the direction
    # to the reader rather than implying progress.
    verdict = "STILL NOT FOUND" if "is not found" in line else "*** DIFFERENT ***"
    print(f"  {label:22} {verdict}", flush=True)
    print(f"  {'':22} {line.strip()}", flush=True)
print("\nDIFFERENT = the client reacted differently; read the message to see "
      "whether that is progress or a regression. All STILL NOT FOUND means "
      "field spelling is not the variable and the next hypothesis must be "
      "structural. The single-shape run is the CONTROL: all three connection "
      "types reported identically there, so a difference here is attributable "
      "to the shape and not to the Data Source form.", flush=True)
print("\nPASSED: the ADOMD.NET client contract is unchanged.")
