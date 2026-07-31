#!/usr/bin/env python3
"""Prove Spark actually computes, not merely that its containers are up.

The compose healthchecks on sail and spark-agent answer "is the port listening"
and "does the agent's /health respond". Neither says a statement can execute,
which is the thing a notebook depends on. This opens a real Livy session and
runs statements through it: Livy REST -> spark-agent -> sail (Spark Connect).

Run standalone, or via `scripts/status.sh --spark`. Stdlib only (no pyspark
needed here — the agent holds the Spark Connect client, not this caller).
Exit 0 = Spark computes, 1 = it does not.
"""
import json
import os
import ssl
import sys
import time
import urllib.parse
import urllib.request

FABRIC = os.environ.get("FABRIC_URL", "https://localhost:9443").rstrip("/")
ENTRA = os.environ.get("ENTRA_URL", "https://localhost:8443").rstrip("/")
TENANT = os.environ.get("FABRIC_TENANT", "11111111-1111-1111-1111-111111111111")
CLIENT_ID = os.environ.get("FABRIC_CLIENT_ID", "cccccccc-0000-0000-0000-000000000002")
CLIENT_SECRET = os.environ.get("FABRIC_CLIENT_SECRET", "daemon-app-secret")

ctx = ssl.create_default_context()  # family self-signed TLS
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE


class CheckError(Exception):
    """A clean failure: this script reports status, so a stack trace is noise."""


# Short by design. A hung statement means Spark is not answering, which is the
# result we want reported, not something to wait a minute for: this runs inside
# `status.sh`, where a slow check is a check people stop running.
TIMEOUT = int(os.environ.get("SPARK_CHECK_TIMEOUT", "20"))


def http(method, url, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, context=ctx, timeout=TIMEOUT) as r:
            raw = r.read()
            return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raise CheckError(f"{method} {urllib.parse.urlparse(url).path} -> HTTP {e.code}") from e
    except Exception as e:  # timeout, connection refused, TLS, malformed body
        raise CheckError(f"{method} {urllib.parse.urlparse(url).path} -> {type(e).__name__}: {e}") from e


def fail(msg):
    raise CheckError(msg)


def main():
    body = urllib.parse.urlencode({
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET,
        "scope": "https://api.fabric.microsoft.com/.default"}).encode()
    try:
        token = json.load(urllib.request.urlopen(
            urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=body),
            context=ctx, timeout=15))["access_token"]
    except Exception as e:
        raise CheckError(f"could not mint a token from entra: {e}") from e

    workspaces = http("GET", f"{FABRIC}/v1/workspaces", token=token).get("value", [])
    if not workspaces:
        # Not a Spark fault: Livy sessions are addressed through a lakehouse.
        print("  skip  no workspace/lakehouse yet, so there is nothing to open a session on")
        return 0
    ws = workspaces[0]
    lakes = http("GET", f"{FABRIC}/v1/workspaces/{ws['id']}/items?type=Lakehouse",
                 token=token).get("value", [])
    if not lakes:
        print("  skip  no lakehouse in the workspace to open a session on")
        return 0

    base = (f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses/{lakes[0]['id']}"
            f"/livyapi/versions/2023-12-01")
    sess = http("POST", f"{base}/sessions", {"kind": "pyspark"}, token=token)
    sid = sess["id"]
    state = sess.get("state")
    try:
        for _ in range(120):
            state = http("GET", f"{base}/sessions/{sid}", token=token)["state"]
            if state in ("idle", "running"):
                break
            if state in ("dead", "error", "killed"):
                fail(f"livy session died on startup (state={state})")
            time.sleep(1)
        else:
            fail(f"livy session never became idle (last state={state})")
        print(f"  ok    livy session {sid} is {state}")

        def run(code):
            st = http("POST", f"{base}/sessions/{sid}/statements", {"code": code}, token=token)
            for _ in range(180):
                got = http("GET", f"{base}/sessions/{sid}/statements/{st['id']}", token=token)
                if got["state"] == "available":
                    out = got["output"]
                    if out.get("status") != "ok":
                        fail(f"statement failed: {json.dumps(out)[:300]}")
                    return out["data"]["text/plain"].strip()
                time.sleep(1)
            fail("statement never became available")

        if (got := run("spark.range(5).count()")) != "5":
            fail(f"spark.range(5).count() = {got}, want 5")
        print("  ok    spark.range(5).count() = 5 (the engine really computed)")

        print(f"  ok    engine version {run('spark.version')}")

        # A DataFrame surviving into the next statement is what makes this a
        # REPL rather than one-shot submits — exactly what notebooks rely on.
        run("_probe = spark.createDataFrame([(1,),(2,),(3,)], ['id'])")
        if (got := run("_probe.filter(_probe.id >= 2).count()")) != "2":
            fail(f"session did not persist state: got {got}, want 2")
        print("  ok    session state persists across statements")
    finally:
        try:
            http("DELETE", f"{base}/sessions/{sid}", token=token)
        except Exception:  # noqa: BLE001 — best-effort cleanup
            pass
    print("  Spark computes.")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except CheckError as err:
        print(f"  FAIL  {err}")
        sys.exit(1)
    except KeyboardInterrupt:
        sys.exit(130)
