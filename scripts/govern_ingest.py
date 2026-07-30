#!/usr/bin/env python3
"""Catalog the fabric-emulator into OpenMetadata (idempotent).

Mapping:  workspace -> OM database, lakehouse -> OM schema, Delta table ->
OM table (columns read from the REAL Delta metadata in OneLake via delta-rs,
not from the control plane — governance sits on the bytes pipelines wrote).

Runs as the compose one-shot `govern-ingest`; env: FABRIC_URL, ENTRA_URL,
OM_URL (see docker-compose.yml). Auth: seeded daemon SP against entra for
the emulator; OpenMetadata's seeded basic-auth admin for the catalog.
"""
import base64
import json
import os
import sys
import urllib.parse

import requests
import urllib3

urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

FABRIC = os.environ.get("FABRIC_URL", "https://localhost:9443").rstrip("/")
ENTRA = os.environ.get("ENTRA_URL", "https://localhost:8443").rstrip("/")
OM = os.environ.get("OM_URL", "http://localhost:8585").rstrip("/")
TENANT = os.environ.get("FABRIC_TENANT", "11111111-1111-1111-1111-111111111111")
CLIENT_ID = os.environ.get("FABRIC_CLIENT_ID", "cccccccc-0000-0000-0000-000000000002")
CLIENT_SECRET = os.environ.get("FABRIC_CLIENT_SECRET", "daemon-app-secret")
SERVICE = "fabric-emulator"

# Delta primitive -> OpenMetadata column dataType.
TYPE_MAP = {
    "string": "STRING", "long": "BIGINT", "integer": "INT", "short": "SMALLINT",
    "byte": "TINYINT", "double": "DOUBLE", "float": "FLOAT", "boolean": "BOOLEAN",
    "binary": "BINARY", "date": "DATE", "timestamp": "TIMESTAMP",
    "timestamp_ntz": "TIMESTAMP",
}


def entra_token(scope):
    r = requests.post(
        f"{ENTRA}/{TENANT}/oauth2/v2.0/token",
        data={"grant_type": "client_credentials", "client_id": CLIENT_ID,
              "client_secret": CLIENT_SECRET, "scope": scope},
        verify=False, timeout=15)
    r.raise_for_status()
    return r.json()["access_token"]


def om_session():
    # Seeded basic-auth admin; password is base64'd in OM's login contract.
    r = requests.post(
        f"{OM}/api/v1/users/login",
        json={"email": "admin@open-metadata.org",
              "password": base64.b64encode(b"admin").decode()},
        timeout=30)
    r.raise_for_status()
    s = requests.Session()
    s.headers["Authorization"] = "Bearer " + r.json()["accessToken"]
    return s


def om_put(s, path, body):
    r = s.put(f"{OM}/api/v1/{path}", json=body, timeout=30)
    if r.status_code not in (200, 201):
        sys.exit(f"OM PUT {path} -> {r.status_code}: {r.text[:400]}")
    return r.json()


def delta_columns(workspace_name, lakehouse_name, table):
    """Read the table's schema from its real Delta log via delta-rs.

    Name-based addressing (az://{ws}/{lakehouse}.Lakehouse/…): the form the
    delta-rs e2e proves; GUID addressing on the account-prefixed Blob path
    does not currently resolve Delta logs."""
    from deltalake import DeltaTable
    url = f"az://{workspace_name}/{lakehouse_name}.Lakehouse/Tables/{table}"
    dt = DeltaTable(url, storage_options={
        "azure_storage_account_name": "onelake",
        "azure_storage_token": entra_token("https://storage.azure.com/.default"),
        "azure_endpoint": f"{FABRIC}/onelake",
        "allow_invalid_certificates": "true",  # family self-signed TLS
    })
    cols = []
    for f in json.loads(dt.schema().to_json())["fields"]:
        t = f["type"]
        prim = t if isinstance(t, str) else t.get("type", "string")
        base = str(prim).split("(")[0]
        cols.append({
            "name": f["name"],
            "dataType": TYPE_MAP.get(base, "STRING"),
            "dataTypeDisplay": str(prim),
            "constraint": "NULL" if f.get("nullable", True) else "NOT_NULL",
        })
    return cols, dt.version()


def list_tables(fabric_session, workspace_name, lakehouse_name):
    """First-level directories under Tables/ on the DFS surface = Delta tables."""
    r = fabric_session.get(
        f"{FABRIC}/{urllib.parse.quote(workspace_name)}",
        params={"resource": "filesystem", "recursive": "false",
                "directory": f"{lakehouse_name}.Lakehouse/Tables"},
        headers={"Host": "onelake.dfs.fabric.microsoft.com"}, timeout=15)
    if r.status_code == 404:
        return []
    r.raise_for_status()
    return [p["name"].rsplit("/", 1)[-1]
            for p in r.json().get("paths", []) if p.get("isDirectory") in (True, "true")]


def main():
    fab = requests.Session()
    fab.verify = False
    fab.headers["Authorization"] = "Bearer " + entra_token(
        "https://api.fabric.microsoft.com/.default")
    storage = requests.Session()
    storage.verify = False
    storage.headers["Authorization"] = "Bearer " + entra_token(
        "https://storage.azure.com/.default")

    om = om_session()
    om_put(om, "services/databaseServices", {
        "name": SERVICE,
        "serviceType": "CustomDatabase",
        "description": "Microsoft Fabric emulator (github.com/calvinchengx/fabric-emulator): "
                       "workspaces/lakehouses/Delta tables cataloged from live state.",
        "connection": {"config": {"type": "CustomDatabase",
                                  "sourcePythonClass": "",
                                  "connectionOptions": {"endpoint": FABRIC}}},
    })

    workspaces = fab.get(f"{FABRIC}/v1/workspaces", timeout=15).json()["value"]
    if not workspaces:
        sys.exit("govern-ingest: the emulator has no workspaces visible to this "
                 "principal — nothing to catalog. (If you just seeded state, "
                 "check the emulator wasn't restarted: state is in-memory "
                 "unless FABRIC_DATA_DIR is set.)")
    n_db = n_schema = n_table = 0
    for ws in workspaces:
        db_fqn = f"{SERVICE}.{ws['displayName']}"
        om_put(om, "databases", {
            "name": ws["displayName"], "service": SERVICE,
            "description": f"Fabric workspace {ws['id']}",
        })
        n_db += 1
        items = fab.get(f"{FABRIC}/v1/workspaces/{ws['id']}/items",
                        params={"type": "Lakehouse"}, timeout=15).json()["value"]
        for lh in items:
            schema_fqn = f"{db_fqn}.{lh['displayName']}"
            om_put(om, "databaseSchemas", {
                "name": lh["displayName"], "database": db_fqn,
                "description": f"Lakehouse {lh['id']}",
            })
            n_schema += 1
            for tbl in list_tables(storage, ws["displayName"], lh["displayName"]):
                cols, version = delta_columns(ws["displayName"], lh["displayName"], tbl)
                om_put(om, "tables", {
                    "name": tbl, "databaseSchema": schema_fqn,
                    "tableType": "Regular", "columns": cols,
                    "description": f"Delta table (version {version}) in OneLake "
                                   f"az://{ws['id']}/{lh['id']}/Tables/{tbl}",
                })
                n_table += 1
                print(f"  cataloged {ws['displayName']}.{lh['displayName']}.{tbl} "
                      f"({len(cols)} columns, delta v{version})", flush=True)

    print(f"govern-ingest: {n_db} database(s), {n_schema} schema(s), {n_table} table(s) "
          f"-> {OM} service '{SERVICE}'", flush=True)


if __name__ == "__main__":
    main()
