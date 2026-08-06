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
import sys
import time
import urllib.parse
import urllib.request
from decimal import Decimal

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))
sys.path.insert(0, DIR)

from stack import Stack, log  # noqa: E402 — after the path insert above

FABRIC_PORT = os.environ.get("GOV_FABRIC_PORT", "9443")
ENTRA_PORT = os.environ.get("GOV_ENTRA_PORT", "8443")
OM_PORT = os.environ.get("GOV_OM_PORT", "8585")
TENANT = "11111111-1111-1111-1111-111111111111"

stack = Stack("fabricgov-e2e", "build-override.yml")


def main():
    import requests
    import urllib3
    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

    log("starting family + OM backing stores")
    # Two phases: `up --wait` chokes on om-migrate (a one-shot that exits 0
    # is counted as failed on some compose versions), so wait only on the
    # long-running services, then start the OM chain and poll its API.
    stack.pulling("up", "-d", "--build", "--wait", "--wait-timeout", "600",
                  "entra-emulator", "keyvault-emulator", "fabric-emulator",
                  "om-postgresql", "om-opensearch")

    log("starting OpenMetadata (first-boot migration takes a few minutes)")
    stack.pulling("up", "-d", "--no-recreate", "openmetadata")

    entra = f"https://localhost:{ENTRA_PORT}"
    fabric = f"https://localhost:{FABRIC_PORT}"
    om = f"http://localhost:{OM_PORT}"

    stack.wait_for_om(om)

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

    # Sensitivity labels. Two different labels on two lakehouses, so a bug
    # that tags everything with one constant cannot pass. `curated` keeps the
    # *less* restrictive one deliberately.
    log("applying sensitivity labels via bulkSetLabels")
    labels = s.get(f"{fabric}/v1/admin/labels", timeout=15).json()["labels"]
    by_label = {label["name"]: label["id"] for label in labels}
    for name, item in (("Confidential", "lake"), ("Public", "curated")):
        r = s.post(f"{fabric}/v1/admin/items/bulkSetLabels",
                   json={"items": [{"id": by_name[item]}], "labelId": by_label[name]},
                   timeout=15)
        assert r.status_code == 200 and not r.json()["failedItems"], (r.status_code, r.text)

    log("running govern-ingest")
    # --no-deps: everything is already up; letting compose re-evaluate the
    # dependency chain here re-runs one-shots and can recreate fabric,
    # wiping its in-memory state between seed and ingest (seen on CI).
    stack.compose("run", "--rm", "--no-deps", "govern-ingest")

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

    # --- sensitivity labels, read back through OpenMetadata's own API ------
    #
    # This is the part a Purview -> OpenMetadata migration cannot do: labels
    # are Purview Information Protection objects, not Atlas entities. The
    # emulator's answer is not evidence here — only OM's is.
    def om_get(path, **params):
        return requests.get(f"{om}/api/v1/{path}", params=params, headers=h, timeout=30)

    def schema_label_tags(fqn):
        r = om_get(f"databaseSchemas/name/{fqn}", fields="tags")
        assert r.status_code == 200, (fqn, r.status_code, r.text[:300])
        return {t["tagFQN"] for t in (r.json().get("tags") or [])
                if t["tagFQN"].startswith("FabricSensitivity.")}

    c = om_get("classifications/name/FabricSensitivity")
    assert c.status_code == 200, (c.status_code, c.text[:300])
    # A Fabric item carries at most one label; the classification must say so.
    assert c.json()["mutuallyExclusive"] is True, c.json()

    # The whole taxonomy is exported, not only the labels in use — including
    # the one whose name contains a space, which is where an FQN goes wrong.
    for label in labels:
        tag = om_get(f"tags/name/{urllib.parse.quote('FabricSensitivity.' + label['name'])}")
        assert tag.status_code == 200, (label["name"], tag.status_code, tag.text[:300])
        assert tag.json()["fullyQualifiedName"] == f"FabricSensitivity.{label['name']}", tag.json()
        assert label["id"] in tag.json()["description"], tag.json()["description"]
    log(f"{len(labels)} label(s) present in OpenMetadata as FabricSensitivity tags")

    assert schema_label_tags("fabric-emulator.govws.lake") == {"FabricSensitivity.Confidential"}, \
        schema_label_tags("fabric-emulator.govws.lake")
    # Negative control: the other lakehouse carries a *different* label, so a
    # constant tag or a blanket "tag everything" fails right here.
    assert schema_label_tags("fabric-emulator.govws.curated") == {"FabricSensitivity.Public"}, \
        schema_label_tags("fabric-emulator.govws.curated")
    log("labels lake=Confidential, curated=Public confirmed through OM")

    # A tag applied by a human in the catalog, under someone else's
    # classification. Re-ingest must not delete it.
    def om_put_json(path, body):
        return requests.put(f"{om}/api/v1/{path}", json=body, headers=h, timeout=30)

    r = om_put_json("classifications", {"name": "E2EManual",
                                        "description": "Hand curation, not from the emulator."})
    assert r.status_code in (200, 201), (r.status_code, r.text[:300])
    r = om_put_json("tags", {"name": "Curated", "classification": "E2EManual",
                             "description": "Applied by a person in OpenMetadata."})
    assert r.status_code in (200, 201), (r.status_code, r.text[:300])
    lake_schema = om_get("databaseSchemas/name/fabric-emulator.govws.lake", fields="tags").json()
    r = requests.patch(
        f"{om}/api/v1/databaseSchemas/{lake_schema['id']}",
        data=json.dumps([{"op": "add", "path": "/tags/-",
                          "value": {"tagFQN": "E2EManual.Curated",
                                    "labelType": "Manual", "state": "Confirmed"}}]),
        headers={**h, "Content-Type": "application/json-patch+json"}, timeout=30)
    assert r.status_code == 200, (r.status_code, r.text[:300])

    # Idempotency: a second ingest must succeed and not duplicate.
    log("re-running govern-ingest (idempotency)")
    stack.compose("run", "--rm", "--no-deps", "govern-ingest")
    t2 = requests.get(f"{om}/api/v1/tables/name/fabric-emulator.govws.lake.orders",
                      headers=h, timeout=30)
    assert t2.status_code == 200
    assert schema_label_tags("fabric-emulator.govws.lake") == {"FabricSensitivity.Confidential"}, \
        "label tag lost or duplicated on re-ingest"
    all_tags = {t["tagFQN"] for t in om_get(
        "databaseSchemas/name/fabric-emulator.govws.lake", fields="tags").json()["tags"]}
    assert "E2EManual.Curated" in all_tags, f"re-ingest deleted a hand-applied tag: {all_tags}"

    # Negative control: clearing the label in Fabric must clear the tag in
    # OpenMetadata. Without this, "the tag is there" only proves tags are
    # written, never that they track the source of truth.
    log("removing the label and re-ingesting (negative control)")
    r = s.post(f"{fabric}/v1/admin/items/bulkRemoveLabels",
               json={"items": [{"id": by_name["lake"]}]}, timeout=15)
    assert r.status_code == 200 and not r.json()["failedItems"], (r.status_code, r.text)
    stack.compose("run", "--rm", "--no-deps", "govern-ingest")
    assert schema_label_tags("fabric-emulator.govws.lake") == set(), \
        f"label removed in Fabric but still tagged in OM: {schema_label_tags('fabric-emulator.govws.lake')}"
    # …and the removal was targeted: the other lakehouse and the hand-applied
    # tag are untouched.
    assert schema_label_tags("fabric-emulator.govws.curated") == {"FabricSensitivity.Public"}, \
        "removing one item's label cleared another's"
    all_tags = {t["tagFQN"] for t in om_get(
        "databaseSchemas/name/fabric-emulator.govws.lake", fields="tags").json()["tags"]}
    assert "E2EManual.Curated" in all_tags, f"label removal took a foreign tag with it: {all_tags}"
    log("label removal propagated to OpenMetadata")

    log("PASS: fabric-emulator.govws.lake.orders cataloged with "
        f"{len(want)} columns, types {sorted(set(want.values()))}; "
        "shortcut and Copy activity lineage cataloged; "
        f"{len(labels)} sensitivity labels exported as OM classification tags")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        stack.dump_logs("om-migrate", "openmetadata", "govern-ingest", "fabric-emulator")
        raise
    finally:
        stack.down()
