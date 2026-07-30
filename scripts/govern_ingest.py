#!/usr/bin/env python3
"""Catalog the fabric-emulator into OpenMetadata (idempotent).

Mapping:  workspace -> OM database, lakehouse -> OM schema, Delta table ->
OM table (columns read from the REAL Delta metadata in OneLake via delta-rs,
not from the control plane — governance sits on the bytes pipelines wrote).

Lineage: OneLake **shortcuts** are literal data-flow edges — a shortcut in
lakehouse B pointing at A/Tables/x means A.x feeds B.x — so each one is
cataloged as a table (schema read from the target's Delta log, since that is
the data it exposes) plus an OM lineage edge target -> shortcut. Only exact,
emulator-known edges are emitted: activity-level lineage (which tables a
notebook or Script activity touched) would require executing or parsing user
code, so it is deliberately NOT invented. See docs/22-openmetadata.md.

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
    "timestamp_ntz": "TIMESTAMP", "decimal": "DECIMAL",
    "struct": "STRUCT", "array": "ARRAY", "map": "MAP",
}


def om_column(name, dtype, nullable=True):
    """Map one Delta field to an OpenMetadata column.

    Carries the detail a catalog is judged on: decimal precision/scale (a
    silent STRING here is exactly the loss governance users notice), array
    element type, and struct children — rather than flattening everything
    that is not a primitive to STRING.
    """
    # delta-rs renders primitives as strings ("string", "decimal(10,2)") and
    # nested types as objects ({"type": "struct", "fields": [...]}, …).
    kind = dtype if isinstance(dtype, str) else dtype.get("type", "string")
    base = str(kind).split("(")[0]
    col = {
        "name": name,
        "dataType": TYPE_MAP.get(base, "STRING"),
        "dataTypeDisplay": str(kind) if isinstance(dtype, str) else base,
        "constraint": "NULL" if nullable else "NOT_NULL",
    }
    if base == "decimal" and "(" in str(kind):
        precision, _, scale = str(kind).split("(", 1)[1].rstrip(")").partition(",")
        try:
            col["precision"] = int(precision)
            col["scale"] = int(scale or 0)
        except ValueError:
            pass
    elif base == "array" and isinstance(dtype, dict):
        el = dtype.get("elementType", "string")
        el_kind = el if isinstance(el, str) else el.get("type", "string")
        col["arrayDataType"] = TYPE_MAP.get(str(el_kind).split("(")[0], "STRING")
        col["dataTypeDisplay"] = f"array<{el_kind}>"
    elif base == "struct" and isinstance(dtype, dict):
        col["children"] = [
            om_column(f["name"], f["type"], f.get("nullable", True))
            for f in dtype.get("fields", [])
        ]
    return col


_token_cache = {}


def entra_token(scope):
    """Cached per scope: delta_columns() runs per table, and re-minting for
    every one is pure churn against the STS."""
    if scope in _token_cache:
        return _token_cache[scope]
    r = requests.post(
        f"{ENTRA}/{TENANT}/oauth2/v2.0/token",
        data={"grant_type": "client_credentials", "client_id": CLIENT_ID,
              "client_secret": CLIENT_SECRET, "scope": scope},
        verify=False, timeout=15)
    r.raise_for_status()
    _token_cache[scope] = r.json()["access_token"]
    return _token_cache[scope]


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
    cols = [om_column(f["name"], f["type"], f.get("nullable", True))
            for f in json.loads(dt.schema().to_json())["fields"]]
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


def list_shortcuts(fab, workspace_id, item_id):
    """Shortcuts declared on an item (OneLake -> OneLake only)."""
    r = fab.get(f"{FABRIC}/v1/workspaces/{workspace_id}/items/{item_id}/shortcuts", timeout=15)
    if r.status_code != 200:
        return []
    return r.json().get("value", [])


def om_lineage(s, from_fqn, to_fqn):
    """Add a table -> table edge. Idempotent: OM upserts by entity pair."""
    def ref(fqn):
        r = s.get(f"{OM}/api/v1/tables/name/{fqn}", timeout=30)
        if r.status_code != 200:
            return None
        return {"id": r.json()["id"], "type": "table"}

    a, b = ref(from_fqn), ref(to_fqn)
    if not a or not b:
        return False
    r = s.put(f"{OM}/api/v1/lineage",
              json={"edge": {"fromEntity": a, "toEntity": b,
                             "lineageDetails": {"description":
                                                "OneLake shortcut (fabric-emulator)"}}},
              timeout=30)
    if r.status_code not in (200, 201):
        sys.exit(f"OM lineage {from_fqn} -> {to_fqn}: {r.status_code} {r.text[:300]}")
    return True


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

    # Second pass: shortcuts. Needs every real table cataloged first, because
    # a shortcut edge references its target table by FQN.
    n_short = n_edge = 0
    for ws in workspaces:
        items = fab.get(f"{FABRIC}/v1/workspaces/{ws['id']}/items",
                        params={"type": "Lakehouse"}, timeout=15).json()["value"]
        by_id = {i["id"]: i for i in items}
        for lh in items:
            for sc in list_shortcuts(fab, ws["id"], lh["id"]):
                target = (sc.get("target") or {}).get("oneLake") or {}
                tgt_item = by_id.get(target.get("itemId"))
                tgt_path = (target.get("path") or "").strip("/")
                # Only Tables/<name> shortcuts are catalog-visible tables.
                if sc.get("path", "").strip("/") != "Tables" or not tgt_item \
                        or not tgt_path.startswith("Tables/"):
                    continue
                tgt_table = tgt_path.split("/", 1)[1]
                try:
                    cols, version = delta_columns(
                        ws["displayName"], tgt_item["displayName"], tgt_table)
                except Exception as e:  # target isn't a Delta table — skip, don't guess
                    print(f"  shortcut {sc['name']}: target not readable as Delta ({e}); "
                          "cataloged without columns", flush=True)
                    cols, version = [], "?"
                schema_fqn = f"{SERVICE}.{ws['displayName']}.{lh['displayName']}"
                om_put(om, "tables", {
                    "name": sc["name"], "databaseSchema": schema_fqn,
                    "tableType": "External", "columns": cols,
                    "description": f"OneLake shortcut -> {tgt_item['displayName']}/"
                                   f"{tgt_path} (Delta version {version})",
                })
                n_short += 1
                if om_lineage(om,
                              f"{SERVICE}.{ws['displayName']}.{tgt_item['displayName']}.{tgt_table}",
                              f"{schema_fqn}.{sc['name']}"):
                    n_edge += 1
                    print(f"  lineage {tgt_item['displayName']}.{tgt_table} -> "
                          f"{lh['displayName']}.{sc['name']}", flush=True)

    print(f"govern-ingest: {n_db} database(s), {n_schema} schema(s), "
          f"{n_table} table(s), {n_short} shortcut(s), {n_edge} lineage edge(s) "
          f"-> {OM} service '{SERVICE}'", flush=True)


if __name__ == "__main__":
    main()
