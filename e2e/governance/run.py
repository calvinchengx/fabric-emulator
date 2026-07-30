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


def pip_install(*pkgs):
    subprocess.run([sys.executable, "-m", "pip", "install", "-q", *pkgs], check=True)


def main():
    pip_install("requests", "deltalake", "pyarrow")
    import requests
    import urllib3
    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

    log("starting family + OM backing stores")
    # Two phases: `up --wait` chokes on om-migrate (a one-shot that exits 0
    # is counted as failed on some compose versions), so wait only on the
    # long-running services, then start the OM chain and poll its API.
    compose("up", "-d", "--build", "--wait", "--wait-timeout", "600",
            "entra-emulator", "keyvault-emulator", "fabric-emulator",
            "om-postgresql", "om-elasticsearch")

    log("starting OpenMetadata (first-boot migration takes a few minutes)")
    compose("up", "-d", "--no-recreate", "openmetadata")

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
    ws = s.post(f"{fabric}/v1/workspaces", json={"displayName": "govws"}, timeout=15).json()
    r = s.post(f"{fabric}/v1/workspaces/{ws['id']}/lakehouses",
               json={"displayName": "lake"}, timeout=15)
    assert r.status_code in (201, 202), (r.status_code, r.text)

    import pyarrow as pa
    from deltalake import write_deltalake
    tbl = pa.table({
        "order_id": pa.array([1, 2, 3], pa.int64()),
        "amount": pa.array([9.5, 3.25, 7.0], pa.float64()),
        "region": ["us", "eu", "apac"],
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
    got = {c["name"]: c["dataType"] for c in t.json()["columns"]}
    want = {"order_id": "BIGINT", "amount": "DOUBLE", "region": "STRING"}
    assert got == want, f"columns mapped wrong: {got}"

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

    # Idempotency: a second ingest must succeed and not duplicate.
    log("re-running govern-ingest (idempotency)")
    compose("run", "--rm", "--no-deps", "govern-ingest")
    t2 = requests.get(f"{om}/api/v1/tables/name/fabric-emulator.govws.lake.orders",
                      headers=h, timeout=30)
    assert t2.status_code == 200

    log("PASS: fabric-emulator.govws.lake.orders cataloged with "
        f"{len(want)} columns, types {sorted(set(want.values()))}; "
        "shortcut cataloged with the target's schema and a lineage edge")


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
