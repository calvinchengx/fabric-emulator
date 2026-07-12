#!/usr/bin/env python3
"""A real Livy client, unmodified in spirit: it speaks the documented Fabric
Livy REST contract and expects real Spark results. It proves the emulator's
native Livy termination + the Spark agent compute an actual answer — and that a
session is a *persistent* REPL (state survives across statements).

Stdlib only. Flow: entra token → workspace + lakehouse → Livy session → submit
PySpark statements → poll results.
"""
import json
import time
import urllib.error
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://fabric-emulator"
TENANT = "11111111-1111-1111-1111-111111111111"
LIVY = None  # set once workspace + lakehouse exist


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
    with urllib.request.urlopen(req) as r:
        raw = r.read()
        return r.status, (json.loads(raw) if raw else {})


def wait_health(url, deadline=90):
    end = time.time() + deadline
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, timeout=2) as r:
                if r.status == 200:
                    return
        except OSError:
            pass
        time.sleep(1)
    raise RuntimeError(f"health never came up: {url}")


def main():
    wait_health(f"{FABRIC}/health")
    print("fabric up", flush=True)

    _, tok = http("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials",
        "client_id": "cccccccc-0000-0000-0000-000000000002",
        "client_secret": "daemon-app-secret",
        "scope": "https://api.fabric.microsoft.com/.default",
    }, form=True)
    token = tok["access_token"]

    _, ws = http("POST", f"{FABRIC}/v1/workspaces", {"displayName": "spark-ws"}, token=token)
    _, lake = http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, token=token)
    base = f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses/{lake['id']}/livyapi/versions/2023-12-01"
    print(f"workspace + lakehouse ready: {ws['id']}", flush=True)

    # Create an interactive session; retry until the agent's SparkSession is up.
    sid = None
    for _ in range(90):
        try:
            code, sess = http("POST", f"{base}/sessions", {"kind": "pyspark"}, token=token)
            sid = sess["id"]
            break
        except urllib.error.HTTPError as e:
            if e.code == 502:  # agent (Spark) not ready yet
                time.sleep(2)
                continue
            raise
    if sid is None:
        raise RuntimeError("session never started (Spark agent unreachable)")
    print(f"livy session {sid} created (state={sess['state']})", flush=True)

    def run(code_str):
        _, st = http("POST", f"{base}/sessions/{sid}/statements", {"code": code_str}, token=token)
        stid = st["id"]
        for _ in range(120):
            _, got = http("GET", f"{base}/sessions/{sid}/statements/{stid}", token=token)
            if got["state"] == "available":
                out = got["output"]
                if out.get("status") != "ok":
                    raise RuntimeError(f"statement error: {out}")
                return out["data"]["text/plain"].strip()
            time.sleep(1)
        raise RuntimeError("statement never became available")

    # A real Spark computation.
    r1 = run("spark.range(5).count()")
    print(f"spark.range(5).count() -> {r1}", flush=True)
    assert r1 == "5", r1

    # Persistence: a variable set in one statement is visible in the next —
    # proving the session is a genuine long-lived REPL, not one-shot submits.
    run("df = spark.createDataFrame([(1,'a'),(2,'b'),(3,'c')], ['id','name'])")
    r2 = run("df.filter(df.id >= 2).count()")
    print(f"df.filter(id>=2).count() -> {r2}", flush=True)
    assert r2 == "2", r2

    # An aggregation returning a driver value.
    r3 = run("spark.range(1, 101).groupBy().sum('id').collect()[0][0]")
    print(f"sum(1..100) -> {r3}", flush=True)
    assert r3 == "5050", r3

    http("DELETE", f"{base}/sessions/{sid}", token=token)

    # --- High-concurrency session: a REPL slot on real Spark (the 5-REPL model).
    _, hc = http("POST", f"{base}/highConcurrencySessions", {"sessionTag": "etl"}, token=token)
    hcid, replid, hcsid = hc["id"], hc["replId"], hc["sessionId"]

    def hc_run(code_str):
        _, st = http("POST", f"{base}/highConcurrencySessions/{hcsid}/repls/{replid}/statements", {"code": code_str}, token=token)
        stid = st["id"]
        for _ in range(120):
            _, got = http("GET", f"{base}/highConcurrencySessions/{hcsid}/repls/{replid}/statements/{stid}", token=token)
            if got["state"] == "available":
                return got["output"]["data"]["text/plain"].strip()
            time.sleep(1)
        raise RuntimeError("hc statement never available")

    r_hc = hc_run("spark.range(7).count()")
    print(f"[HC] spark.range(7).count() -> {r_hc}", flush=True)
    assert r_hc == "7", r_hc
    http("DELETE", f"{base}/highConcurrencySessions/{hcid}", token=token)

    # --- Batch: run a script fetched from OneLake on real Spark.
    try:
        http("POST", f"{ENTRA}/admin/api/apps",
             {"displayName": "storage", "appIdUri": "https://storage.azure.com", "isConfidential": False})
    except urllib.error.HTTPError as e:
        if e.code != 409:
            raise
    _, stok = http("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials",
        "client_id": "cccccccc-0000-0000-0000-000000000002",
        "client_secret": "daemon-app-secret",
        "scope": "https://storage.azure.com/.default",
    }, form=True)
    storage_token = stok["access_token"]

    # Upload the batch script into the lakehouse's Files (Blob surface, Host-routed).
    script = b"rows = spark.range(1000).filter('id % 2 = 0').count()\nprint('even rows:', rows)\n"
    put = urllib.request.Request(
        f"{FABRIC}/{ws['id']}/{lake['id']}/Files/batch.py", data=script, method="PUT",
        headers={"Host": "onelake.blob.fabric.microsoft.com", "Authorization": "Bearer " + storage_token,
                 "x-ms-blob-type": "BlockBlob"})
    with urllib.request.urlopen(put) as r:
        assert r.status in (200, 201), r.status

    _, b = http("POST", f"{base}/batches", {"file": f"{ws['id']}/{lake['id']}/Files/batch.py"}, token=token)
    bg = None
    for _ in range(120):
        _, bg = http("GET", f"{base}/batches/{b['id']}", token=token)
        if bg["state"] in ("success", "dead"):
            break
        time.sleep(1)
    print(f"[BATCH] state={bg['state']} log={bg.get('log')}", flush=True)
    assert bg["state"] == "success", bg
    assert any("even rows: 500" in line for line in bg.get("log", [])), bg["log"]
    http("DELETE", f"{base}/batches/{b['id']}", token=token)

    print("NATIVE-LIVY E2E: PASS", flush=True)


if __name__ == "__main__":
    main()
