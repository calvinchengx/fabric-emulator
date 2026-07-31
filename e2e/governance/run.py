#!/usr/bin/env python3
"""e2e: the optional governance profile, witnessed end to end.

Brings up the family + OpenMetadata (profile `governance`, NO compute
sidecars — this witness is about the catalog), seeds a workspace/lakehouse
and a REAL Delta table via delta-rs, runs the `govern-ingest` one-shot, and
asserts through OpenMetadata's API that the table arrived with its columns
mapped from the Delta schema.

Would have caught: the OM server image's MySQL driver/port defaults leaking
past our Postgres pins, and schema drift in scripts/govern_ingest.py.
"""
import base64
import json
import os
from decimal import Decimal
import subprocess
import sys
import time
import urllib.request

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))

FABRIC_PORT = os.environ.get("GOV_FABRIC_PORT", "9443")
ENTRA_PORT = os.environ.get("GOV_ENTRA_PORT", "8443")
OM_PORT = os.environ.get("GOV_OM_PORT", "8585")
TENANT = "11111111-1111-1111-1111-111111111111"

COMPOSE = ["docker", "compose", "-p", "fabricgov-e2e",
           "-f", os.path.join(REPO, "docker-compose.yml"),
           "-f", os.path.join(DIR, "build-override.yml"),
           "--profile", "governance"]


def log(msg):
    print(f"==> {msg}", flush=True)


def compose(*args, check=True):
    return subprocess.run(COMPOSE + list(args), check=check,
                          env={**os.environ, "GOV_BUILD_CONTEXT": REPO})


def compose_pulling(*args, attempts=3, base_delay=15):
    """Run a compose command whose failure mode is usually a registry hiccup.

    OpenMetadata ships from docker.getcollate.io — a third-party registry with
    no mirror — so a reset connection mid-pull reds an otherwise-green run.
    (Seen in CI: `read tcp ...:443: read: connection reset by peer`.)

    Retries are bounded and each one is logged, so a real outage still fails
    the suite rather than being silently absorbed or hanging.
    """
    for attempt in range(1, attempts + 1):
        result = compose(*args, check=False)
        if result.returncode == 0:
            return result
        if attempt == attempts:
            log(f"compose {args[0]} failed {attempts}x — giving up")
            raise subprocess.CalledProcessError(result.returncode, COMPOSE + list(args))
        delay = base_delay * attempt
        log(f"compose {args[0]} exited {result.returncode}; "
            f"retrying in {delay}s ({attempt}/{attempts - 1}) — likely a registry hiccup")
        time.sleep(delay)


def main():
    import requests
    import urllib3
    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

    log("starting family + OM backing stores")
    # Two phases: `up --wait` chokes on om-migrate (a one-shot that exits 0
    # is counted as failed on some compose versions), so wait only on the
    # long-running services, then start the OM chain and poll its API.
    compose_pulling("up", "-d", "--build", "--wait", "--wait-timeout", "600",
                    "entra-emulator", "keyvault-emulator", "fabric-emulator",
                    "om-postgresql", "om-elasticsearch")

    log("starting OpenMetadata (first-boot migration takes a few minutes)")
    compose_pulling("up", "-d", "--no-recreate", "openmetadata")

    entra = f"https://localhost:{ENTRA_PORT}"
    fabric = f"https://localhost:{FABRIC_PORT}"
    om = f"http://localhost:{OM_PORT}"

    import requests
    end = time.time() + 900
    while time.time() < end:
        try:
            if requests.get(f"{om}/api/v1/system/version", timeout=3).status_code == 200:
                break
        except requests.RequestException:
            pass
        time.sleep(5)
    else:
        raise RuntimeError("OpenMetadata never became healthy")

    def token(scope):
        r = requests.post(
            f"{entra}/{TENANT}/oauth2/v2.0/token",
            data={"grant_type": "client_credentials",
                  "client_id": "cccccccc-0000-0000-0000-000000000002",
                  "client_secret": "daemon-app-secret", "scope": scope},
            verify=False, timeout=15)
        r.raise_for_status()
        return r.json()["access_token"]

    log("seeding workspace + lakehouse + real Delta table")
    s = requests.Session()
    s.verify = False
    s.headers["Authorization"] = "Bearer " + token("https://api.fabric.microsoft.com/.default")
    r = s.post(f"{fabric}/v1/workspaces", json={"displayName": "govws"}, timeout=15)
    assert r.status_code == 201, (r.status_code, r.text)
    ws = r.json()
    r = s.post(f"{fabric}/v1/workspaces/{ws['id']}/lakehouses",
               json={"displayName": "lake"}, timeout=15)
    assert r.status_code in (201, 202), (r.status_code, r.text)

    import pyarrow as pa
    from deltalake import write_deltalake
    # Deliberately includes a decimal and nested types: a catalog that
    # flattens decimal(10,2) to STRING loses precision users care about, so
    # OM's own validation is the oracle for the mapping.
    tbl = pa.table({
        "order_id": pa.array([1, 2, 3], pa.int64()),
        "amount": pa.array([9.5, 3.25, 7.0], pa.float64()),
        "region": ["us", "eu", "apac"],
        "price": pa.array([Decimal("1.50"), Decimal("2.25"), Decimal("3.00")],
                          pa.decimal128(10, 2)),
        "tags": pa.array([["a"], ["b"], ["c"]], pa.list_(pa.string())),
        "meta": pa.array([{"src": "web"}, {"src": "app"}, {"src": "web"}],
                         pa.struct([("src", pa.string())])),
    })
    write_deltalake("az://govws/lake.Lakehouse/Tables/orders", tbl, storage_options={
        "azure_storage_account_name": "onelake",
        "azure_storage_token": token("https://storage.azure.com/.default"),
        "azure_endpoint": f"{fabric}/onelake",
        "allow_invalid_certificates": "true",
    })
    log("delta table written via delta-rs")

    # A second lakehouse holding a OneLake shortcut to the first one's table:
    # a literal data-flow edge, and the only lineage the emulator can know
    # exactly (see scripts/govern_ingest.py).
    log("seeding a shortcut (the lineage edge under test)")
    r = s.post(f"{fabric}/v1/workspaces/{ws['id']}/lakehouses",
               json={"displayName": "curated"}, timeout=15)
    assert r.status_code in (201, 202), (r.status_code, r.text)
    items = s.get(f"{fabric}/v1/workspaces/{ws['id']}/items",
                  params={"type": "Lakehouse"}, timeout=15).json()["value"]
    by_name = {i["displayName"]: i["id"] for i in items}
    r = s.post(f"{fabric}/v1/workspaces/{ws['id']}/items/{by_name['curated']}/shortcuts",
               json={"path": "Tables", "name": "orders_ref",
                     "target": {"oneLake": {"workspaceId": ws["id"],
                                            "itemId": by_name["lake"],
                                            "path": "Tables/orders"}}}, timeout=15)
    assert r.status_code in (200, 201), (r.status_code, r.text)

    # Execute a real Copy over the Delta directory. This creates a second table
    # and an activity-produced lineage edge, independently of shortcut lineage.
    log("executing Copy activity (the activity lineage edge under test)")
    pipeline = {"properties": {"activities": [{
        "name": "CurateOrders", "type": "Copy", "typeProperties": {
            "source": {"location": {"itemId": by_name["lake"],
                                      "path": "Tables/orders"}},
            "sink": {"location": {"itemId": by_name["curated"],
                                    "path": "Tables/orders_copy"}},
        }}]}}
    payload = base64.b64encode(json.dumps(pipeline).encode()).decode()
    r = s.post(f"{fabric}/v1/workspaces/{ws['id']}/items", json={
        "displayName": "curate-orders", "type": "DataPipeline",
        "definition": {"parts": [{"path": "pipeline-content.json",
                                    "payloadType": "InlineBase64",
                                    "payload": payload}]},
    }, timeout=15)
    assert r.status_code == 202, (r.status_code, r.text)
    opid = r.headers["x-ms-operation-id"]
    for _ in range(60):
        op = s.get(f"{fabric}/v1/operations/{opid}", timeout=15).json()
        if op.get("status") == "Succeeded":
            pipeline_id = s.get(f"{fabric}/v1/operations/{opid}/result", timeout=15).json()["id"]
            break
        time.sleep(.1)
    else:
        raise RuntimeError("pipeline create did not complete")
    r = s.post(f"{fabric}/v1/workspaces/{ws['id']}/items/{pipeline_id}/jobs/instances",
               params={"jobType": "Pipeline"}, json={}, timeout=30)
    assert r.status_code == 202, (r.status_code, r.text)
    copy_job = s.get(r.headers["Location"], timeout=15).json()
    assert copy_job["status"] == "Completed", copy_job

    log("running govern-ingest")
    # --no-deps: everything is already up; letting compose re-evaluate the
    # dependency chain here re-runs one-shots and can recreate fabric,
    # wiping its in-memory state between seed and ingest (seen on CI).
    compose("run", "--rm", "--no-deps", "govern-ingest")

    log("asserting through OpenMetadata's API")
    r = requests.post(f"{om}/api/v1/users/login",
                      json={"email": "admin@open-metadata.org",
                            "password": base64.b64encode(b"admin").decode()}, timeout=30)
    r.raise_for_status()
    h = {"Authorization": "Bearer " + r.json()["accessToken"]}

    t = requests.get(f"{om}/api/v1/tables/name/fabric-emulator.govws.lake.orders",
                     headers=h, timeout=30)
    assert t.status_code == 200, (t.status_code, t.text[:300])
    cols = {c["name"]: c for c in t.json()["columns"]}
    got = {n: c["dataType"] for n, c in cols.items()}
    want = {"order_id": "BIGINT", "amount": "DOUBLE", "region": "STRING",
            "price": "DECIMAL", "tags": "ARRAY", "meta": "STRUCT"}
    assert got == want, f"columns mapped wrong: {got}"
    # Decimal precision/scale must survive into the catalog.
    assert cols["price"].get("precision") == 10 and cols["price"].get("scale") == 2, \
        f"decimal precision lost: {cols['price']}"
    assert cols["tags"].get("arrayDataType") == "STRING", \
        f"array element type lost: {cols['tags']}"
    assert [c["name"] for c in cols["meta"].get("children", [])] == ["src"], \
        f"struct children lost: {cols['meta']}"

    svc = requests.get(f"{om}/api/v1/services/databaseServices/name/fabric-emulator",
                       headers=h, timeout=30)
    assert svc.status_code == 200 and svc.json()["serviceType"] == "CustomDatabase"

    # The shortcut is cataloged as a table carrying the TARGET's schema —
    # that is the data it exposes.
    sc = requests.get(f"{om}/api/v1/tables/name/fabric-emulator.govws.curated.orders_ref",
                      headers=h, timeout=30)
    assert sc.status_code == 200, (sc.status_code, sc.text[:300])
    sc_cols = {c["name"]: c["dataType"] for c in sc.json()["columns"]}
    assert sc_cols == want, f"shortcut columns wrong: {sc_cols}"

    copied = requests.get(
        f"{om}/api/v1/tables/name/fabric-emulator.govws.curated.orders_copy",
        headers=h, timeout=30)
    assert copied.status_code == 200, (copied.status_code, copied.text[:300])

    # …and OM holds the lineage edge orders -> orders_ref.
    lin = requests.get(
        f"{om}/api/v1/lineage/table/name/fabric-emulator.govws.lake.orders",
        params={"upstreamDepth": 1, "downstreamDepth": 1}, headers=h, timeout=30)
    assert lin.status_code == 200, (lin.status_code, lin.text[:300])
    graph = lin.json()
    ids = {n.get("id") for n in graph.get("nodes", [])}
    ids.add((graph.get("entity") or {}).get("id"))
    assert sc.json()["id"] in ids, f"shortcut not in lineage graph: {graph}"
    edges = graph.get("downstreamEdges") or graph.get("edges") or []
    assert any(e.get("toEntity") == sc.json()["id"] for e in edges), \
        f"no downstream edge to the shortcut: {edges}"
    log("lineage edge orders -> curated.orders_ref present in OpenMetadata")

    activity_lin = requests.get(
        f"{om}/api/v1/lineage/table/name/fabric-emulator.govws.curated.orders_copy",
        params={"upstreamDepth": 1, "downstreamDepth": 1}, headers=h, timeout=30)
    assert activity_lin.status_code == 200, (activity_lin.status_code, activity_lin.text[:300])
    activity_graph = activity_lin.json()
    activity_ids = {n.get("id") for n in activity_graph.get("nodes", [])}
    activity_ids.add((activity_graph.get("entity") or {}).get("id"))
    assert t.json()["id"] in activity_ids, f"Copy source absent from lineage: {activity_graph}"
    log("activity lineage lake.orders -> curated.orders_copy present in OpenMetadata")

    # Idempotency: a second ingest must succeed and not duplicate.
    log("re-running govern-ingest (idempotency)")
    compose("run", "--rm", "--no-deps", "govern-ingest")
    t2 = requests.get(f"{om}/api/v1/tables/name/fabric-emulator.govws.lake.orders",
                      headers=h, timeout=30)
    assert t2.status_code == 200

    log("PASS: fabric-emulator.govws.lake.orders cataloged with "
        f"{len(want)} columns, types {sorted(set(want.values()))}; "
        "shortcut and Copy activity lineage cataloged")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        for svc in ("om-migrate", "openmetadata", "govern-ingest", "fabric-emulator"):
            sys.stderr.write(f"\n==== {svc} log tail ====\n")
            compose("logs", "--tail", "40", svc, check=False)
        raise
    finally:
        compose("down", "-v", "--remove-orphans", check=False)
