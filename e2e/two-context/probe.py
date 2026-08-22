#!/usr/bin/env python3
"""Can a notebook cell reach the credential the agent reads OneLake with?

WHY ASK. Stage A made OneLake refuse a direct path read from a principal whose
grant narrows the table. It does not reach a notebook, because our Spark agent
holds ONE service credential and uses it for every caller: the read arrives as a
Contributor and is correctly allowed. Real Fabric's user context carries the
user's own identity, which is what makes the platform block apply there.

That is an assumption about our agent, read off the source (`exec()` runs user
code in the agent process, which also calls `storage.token()`). Assumptions read
off source are what this session has been wrong about twice, so it is measured
before anything is built on it. Three questions:

  1. can a cell obtain the agent's storage bearer?
  2. can a VIEWER, narrowed by a role, read the table's files by path anyway?
  3. does the same viewer's own identity get refused by OneLake (stage A), so
     the gap is the IDENTITY and not the rule?

A "yes, yes, yes" says the two-context split is the fix and says why. Any other
answer means the design is aimed at the wrong thing.

Runs against the e2e/livy stack, reusing its helpers rather than restating them:
  docker compose -f ../livy/docker-compose.yml run --rm \
      -v "$PWD/probe.py:/probe.py" client python3 /probe.py

EXIT 0 WHATEVER IT FINDS. A spike reports; it does not gate.
"""
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, "/")
import client as c  # noqa: E402 - the e2e's helpers; see the docstring

VIEWER = "two-context-viewer"


def statements(base, sid, token):
    def run(code):
        _, st = c.http("POST", f"{base}/sessions/{sid}/statements",
                       {"code": code}, token=token)
        for _ in range(120):
            _, got = c.http("GET", f"{base}/sessions/{sid}/statements/{st['id']}",
                            token=token)
            if got["state"] == "available":
                out = got["output"]
                if out.get("status") != "ok":
                    return "ERROR: " + str(out.get("evalue"))[:160]
                return out["data"]["text/plain"].strip()
            time.sleep(1)
        return "TIMEOUT"
    return run


def open_session(base, token):
    for _ in range(90):
        try:
            _, sess = c.http("POST", f"{base}/sessions", {"kind": "pyspark"}, token=token)
            return sess["id"]
        except urllib.error.HTTPError as e:
            if e.code == 502:  # the agent's Spark is not up yet
                time.sleep(2)
                continue
            raise
    raise RuntimeError("no livy session")


def main():
    c.wait_health(f"{c.FABRIC}/health")
    _, tok = c.http("POST", f"{c.ENTRA}/{c.TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials",
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
        "client_secret": "daemon-app-secret",
        "scope": "https://api.fabric.microsoft.com/.default",
    }, form=True)
    token = tok["access_token"]

    _, ws = c.http("POST", f"{c.FABRIC}/v1/workspaces",
                   {"displayName": "two-context-ws"}, token=token)
    _, lake = c.http("POST", f"{c.FABRIC}/v1/workspaces/{ws['id']}/lakehouses",
                     {"displayName": "lake"}, token=token)
    base = (f"{c.FABRIC}/v1/workspaces/{ws['id']}/lakehouses/{lake['id']}"
            "/livyapi/versions/2023-12-01")
    c.http("POST", f"{c.FABRIC}/v1/workspaces/{ws['id']}/roleAssignments",
           {"principal": {"id": VIEWER, "type": "User"}, "role": "Viewer"}, token=token)

    path = (f"abfss://{ws['id']}@onelake.dfs.fabric.microsoft.com"
            f"/{lake['id']}/Tables/sales")
    orun = statements(base, open_session(base, token), token)
    orun("df = spark.createDataFrame([(1, 10), (1, 20), (2, 30)], "
         "['region_id', 'amount'])")
    orun(f"df.write.format('delta').mode('overwrite').save('{path}')")
    orun(f"spark.sql(\"CREATE TABLE IF NOT EXISTS sales USING delta LOCATION '{path}'\")")
    print(f"owner rows: {orun('spark.sql(\"SELECT count(*) FROM sales\").collect()[0][0]')}",
          flush=True)

    # A role that narrows the viewer to one region and one column.
    c.http("PUT", f"{c.FABRIC}/v1/workspaces/{ws['id']}/items/{lake['id']}/dataAccessRoles",
           {"value": [{
               "name": "region1_only",
               "decisionRules": [{
                   "effect": "Permit",
                   "rows": "SELECT * FROM sales WHERE region_id = 1",
                   "columns": ["region_id"],
                   "permission": [
                       {"attributeName": "Path",
                        "attributeValueIncludedIn": ["Tables/sales"]},
                       {"attributeName": "Action",
                        "attributeValueIncludedIn": ["Read"]}]}],
               "members": {"microsoftEntraMembers": [{"objectId": VIEWER}]}}]},
           token=token)

    vtok = c.forge_token(VIEWER)
    vrun = statements(base, open_session(base, vtok), vtok)

    print("\nQ0 the control: is the viewer filtered on the SQL path?", flush=True)
    print(f"    SELECT count(*) FROM sales -> {vrun('spark.sql(\"SELECT count(*) FROM sales\").collect()[0][0]')}",
          flush=True)

    print("\nQ1 can a cell reach the agent's storage credential?", flush=True)
    for code in ("__import__('storage').token()[:12]",
                 "list(__import__('storage').options())",
                 "[k for k in __import__('os').environ if 'TOKEN' in k or 'SECRET' in k]"):
        print(f"    {code}\n        -> {vrun(code)}", flush=True)

    print("\nQ2 can the viewer read the files by PATH from a cell?", flush=True)
    print(f"    spark.read.format('delta').load(...).count() -> "
          f"{vrun(f'spark.read.format(chr(100)+chr(101)+chr(108)+chr(116)+chr(97)).load({path!r}).count()')}",
          flush=True)
    print(f"    ...columns -> {vrun(f'str(spark.read.format(chr(100)+chr(101)+chr(108)+chr(116)+chr(97)).load({path!r}).columns)')}",
          flush=True)

    print("\nQ3 and with the viewer's OWN identity, straight at OneLake?", flush=True)
    vstore = c.forge_token(VIEWER, audience="https://storage.azure.com")
    req = urllib.request.Request(
        f"{c.FABRIC}/{ws['id']}/{lake['id']}/Tables/sales/_delta_log/"
        "00000000000000000000.json", method="GET",
        headers={"Host": "onelake.dfs.fabric.microsoft.com",
                 "Authorization": "Bearer " + vstore})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            print(f"    direct OneLake GET -> {r.status}", flush=True)
    except urllib.error.HTTPError as e:
        print(f"    direct OneLake GET -> {e.code} {e.read()[:120]!r}", flush=True)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:  # noqa: BLE001 - a spike reports, it does not gate
        import traceback
        traceback.print_exc()
        sys.exit(0)
