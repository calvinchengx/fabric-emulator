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
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
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
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
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

    # --- DML row-count envelope, through the sql statement kind.
    #
    # DataFusion reports INSERT's row count as uint64, which the Arrow
    # conversion to the Connect client rejects. The agent used to absorb that
    # failure as an EMPTY result, so a client could not tell "wrote 3 rows"
    # from "wrote nothing" — parity.md carried it as 🟡 for exactly that. The
    # agent now recovers the count from the statement's cached result relation.
    # The count-back SELECT is the half that matters: recovery must not re-run
    # the INSERT, so 3 in the envelope AND 3 in the table is the whole claim.
    def run_sql_stmt(code_str):
        _, st = http("POST", f"{base}/sessions/{sid}/statements",
                     {"code": code_str, "kind": "sql"}, token=token)
        stid = st["id"]
        for _ in range(120):
            _, got = http("GET", f"{base}/sessions/{sid}/statements/{stid}", token=token)
            if got["state"] == "available":
                out = got["output"]
                if out.get("status") != "ok":
                    raise RuntimeError(f"sql statement error: {out}")
                return out["data"].get("application/json", {})
            time.sleep(1)
        raise RuntimeError("sql statement never became available")

    run_sql_stmt("CREATE TABLE dml_counts (id INT)")
    ins = run_sql_stmt("INSERT INTO dml_counts VALUES (1), (2), (3)")
    assert ins.get("data") == [[3]], f"INSERT envelope should carry its count: {ins}"
    back = run_sql_stmt("SELECT COUNT(*) AS n FROM dml_counts")
    assert back.get("data") == [[3]], f"count-back disagrees — re-execution or loss: {back}"
    print("INSERT envelope -> [[3]], table still 3 (count recovered, not re-run)", flush=True)

    # --- Spark Job Definition, EXECUTED BY THE EMULATOR.
    #
    # This suite is where the claim can be witnessed at all: the emulator here
    # has FABRIC_SPARK_AGENT_URL, so it IS the Spark pool, exactly as Fabric's
    # is. (e2e/notebook-run deliberately runs with NO agent — its runner plays
    # the external engine to witness the callback contract, so an
    # emulator-executes assertion there can never pass.)
    #
    # Nothing below executes the job. It publishes a definition, submits the
    # job, and polls: `sjd-result=5` can only be produced by the emulator
    # handing the submitted file to the engine with the definition's arguments
    # as argv — 3 rows from the lakehouse table plus the increment of 2.
    import base64

    def part(path, body):
        return {"path": path, "payloadType": "InlineBase64",
                "payload": base64.b64encode(body.encode()).decode()}

    sjd_cfg = {"executableFile": "main.py", "arguments": ["--increment", "2"],
               "defaultLakehouseArtifactId": lake["id"],
               "defaultLakehouseWorkspaceId": ws["id"]}
    # spark.range(3) is engine work in the SJD's own session; argv[2] is the
    # definition's argument the emulator must supply. 3 + 2 = 5 fails if either
    # half is missing, and needs no table shared across sessions.
    src = ("import sys\n"
           "print('sjd-result=' + str(spark.range(3).count() + int(sys.argv[2])))\n")
    _, created = http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items", {
        "displayName": "aggregate-job", "type": "SparkJobDefinition",
        "definition": {"parts": [part("SparkJobDefinitionV1.json", json.dumps(sjd_cfg)),
                                 part("main.py", src)]}}, token=token)
    # Item create is a Fabric LRO: 202 with the operation id in the
    # x-ms-operation-id HEADER, which this suite's http() does not surface (it
    # returns status + body only, and every other call here needs nothing more).
    # Rather than widen a helper the whole file depends on, resolve the item by
    # listing — the same fact, one call later, with no shared-code change.
    sjd_id = created.get("id")
    for _ in range(60):
        if sjd_id:
            break
        _, items = http("GET", f"{FABRIC}/v1/workspaces/{ws['id']}/items", token=token)
        for it in items.get("value", []):
            if it.get("displayName") == "aggregate-job" and it.get("type") == "SparkJobDefinition":
                sjd_id = it["id"]
                break
        time.sleep(1)
    assert sjd_id, "Spark job definition item was never created"

    http("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items/{sjd_id}/jobs/instances?jobType=sparkjob",
         token=token)
    _, jobs = http("GET", f"{FABRIC}/v1/workspaces/{ws['id']}/items/{sjd_id}/jobs/instances", token=token)
    sjd_jid = jobs["value"][0]["id"]

    final = {}
    for _ in range(60):
        _, final = http("GET",
                        f"{FABRIC}/v1/workspaces/{ws['id']}/items/{sjd_id}/jobs/instances/{sjd_jid}",
                        token=token)
        if final.get("status") in ("Completed", "Failed"):
            break
        time.sleep(2)
    _, sjd_done = http("GET",
                       f"{FABRIC}/v1/workspaces/{ws['id']}/items/{sjd_id}/jobs/instances/{sjd_jid}/sparkJobRun",
                       token=token)
    assert final.get("status") == "Completed", (
        f"emulator did not run the Spark job: {final} / engine said: "
        f"{sjd_done.get('error') or sjd_done.get('output')!r}")
    assert "sjd-result=5" in sjd_done.get("output", ""), sjd_done
    print(f"SJD executed by the emulator: {sjd_done['output'].strip()}", flush=True)

    # --- Delta maintenance on a OneLake table, through the delta-rs path.
    #
    # Sail's planner has no OPTIMIZE/VACUUM and rejects Change Data Feed reads,
    # so the agent routes them to delta-rs (python/spark_agent/delta_ops.py). The point of
    # running it *here* is the URL: an abfss:// OneLake table, which delta-rs can
    # only open with a Storage bearer of its own. A local path would prove the
    # interception fires and nothing about the credentials.
    onelake = "abfss://spark-ws@onelake.dfs.fabric.microsoft.com/lake.Lakehouse/Tables"
    maint = f"{onelake}/maint"
    # Two appends, so there is something to compact. Written by *Sail*, so the
    # table delta-rs later opens is one the engine produced.
    run(f"spark.sql('SELECT 1 AS id').write.format('delta').mode('overwrite').save('{maint}')")
    run(f"spark.sql('SELECT 2 AS id').write.format('delta').mode('append').save('{maint}')")

    run("import delta_ops, storage")

    # Negative control: the same operation with a *wrong* bearer must be
    # refused. Without it, a pass below would not distinguish "the token
    # worked" from "OneLake lets anyone in" — which is the entire claim.
    #
    # A wrong token rather than no options at all: with empty storage_options
    # delta-rs never reaches OneLake, it falls back to the Azure IMDS endpoint
    # (169.254.169.254) and dies on connection-refused. That failure says
    # nothing about whether the emulator checks bearers, so a control built on
    # it would pass vacuously.
    run("bad = dict(storage.options(), azure_storage_token='not-a-valid-token')")
    try:
        run(f"delta_ops.execute('optimize', {{'target': 'delta.`{maint}`', 'rest': ''}}, None, bad)")
        raise AssertionError("OPTIMIZE with an invalid bearer succeeded — OneLake "
                             "is not enforcing the Storage token, so the "
                             "credentialed runs below prove nothing")
    except RuntimeError as e:
        # It must fail *on authentication*: any other error would let a typo'd
        # URL or a missing table masquerade as proof that the bearer mattered.
        refusal = str(e)
        if not any(m in refusal for m in ("401", "403", "Unauthenticated",
                                          "AuthenticationFailed",
                                          "NoAuthenticationInformation")):
            raise AssertionError(f"expected an auth refusal, got: {refusal[:500]}")
    print("[DELTA] OPTIMIZE with an invalid bearer refused (negative control)", flush=True)

    r_opt = run(f"spark.sql('OPTIMIZE delta.`{maint}`').collect()[0][0]")
    print(f"[DELTA] {r_opt}", flush=True)
    # 2 files in, 1 out: a real compaction of a real OneLake table.
    assert "compacted 2 file(s) into 1" in r_opt and "delta-rs" in r_opt, r_opt

    r_vac = run(f"spark.sql('VACUUM delta.`{maint}` RETAIN 0 HOURS DRY RUN').collect()[0][0]")
    print(f"[DELTA] {r_vac}", flush=True)
    assert "would delete 2 file(s)" in r_vac, r_vac

    # Change Data Feed needs a CDF-enabled table, and Sail cannot create one:
    # its writer answers "Unsupported table features required: [ChangeDataFeed]".
    # So delta-rs writes this one — the same delta-rs the CDF helper reads with,
    # through the same credentials. That is the honest scope of the claim: the
    # emulator can serve a change feed on OneLake, not that Sail can enable one.
    cdf = f"{onelake}/cdf"
    run("from deltalake import write_deltalake; import pyarrow as pa")
    run(f"write_deltalake('{cdf}', pa.table({{'id': [1]}}), mode='overwrite',"
        f" configuration={{'delta.enableChangeDataFeed': 'true'}},"
        f" storage_options=storage.options())")
    run(f"write_deltalake('{cdf}', pa.table({{'id': [2]}}), mode='append',"
        f" storage_options=storage.options())")

    r_cdf = run(f"spark.delta_change_feed('{cdf}').count()")
    print(f"[DELTA] change feed rows -> {r_cdf}", flush=True)
    assert r_cdf == "2", r_cdf

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
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
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
