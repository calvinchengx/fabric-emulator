"""An Environment item supplies a package the runtime image does not have.

This is the witness docs/37 §1 demands, and the reason its grade has not moved:

    the witness must be an e2e that imports a package only an Environment could
    have supplied, and the grade moves to 🟢 then and not before. A parser test
    must never again stand behind an applied-to-the-session claim.

So the shape of this test is the point. It does not assert that a request was
composed, or that a package list was parsed — the unit tests do that, one layer
below the claim. It asserts that **a notebook statement imports a module that
was not in the image**, which is the only evidence that the Environment reached
the session and did something.

The negative half matters just as much: the same import is run FIRST, on a
session with no Environment, and must FAIL. Without that, a package which
happened to be in the image would make this pass while proving nothing — the
exact "accepted but inert" trap the engine-matrix probes are written to avoid.
"""

import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://fabric-emulator"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT = "00d88624-f0d7-46f6-a641-6232c2608928"
SECRET = "daemon-app-secret"

# Small, pure-Python, no dependencies, and certainly not in the agent image.
# The import is the assertion, so the package only has to be absent and cheap.
PACKAGE = "cowsay"


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
    with urllib.request.urlopen(req) as r:
        raw = r.read()
        return r.status, (json.loads(raw) if raw else None)


def fail(msg):
    print(f"FAIL: {msg}", file=sys.stderr)
    raise SystemExit(1)


def wait_health(url, deadline=90):
    end = time.time() + deadline
    while time.time() < end:
        try:
            urllib.request.urlopen(url, timeout=3).read()
            return
        except Exception:  # noqa: BLE001 - polling until it answers
            time.sleep(1)
    fail(f"{url} never became healthy")


def main():
    wait_health(f"{FABRIC}/health")
    _, tok = http("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials", "client_id": CLIENT,
        "client_secret": SECRET, "scope": "https://api.fabric.microsoft.com/.default",
    }, form=True)
    token = tok["access_token"]

    _, ws = http("POST", f"{FABRIC}/v1/workspaces", {"displayName": "env-ws"}, token=token)
    _, lake = http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses",
                   {"displayName": "lake"}, token=token)
    base = f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses/{lake['id']}/livyapi/versions/2023-12-01"

    import base64
    payload = base64.b64encode(f"{PACKAGE}\n".encode()).decode()
    status, _ = http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items", {
        "displayName": "team-env", "type": "Environment",
        "definition": {"parts": [{"path": "Libraries/requirements.txt",
                                  "payloadType": "InlineBase64", "payload": payload}]},
    }, token=token)
    if status != 202:
        fail(f"creating the Environment answered {status}, want 202")
    # Poll the item list rather than the operation: either resolves the id, and
    # the list needs no header plumbing.
    env_id = None
    for _ in range(60):
        _, items = http("GET", f"{FABRIC}/v1/workspaces/{ws['id']}/items", token=token)
        for it in (items or {}).get("value", []):
            if it.get("displayName") == "team-env":
                env_id = it["id"]
        if env_id:
            break
        time.sleep(0.5)
    if not env_id:
        fail("the Environment item never appeared")

    def run(hcsid, replid, code_str):
        # Retry the submit until the agent's SparkSession is up: the emulator
        # answers 502 while it cannot reach the agent, and the agent starts
        # slower than the control plane. Same handling as e2e/livy.
        st = None
        for _ in range(90):
            try:
                _, st = http("POST",
                             f"{base}/highConcurrencySessions/{hcsid}/repls/{replid}/statements",
                             {"code": code_str}, token=token)
                break
            except urllib.error.HTTPError as err:
                if err.code == 502:
                    time.sleep(2)
                    continue
                raise
        if st is None:
            fail("the Spark agent never became reachable")
        for _ in range(120):
            _, got = http("GET",
                          f"{base}/highConcurrencySessions/{hcsid}/repls/{replid}/statements/{st['id']}",
                          token=token)
            if got.get("state") == "available":
                out = got.get("output") or {}
                return out.get("status"), json.dumps(out.get("data") or out.get("evalue") or "")
            time.sleep(0.5)
        fail("statement never became available")

    probe = f"import {PACKAGE}; '{PACKAGE} is importable'"

    # 1. WITHOUT an Environment: the import must fail. This is what makes the
    #    positive half evidence rather than coincidence.
    _, plain = http("POST", f"{base}/highConcurrencySessions", {"sessionTag": "plain"}, token=token)
    st, detail = run(plain["sessionId"], plain["replId"], probe)
    if st != "error":
        fail(f"{PACKAGE} imported WITHOUT an Environment ({st}) — the package is already "
             f"in the image, so this test cannot prove anything. Pick one that is not.")
    print(f"OK: without an Environment, `import {PACKAGE}` fails as it must", flush=True)
    http("DELETE", f"{base}/highConcurrencySessions/{plain['id']}", token=token)

    # 2. WITH the Environment bound: the same import must succeed.
    _, hc = http("POST", f"{base}/highConcurrencySessions",
                 {"sessionTag": "env", "environmentId": env_id}, token=token)
    st, detail = run(hc["sessionId"], hc["replId"], probe)
    if st != "ok":
        fail(f"with the Environment bound, `import {PACKAGE}` still failed: {detail}")

    print(f"OK: `import {PACKAGE}` succeeds only with the Environment bound — "
          f"the item reached the session and installed it", flush=True)
    http("DELETE", f"{base}/highConcurrencySessions/{hc['id']}", token=token)


try:
    main()
except urllib.error.HTTPError as err:  # report the body, not just the code
    fail(f"{err.code} {err.reason}: {err.read()[:400]!r}")
