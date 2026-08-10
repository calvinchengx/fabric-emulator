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

import fcntl  # POSIX-only, and so is this harness: it runs on ubuntu in CI
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


RUN_LOCK = os.path.join(tempfile.gettempdir(), "xmla-e2e.lock")
_lock_fd = None


def require_sole_run():
    """Refuse to start while another copy of this harness is running.

    THE BUG THIS CLOSES (it was misdiagnosed twice, so the mechanism is
    recorded rather than the symptom). This harness owns three FIXED global
    names: `WORK`, the container name in `FORWARDER`, and `PORT`. Setup is
    destructive on the first two — it `rmtree`s WORK and `docker rm -f`s the
    forwarder — so a second invocation does not merely collide with a first,
    it DISMANTLES it mid-flight:

      * WORK dies -> the probe's cert bind mount is path-based (virtiofs), so
        deleting the host file makes the file vanish inside the ALREADY RUNNING
        container. `update-ca-certificates` lists it with `find -L` and then
        reads it with `sed` (lines 161 and 101 of that script), and in the
        window between the two it disappears:
            sed: can't read /usr/local/share/ca-certificates/xmla-e2e.crt
      * the forwarder dies -> the client's post-token dial to 443 lands on
        nothing:
            Connection refused [::ffff:…]:443 (host.docker.internal:443)

    Both appear in captured screen logs, and both read as findings about
    ADOMD.NET rather than as harness damage — which is what made this expensive.

    `require_free_port` does not cover it: it guards PORT alone, and a run that
    overrides XMLA_PORT sails past it and still destroys WORK. The lock does
    cover it, because the exclusive resources are exclusive whatever the port —
    443 can only be published once.

    An earlier guard here removed DIRECTORIES found at cert.pem, on the theory
    that Docker had created them by mounting a missing source. That theory is
    FALSE and was tested: a directory mounted at that path leaves
    `update-ca-certificates` exiting 0. The directories were a second symptom
    of this same race (Docker recreating the source another run had deleted),
    never the cause.
    """
    global _lock_fd
    _lock_fd = open(RUN_LOCK, "w")
    try:
        fcntl.flock(_lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        raise SystemExit(
            "another e2e/xmla run holds the lock, and this harness cannot share "
            "its work dir, its 443 forwarder or its port.\n"
            f"  Wait for it to finish, or clear a crashed one: rm {RUN_LOCK}\n"
            "  Overriding XMLA_PORT does NOT make two runs safe — they still "
            "share the work dir.")
    _lock_fd.write(f"{os.getpid()}\n")
    _lock_fd.flush()
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
# Screen 9: answer generateastoken too. The reply contract was READ off the
# assembly rather than screened — `PbiPremiumAuthenticationHandle+MWCToken`
# declares a single [DataMember] `Token` — so this is not a hypothesis the way
# the routing shapes were. See e2e/xmla/contract and docs/32.
#
# Kept behind its own flag so the refusing control still refuses: that run is
# the regression witness for the first-call contract, and answering everywhere
# would destroy the thing it pins.
ANSWER_TOKEN = False
# Screen 11: answer /webapi/clusterResolve. Screen 10 discovered the call and
# the contract reader named its reply —
# `ASAzureUtility+PowerBIClusterResolutionResult` declares FixedClusterUri,
# DynamicClusterUri, NewTenantId, RuleDescription, TTLSeconds.
#
# This is the steering hook that has been open since screen 5: the client ASKS
# which cluster to use, so a URI naming our host AND PORT should bring the XMLA
# call back to the listener instead of to :443.
ANSWER_CLUSTER = False
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
    """SCREEN 8 — recover the generateastoken body, and name the segment read.

    Screen 7 ADVANCED: at one path segment or more, the client accepts the
    routing reply and POSTs `/metadata/v201606/generateastoken`. That body is
    the contract of the next call and the first sight of anything past routing
    — and screen 7 captured it and then failed to print it, because the run
    record had been narrowed to (method, path). The harness now prints headers
    and body in full; this run exists to collect what that one threw away.

    It also settles a question screen 7 raised but could not answer. The path
    segments are DISTINCT GUIDs, so whichever one the client echoes back as
    `capacityObjectId` names the index it reads:

      L1  one segment    — GUID-1 is the only candidate; the control
      L2  two segments   — GUID-1 or GUID-2 distinguishes first from last
      L5  five segments  — confirms against a long path, where an interior
                           index would otherwise look like "the last one"

    Nothing here sweeps depth again: screen 7 established that >=1 segment
    advances and 0 does not. Re-running the full ladder would cost five
    containers to re-derive a settled fact.
    """
    seg = [f"{n}" * 8 + f"-{n}{n}{n}{n}-{n}{n}{n}{n}-{n}{n}{n}{n}-" + f"{n}" * 12
           for n in range(1, 7)]

    def ws(capacity_uri, kind="Group", sku="Premium"):
        return {"id": "00000000-0000-0000-0000-000000000001", "name": WORKSPACE,
                "type": kind, "capacitySku": sku, "capacityUri": capacity_uri}

    ct = {"Content-Type": "application/json; charset=utf-8"}
    out = []
    for depth in (1, 2, 5):
        path = "".join(f"/{s}" for s in seg[:depth])
        out.append((f"L{depth}-path-{depth}-segment", 200, ct,
                    json.dumps([ws(f"https://{host}{path}/")]).encode()))
    return out


def _shapes_screen7(host):
    """SCREEN 7 — the path-segment ladder, plus a host-label arm. For the record.

    Screen 6 named the field: an empty `capacityUri` draws the client's own
    `InvalidDataException :: … capacity uri is null or empty!`, so it is read
    and validated; a non-empty one throws `IndexOutOfRangeException` inside
    `PbiPremiumAuthenticationHandle.TryResolvePbiWorkspace`, which derives two
    values nothing in the reply carries — `pbiDedicatedRolloutFqdn` and
    `capacityObjectId`.

    IndexOutOfRange means indexing past the end, so the variable to sweep is
    LENGTH. The failure mode picks the experiment; nothing here guesses at a
    URL format.

    Two things can be indexed, and screen 6 excluded neither:

      L0..L5  PATH DEPTH. The primary hypothesis — screen 6's A, C and D all
              had an EMPTY path and all failed identically. Host stays the
              listener so that a shape which finally parses dials back HERE and
              its next request is captured, which is the whole deliverable.
      H5, H7  HOST LABEL COUNT. NOT excluded by screen 6: its two hosts had 3
              and 4 dot-separated labels, so a parser wanting label 5 would
              fail on both exactly as observed. Cheap to rule out, and
              expensive to discover later. These cannot capture a follow-up
              request — the hosts do not resolve — so they are read for a
              CHANGE IN ERROR only.

    Every path segment is a distinct GUID. GUID-shaped because `capacityObjectId`
    is likely one and a malformed value would fail differently, confounding
    depth with format; DISTINCT so that if the client ever does dial back, the
    value it carries names which position it read.
    """
    seg = [f"{n}" * 8 + f"-{n}{n}{n}{n}-{n}{n}{n}{n}-{n}{n}{n}{n}-" + f"{n}" * 12
           for n in range(1, 7)]

    def ws(capacity_uri, kind="Group", sku="Premium"):
        return {"id": "00000000-0000-0000-0000-000000000001", "name": WORKSPACE,
                "type": kind, "capacitySku": sku, "capacityUri": capacity_uri}

    ct = {"Content-Type": "application/json; charset=utf-8"}
    out = []
    for depth in range(6):
        path = "".join(f"/{s}" for s in seg[:depth])
        out.append((f"L{depth}-path-{depth}-segment", 200, ct,
                    json.dumps([ws(f"https://{host}{path}/")]).encode()))
    for labels in (5, 7):
        fqdn = ".".join(chr(ord("a") + i) for i in range(labels))
        out.append((f"H{labels}-host-{labels}-label", 200, ct,
                    json.dumps([ws(f"https://{fqdn}/")]).encode()))
    return out


def _shapes_screen6(host):
    """SCREEN 6 — capacityUri, one shape per RUN. Kept for the record.

    Screen 5 got the routing reply CONSUMED: one call instead of six, both
    diagnostic errors gone, and the failure moved downstream to an
    `IndexOutOfRangeException` thrown inside `AdomdConnection.Open()`. That
    success broke the screen's own mechanism — the client only re-asks when it
    rejected the last answer, so an accepted shape ends the request stream and
    the 2nd and 3rd shapes are never served. A screen is a tool for FAILURE.
    Past the first success, shapes must vary across runs, one per process, so
    each gets a cold client with no resolved workspace cached.

    Two questions, answered together in one pass:

    1. WHICH parser throws. The probe now prints the exception's frames, so the
       method names itself instead of being guessed at one candidate URI per
       run. This is the same move as reading the assembly: ask the shipped code
       what it wants rather than screening the space of what it might.
    2. WHETHER capacityUri is implicated at all. B sends it empty. If the
       exception is unchanged between A and B, the field is not the variable
       and the frames from (1) are the only thing worth reading.

      A capacityUri = https://<listener>/  — screen 5's shape, the control that
        must reproduce the IndexOutOfRangeException for B..D to mean anything
      B capacityUri = ""                   — is this field implicated at all?
      C a real-shaped Power BI cluster host (unreachable, deliberately: what is
        under test is whether it PARSES, which happens before any connect)
      D authority only, no scheme          — if the parser wants a bare host
    """
    cluster = f"https://{host}/"
    real = "https://wabi-us-north-central-h-primary-redirect.analysis.windows.net/"

    def ws(capacity_uri, kind="Group", sku="Premium"):
        return {"id": "00000000-0000-0000-0000-000000000001", "name": WORKSPACE,
                "type": kind, "capacitySku": sku, "capacityUri": capacity_uri}

    ct = {"Content-Type": "application/json; charset=utf-8"}
    return [
        ("A-capacityUri-listener", 200, ct, json.dumps([ws(cluster)]).encode()),
        ("B-capacityUri-empty", 200, ct, json.dumps([ws("")]).encode()),
        ("C-capacityUri-real-shaped", 200, ct, json.dumps([ws(real)]).encode()),
        ("D-capacityUri-no-scheme", 200, ct, json.dumps([ws(host)]).encode()),
    ]


def _shapes_screen5(host):
    """SCREEN 5 — DERIVED FROM THE ASSEMBLY, not guessed. Kept for the record.

    Reflecting over Microsoft.AnalysisServices.AdomdClient 19.84.1 gives the
    contract the routing reply deserialises into:

        PbiPremiumAuthenticationHandle+Workspace201606
            id, name, type, capacitySku, capacityUri     (all [DataMember])

        WorkspaceType201606            = User | Group | Folder
        WorkspaceCapacitySkuType201606 = Shared | Premium
        ResolvePbiWorkspaceErrorReason = None | WorkspaceNotFound
                                       | WorkspaceNotOnPbiPremium
                                       | WorkspaceNameDuplicated

    Two things follow that no screen could have guessed. **No type in the
    assembly holds a Workspace201606 collection**, so the payload is a BARE
    ARRAY of these rather than an enveloped list — every enveloped shape tried
    so far was answering a question the client never asked. And the earlier
    screens sent `name` but never `type`, `capacitySku` or `capacityUri`, so
    the objects deserialised with those null.

    The error enum is the discriminator that makes this screen self-checking:
    a reply that reaches the workspace but fails its premium check reports
    WorkspaceNotOnPbiPremium, NOT WorkspaceNotFound. So a change in WHICH
    error appears is progress even when the connection still fails.

      A bare array, all five members, type=Group, capacitySku=Premium
      B same, enveloped in {"value": …} — the control that should now FAIL
        differently if the bare array is right
      C bare array, type=User (the personal-workspace path, which the resolver
        carries a dedicated `personalWorkspace` field for)
    """
    cluster = f"https://{host}/"
    def ws(kind, sku="Premium"):
        return {"id": "00000000-0000-0000-0000-000000000001", "name": WORKSPACE,
                "type": kind, "capacitySku": sku, "capacityUri": cluster}
    def j(o):
        return json.dumps(o).encode()

    ct = {"Content-Type": "application/json; charset=utf-8"}
    return [
        ("A-bare-array-Group-Premium", 200, ct, j([ws("Group")])),
        ("B-enveloped-control", 200, ct, j({"value": [ws("Group")]})),
        ("C-bare-array-User", 200, ct, j([ws("User")])),
    ]


SHAPE_INDEX = 0  # which shape this probe run serves; the phase-0 loop advances it

# SCREEN 12 — the form of the cluster URI, one variable.
#
# Screen 11 sent a full URL and resolution rejected it without opening a
# socket. If the client prefixes a scheme itself, "https://https://..." fails
# exactly that way. These three rungs separate the possibilities; the routing
# shape is pinned to the known-good L1 so the cluster form is the only thing
# that varies.
def _cluster_forms(host, port):
    return [
        ("full-url", f"https://{host}:{port}"),
        ("host-port", f"{host}:{port}"),
        ("host-only", f"{host}"),
    ]


CLUSTER_INDEX = 0
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
                # ONE shape per run, not per request. Rotating per request only
                # screens while shapes fail: the moment one is accepted the
                # client stops asking and the rest are never served (screen 5).
                # SHAPE_INDEX is set by the phase-0 loop, which restarts the
                # probe per shape so each meets a client with nothing cached.
                label, status, headers, body = shapes[SHAPE_INDEX]
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
        if ANSWER_CLUSTER and self.path.startswith("/webapi/clusterResolve"):
            # Both URIs name the listener's own host:port. If the client
            # honours them, its next request is the first XMLA/SOAP envelope
            # this project has seen; if it still dials 443, the cluster reply
            # is not what steers it and that is equally worth knowing.
            label, target = _cluster_forms(CONTAINER_HOST, PORT)[CLUSTER_INDEX]
            b = json.dumps({
                "FixedClusterUri": target, "DynamicClusterUri": target,
                "NewTenantId": None, "RuleDescription": "emulator",
                "TTLSeconds": 3600,
            }).encode()
            print(f"    -> answering 200 clusterResolve [{label}] -> {target}", flush=True)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)
            return
        if ANSWER_TOKEN and self.path.startswith("/metadata/v201606/generateastoken"):
            # {"Token": "<string>"} — the contract, read from the assembly.
            # The value is ours to mint: the client carries it into whatever it
            # asks next, and capturing THAT is the point of answering here.
            b = b'{"Token":"emulator-mwc-token"}'
            print("    -> answering 200 with the MWCToken contract", flush=True)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)
            return
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

# BEFORE the destructive setup below, not after: the whole point is that a
# second run must die while it can still do no damage.
require_sole_run()
require_free_port(PORT, "TLS capture listener")

shutil.rmtree(WORK, ignore_errors=True)
os.makedirs(WORK)
log("issuing a self-signed CA for the container's view of this host")
make_cert(WORK)

ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
# PROTOCOL_TLS_SERVER negotiates the best version BOTH sides accept, which still
# leaves TLS 1.0/1.1 on the table if the client offers nothing better. ADOMD.NET
# does far better than that, so pinning the floor costs this listener nothing and
# stops the suite modelling a downgrade nobody wants.
ctx.minimum_version = ssl.TLSVersion.TLSv1_2
ctx.load_cert_chain(os.path.join(WORK, "cert.pem"), os.path.join(WORK, "key.pem"))
srv = TLSServer(("0.0.0.0", PORT), Capture)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
threading.Thread(target=srv.serve_forever, daemon=True).start()
log(f"TLS capture listener on :{PORT}")

target = f"{CONTAINER_HOST}:{PORT}"
log(f"running ADOMD.NET against {target} (linux/amd64, {SDK_IMAGE})")
FORWARDER = "xmla-e2e-443"


def start_443_forwarder():
    """Publish host port 443 and pipe it to the capture listener.

    Screen 9: once the token is accepted the client opens its XMLA connection
    to the routing host on **443**, discarding the port in the Data Source —
    `capacityUri` carried `:18446` and was dialled at `:443` anyway. So the
    endpoint address is derived, and nothing in the reply redirects it.

    The harness cannot bind 443 itself (a privileged port; the Python process
    is not root), but the Docker daemon can publish it without sudo. socat is a
    plain TCP pipe, so TLS still terminates at the capture listener with the
    same self-signed cert the client already trusts — this adds a door, not a
    man in the middle.

    Returns True when the door is open; False (with a reason logged) when it is
    not, so the caller can degrade rather than hang.
    """
    subprocess.run(["docker", "rm", "-f", FORWARDER],
                   capture_output=True, text=True)
    proc = subprocess.run([
        "docker", "run", "-d", "--rm", "--name", FORWARDER,
        "--add-host", f"{CONTAINER_HOST}:host-gateway",
        "-p", "443:443", "alpine/socat",
        "TCP-LISTEN:443,fork,reuseaddr", f"TCP:{CONTAINER_HOST}:{PORT}",
    ], capture_output=True, text=True)
    if proc.returncode:
        err = (proc.stderr or "").strip()
        log(f"NOTE: could not publish 443 ({err.splitlines()[-1] if err else '?'}).")
        log("      The client dials 443 after the token, so anything past it "
            "will show connection-refused rather than a captured request.")
        return False
    log("443 forwarder up: the client's post-token connection now reaches the listener")
    return True


def stop_443_forwarder():
    subprocess.run(["docker", "rm", "-f", FORWARDER], capture_output=True, text=True)


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

FORWARDING_443 = start_443_forwarder()

ANSWER_ROUTING = True
# Screen 9: with the routing reply accepted, answer the token too. Everything
# past it still gets the SOAP fault, so the FIRST request the client makes
# carrying an accepted token is captured and then refused — which is the
# recording nobody in this project has ever had.
ANSWER_TOKEN = True
ANSWER_CLUSTER = True

# ONE PROBE RUN PER SHAPE. Screen 5 established that an accepted reply is
# cached for the life of the client process, so a second shape in the same run
# is never requested. Restarting the probe is what makes each shape meet a
# cold client — the cost is one container per shape, and the alternative is
# results that silently describe only the first.
runs = []   # [(label, [(method, path)], stdout)]
SHAPE_INDEX = 0  # L1: one path segment, established as accepted by screen 7
for CLUSTER_INDEX, (label, _target) in enumerate(_cluster_forms(CONTAINER_HOST, PORT)):
    with LOCK:
        REQUESTS.clear()
        SHAPE_LOG.clear()
    log(f"SCREEN 12 cluster form {CLUSTER_INDEX + 1}/3: {label}")
    try:
        proc2 = run_probe()
    except subprocess.TimeoutExpired:
        srv.shutdown()
        stop_443_forwarder()
        raise SystemExit(f"FAILED: the phase-0 probe for {label} did not finish "
                         f"within 15 minutes")
    if not any(ln.startswith("CASE ") for ln in proc2.stdout.splitlines()):
        # No CASE line at all means the probe never got as far as connecting —
        # a compile error, a missing package, or the empty-streams failure this
        # harness has seen before. Abort on the FIRST one: the alternative is
        # N containers producing N identical non-results.
        srv.shutdown()
        stop_443_forwarder()
        raise SystemExit(
            f"FAILED: the phase-0 probe for {label} reported no CASE line, so it "
            f"never reached a connection (exit {proc2.returncode}).\n"
            f"---- stdout ----\n{proc2.stdout.strip() or '(empty)'}\n"
            f"---- stderr ----\n{proc2.stderr.strip() or '(empty)'}")
    if not SHAPE_LOG:
        print(f"  WARNING {label}: served to no request — the client never asked, "
              f"so this shape was NOT under test.", flush=True)
    # Keep the WHOLE request, not just method and path. Screen 7 advanced past
    # routing for the first time and the new POST's body — the actual contract
    # of the next call — was captured in memory and then not printed, because
    # this list had been narrowed to two fields. The body is the deliverable;
    # the path is only the label on it.
    with LOCK:
        runs.append((label, [dict(r) for r in REQUESTS], proc2.stdout))
srv.shutdown()
stop_443_forwarder()

for label, reqs, out in runs:
    phase2 = [(r["method"], r["path"]) for r in reqs]
    print(f"\n---- PHASE 0 · {label}: {len(phase2)} request(s) ----", flush=True)
    for m, path in phase2:
        print(f"  {m} {path}", flush=True)
    beyond = [r for r in reqs
              if not r["path"].startswith("/powerbi/databases/v201606/workspaces")]
    if beyond:
        print("  ADVANCED — requests past routing, never before observed:", flush=True)
        # Body and headers IN FULL. This is the first sight of the call after
        # routing, and a truncated record of it costs another full run to
        # recover — which is exactly what happened when only the path was kept.
        for r in beyond:
            print(f"    {r['method']} {r['path']}", flush=True)
            for k, v in sorted(r.get("headers", {}).items()):
                if k in ("authorization", "cookie"):
                    v = f"<{len(v)} chars, not logged>"
                print(f"      {k}: {v}", flush=True)
            print(f"      BODY: {r.get('body') or '(empty)'}", flush=True)
    elif phase2 == phase1:
        print("  UNCHANGED — identical to the refused run, so the doubled routing "
              "call is unconditional rather than a 404 fallback.", flush=True)
    elif len(phase2) < len(phase1):
        # The refusing run shows what rejection costs: every connection re-asks,
        # once with PreferClientRouting and once without. FEWER calls than that
        # means the client did not re-ask — it kept the answer. That is
        # acceptance evidence independent of the error text, which is why it is
        # a separate verdict rather than folded into "stopped at routing".
        print(f"  CONSUMED — {len(phase1)} routing call(s) when refused, "
              f"{len(phase2)} here. The client stopped re-asking, so the reply "
              f"was accepted and any failure below is DOWNSTREAM of routing.",
              flush=True)
    else:
        print("  STOPPED AT ROUTING — the reply was rejected.", flush=True)
    # The frames, not just the message. An IndexOutOfRangeException says
    # "Index was outside the bounds of the array" and names nothing; its stack
    # names the method, which is what turns the next step from screening
    # candidate URIs into reading the parser.
    for ln in out.splitlines():
        if ln.startswith("CASE powerbi") or ln.lstrip().startswith(
                ("FRAME powerbi", "THREW powerbi", "INNER powerbi")):
            print(f"  {ln.strip()}", flush=True)

print("\n---- capacityUri screen ----", flush=True)
first = {label: next((ln for ln in out.splitlines()
                      if ln.startswith("CASE powerbi")), "(no powerbi case)")
         for label, _, out in runs}
# The BASELINE is the first shape, whatever it is called. Naming it in a string
# is how a screen quietly stops comparing against anything when the shapes are
# renamed for the next round.
baseline, _, _ = runs[0]
base = first[baseline]
for label, _, _ in runs:
    line = first[label]
    same = f"  same as {baseline}" if label != baseline and line == base else ""
    print(f"  {label:26} {line.split('::', 1)[-1].strip()}{same}", flush=True)
print(f"\nEvery shape reporting what {baseline} reports means the swept variable "
      f"is NOT the one, and the frames above are all that narrows it. A shape "
      f"that differs names what the parser wanted — and in a LADDER, the rung "
      f"where the error changes is the index it reaches for.", flush=True)
print("\nPASSED: the ADOMD.NET client contract is unchanged.")
