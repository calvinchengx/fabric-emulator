#!/usr/bin/env python3
"""The two-context split, asserted: what a notebook cell can and cannot reach.

`probe.py` measured this on a single-process agent and found a cell holding the
agent's storage bearer, listing `ENTRA_CLIENT_SECRET` out of the environment,
and reading a narrowed table's files in full. This is that measurement turned
into a witness, run with `FABRIC_TWO_CONTEXT=1`.

EVERY CLAIM IS PAIRED. A stack where nothing works would satisfy "the cell
cannot reach the secret" and prove nothing, so each refusal here sits next to a
read that must still succeed: the viewer is filtered, not broken, and the owner
is untouched.

ONE ASSERTION IS A TRIPWIRE, NOT A GUARANTEE. The path read still returns every
row, because Sail holds its own `AZURE_STORAGE_TOKEN` and executes the read with
that identity whatever the calling process holds (docs/54). Asserting the
CURRENT number means the day someone closes that gap, this fails and says so
rather than leaving a stale 🟡 in the parity map.
"""
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, "/")
import client as c  # noqa: E402, I001 - the livy e2e's helpers, mounted alongside

VIEWER = "two-context-viewer"


def statements(base, sid, token):
    def run(code):
        _, st = c.http("POST", f"{base}/sessions/{sid}/statements",
                       {"code": code}, token=token)
        for _ in range(180):
            _, got = c.http("GET", f"{base}/sessions/{sid}/statements/{st['id']}",
                            token=token)
            if got["state"] == "available":
                out = got["output"]
                if out.get("status") != "ok":
                    raise RuntimeError(f"statement failed: {str(out.get('evalue'))[:300]}")
                return out["data"]["text/plain"].strip()
            time.sleep(1)
        raise RuntimeError("statement never became available")
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
    _, stok = c.http("POST", f"{c.ENTRA}/{c.TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials",
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
        "client_secret": "daemon-app-secret",
        "scope": "https://storage.azure.com/.default",
    }, form=True)
    service_bearer = stok["access_token"]

    _, ws = c.http("POST", f"{c.FABRIC}/v1/workspaces",
                   {"displayName": "two-context-ws"}, token=token)
    _, lake = c.http("POST", f"{c.FABRIC}/v1/workspaces/{ws['id']}/lakehouses",
                     {"displayName": "lake"}, token=token)
    base = (f"{c.FABRIC}/v1/workspaces/{ws['id']}/lakehouses/{lake['id']}"
            "/livyapi/versions/2023-12-01")
    c.http("POST", f"{c.FABRIC}/v1/workspaces/{ws['id']}/roleAssignments",
           {"principal": {"id": VIEWER, "type": "User"}, "role": "Viewer"}, token=token)
    print("workspace, lakehouse and viewer ready", flush=True)

    sales = (f"abfss://{ws['id']}@onelake.dfs.fabric.microsoft.com"
             f"/{lake['id']}/Tables/sales")
    secret = (f"abfss://{ws['id']}@onelake.dfs.fabric.microsoft.com"
              f"/{lake['id']}/Tables/secret")
    orun = statements(base, open_session(base, token), token)
    orun("df = spark.createDataFrame([(1, 10), (1, 20), (2, 30)], "
         "['region_id', 'amount'])")
    orun(f"df.write.format('delta').mode('overwrite').save('{sales}')")
    orun(f"spark.sql(\"CREATE TABLE IF NOT EXISTS sales USING delta LOCATION '{sales}'\")")
    orun("spark.createDataFrame([(9, 99)], ['region_id', 'amount'])"
         f".write.format('delta').mode('overwrite').save('{secret}')")
    orun(f"spark.sql(\"CREATE TABLE IF NOT EXISTS secret USING delta LOCATION '{secret}'\")")
    assert orun("spark.sql('SELECT count(*) FROM sales').collect()[0][0]") == "3"
    print("    owner: sales=3 rows, secret exists", flush=True)

    # A role that narrows the viewer to one region and one column of `sales`,
    # and says nothing at all about `secret`.
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

    # 1. The viewer is FILTERED, not broken. Everything below is only meaningful
    #    while this holds.
    rows = vrun("spark.sql('SELECT count(*) FROM sales').collect()[0][0]")
    assert rows == "2", f"viewer rows = {rows}, want 2 of 3"
    cols = vrun("','.join(spark.sql('SELECT * FROM sales').columns)").strip("'\"")
    assert cols == "region_id", f"viewer columns = {cols!r}"
    print(f"    viewer: {rows} of 3 rows, columns {cols}"
          " -- supplied by the system context", flush=True)

    # 2. A table no rule mentions is not merely unreadable, it is NOT THERE.
    #    Deny-by-default is free once the contexts are separate: nothing to drop
    #    and nothing to sweep, because it was never registered.
    listed = vrun("str(sorted(r[1] for r in spark.sql('SHOW TABLES').collect()))")
    assert "secret" not in listed, f"an ungranted table is nameable: {listed}"
    print(f"    viewer catalog: {listed}", flush=True)

    # 3. THE ESCALATION, closed. The cell holds no service credential and no
    #    means of minting one.
    env = vrun("str(sorted(k for k in __import__('os').environ "
               "if 'SECRET' in k or 'TOKEN' in k))")
    assert "ENTRA_CLIENT_SECRET" not in env, f"the cell can mint tokens: {env}"
    print(f"    viewer environment: {env}", flush=True)

    cell_bearer = vrun("__import__('storage').token()").strip("'\"")
    assert cell_bearer != service_bearer, "the cell holds the SERVICE bearer"
    assert cell_bearer, "the cell holds no bearer at all -- it cannot read anything"
    print("    the cell's bearer is the caller's own, not the agent's", flush=True)

    # 4. THE OWNER IS UNTOUCHED. A role narrows the principal it names.
    still = orun("spark.sql('SELECT count(*) FROM sales').collect()[0][0]")
    assert still == "3", f"the owner was narrowed too: {still}"

    # 5. THE PATH READ, refused. This is the whole point of the split, and it
    #    is refused by ONELAKE rather than by anything we patched: the cell's
    #    engine belongs to this caller and carries this caller's token, so the
    #    read arrives as a principal whose grant narrows the table, and the
    #    platform blocks it exactly as Fabric documents.
    #
    #    For most of this feature's life it returned all 3 rows, because one
    #    shared engine served every caller with a service credential. A
    #    per-user engine is not a mitigation bolted on top; it is the shape
    #    Fabric has, and the refusal falls out of it.
    def cell(code_str):
        code = ("try:\n"
                f"    _v = {code_str}\n"
                "    print('ok', _v)\n"
                "except Exception as _e:\n"
                "    print('blocked', type(_e).__name__, str(_e)[:120])\n")
        return vrun(code).strip()

    out = cell(f"spark.read.format('delta').load({sales!r}).count()")
    assert not out.startswith("ok"), f"the cell read the unfiltered table: {out}"
    assert "403" in out or "Forbidden" in out, (
        f"the path read failed, but not with OneLake's refusal: {out}")
    print(f"    path read from a cell: {out[:90]}", flush=True)

    # And the SQL path still works for the same caller in the same breath, so
    # the refusal above is scoped to raw access rather than to the table.
    again = vrun("spark.sql('SELECT count(*) FROM sales').collect()[0][0]")
    assert again == "2", f"the filtered read broke: {again}"

    print("TWO-CONTEXT E2E: PASS", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
