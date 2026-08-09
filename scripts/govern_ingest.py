#!/usr/bin/env python3
"""Catalog the fabric-emulator into OpenMetadata (idempotent).

Mapping:  workspace -> OM database, lakehouse -> OM schema, Delta table ->
OM table (columns read from the REAL Delta metadata in OneLake via delta-rs,
not from the control plane — governance sits on the bytes pipelines wrote).

Sensitivity labels -> OM classification tags: see LABEL_CLASSIFICATION below.

Lineage: OneLake **shortcuts** and executed pipeline Copy activities are
literal data-flow edges. A shortcut in
lakehouse B pointing at A/Tables/x means A.x feeds B.x — so each one is
cataloged as a table (schema read from the target's Delta log, since that is
the data it exposes) plus an OM lineage edge target -> shortcut. Only exact,
emulator-known edges are emitted; notebook and Script activity lineage is not
guessed from user code. See docs/22-openmetadata.md.

Runs as the compose one-shot `govern-ingest`; env: FABRIC_URL, ENTRA_URL,
OM_URL (see docker-compose.yml). Auth: seeded daemon SP against entra for
the emulator; OpenMetadata's seeded basic-auth admin for the catalog.
"""
import base64
import json
import os
import pathlib
import sys
import time
import urllib.parse

import requests
import urllib3
import yaml

urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

FABRIC = os.environ.get("FABRIC_URL", "https://localhost:9443").rstrip("/")
ENTRA = os.environ.get("ENTRA_URL", "https://localhost:8443").rstrip("/")
OM = os.environ.get("OM_URL", "http://localhost:8585").rstrip("/")
TENANT = os.environ.get("FABRIC_TENANT", "6f89cf12-978b-4d23-ac18-9ef0c127cf87")
CLIENT_ID = os.environ.get("FABRIC_CLIENT_ID", "00d88624-f0d7-46f6-a641-6232c2608928")
CLIENT_SECRET = os.environ.get("FABRIC_CLIENT_SECRET", "daemon-app-secret")
SERVICE = "fabric-emulator"

# A fresh stack is empty by design: the seed is one capacity row and nothing
# else (docs/06), because the first authenticated caller to create a workspace
# becomes its Admin. That made the documented governance flow
# (`--profile governance up` then `run --rm govern-ingest`) fail on a new
# install with nothing to catalog. So when there is nothing at all, create a
# small real demo first. Only ever fires on a completely empty emulator, so it
# cannot touch state you seeded yourself. Set GOVERN_SEED_DEMO=0 to opt out and
# get the old "nothing to catalog" exit instead.
SEED_DEMO = os.environ.get("GOVERN_SEED_DEMO", "1").strip().lower() not in ("0", "false", "no")
DEMO_WORKSPACE = os.environ.get("GOVERN_DEMO_WORKSPACE", "DemoWorkspace")
DEMO_LAKEHOUSE = os.environ.get("GOVERN_DEMO_LAKEHOUSE", "demo_lakehouse")
DEMO_TABLE = os.environ.get("GOVERN_DEMO_TABLE", "orders")

# --- sensitivity labels -> OpenMetadata classification tags ----------------
#
# Purview's Data Map speaks the Atlas API and OpenMetadata ships an Atlas
# connector, so a Purview -> OM migration carries assets, classifications,
# glossary and lineage. Sensitivity labels are the one thing it cannot carry:
# they are Microsoft Purview Information Protection objects, not Atlas
# entities. The emulator models labels (docs/parity.md, internal/api/labels.go),
# so this ingest exports them as an OM **Classification** with one **Tag** per
# label, and applies the matching tag to the entity that carries the label.
#
# Every payload shape below is from OpenMetadata's REST reference (1.13.x —
# the pinned server version), not inferred:
#
#   PUT /v1/classifications            {name, displayName, description,
#                                       mutuallyExclusive, owners, provider}
#     api-reference/governance/classifications/create
#   PUT /v1/tags                       {name, classification, displayName,
#                                       description, parent, style, owners,
#                                       provider}
#     api-reference/governance/tags/create
#   databaseSchema `tags[]` (TagLabel) {tagFQN, labelType, state}
#     api-reference/data-assets/database-schemas/create
#   GET /v1/databaseSchemas/name/{fqn}?fields=...,tags
#     api-reference/data-assets/database-schemas/retrieve
LABEL_CLASSIFICATION = os.environ.get("GOVERN_LABEL_CLASSIFICATION", "FabricSensitivity")

# Delta primitive -> OpenMetadata column dataType.
TYPE_MAP = {
    "string": "STRING", "long": "BIGINT", "integer": "INT", "short": "SMALLINT",
    "byte": "TINYINT", "double": "DOUBLE", "float": "FLOAT", "boolean": "BOOLEAN",
    "binary": "BINARY", "date": "DATE", "timestamp": "TIMESTAMP",
    "timestamp_ntz": "TIMESTAMP", "decimal": "DECIMAL",
    "struct": "STRUCT", "array": "ARRAY", "map": "MAP",
}


# OpenMetadata rejects a table outright when a column of one of these types
# carries no dataLength.
LENGTH_REQUIRED = {"CHAR", "VARCHAR", "BINARY", "VARBINARY"}

# Matches the VARBINARY(4000) the SQL analytics endpoint reflects a Delta
# `binary` column as.
DEFAULT_BINARY_LENGTH = 4000


# --- ODCS contracts -> catalog semantics -------------------------------------
# The emulator can tell OpenMetadata a table's SHAPE — columns, types, Delta
# version. It cannot tell it what the table MEANS, because the emulator does
# not know: meaning lives in the data contract. Without this, every table in the
# catalog gets the same machine-written description and no PII marking at all,
# which is the difference between an inventory and a catalog.
CONTRACTS = pathlib.Path(os.environ.get(
    "ODCS_CONTRACTS",
    pathlib.Path(__file__).resolve().parent.parent / "examples/medallion-pyspark/contracts"))

# Which contract element describes which Delta table. Explicit rather than
# derived, because it is a modelling decision and not a naming rule: a landing
# contract's element describes the FILE the vendor sent ("customers"), and the
# bronze table is that file ingested ("bronze_customers"). Asserting the two are
# the same thing is a claim, so it is written down where it can be argued with.
CONTRACT_FOR_TABLE = {
    "bronze_customers": ("landing-contoso-pos", "customers"),
    "bronze_orders": ("landing-contoso-pos", "orders"),
    "bronze_erp_changes": ("landing-contoso-erp", "changes"),
    "bronze_fx_rates": ("reference-data", "fx_rates"),
    "bronze_product_hierarchy": ("reference-data", "product_hierarchy"),
    "silver_customers": ("silver-sales", "silver_customers"),
    "silver_orders": ("silver-sales", "silver_orders"),
}


def load_contracts():
    """Read every ODCS contract next to the example. Missing dir is not fatal:
    the catalog is still useful without semantics, just poorer."""
    out = {}
    if not CONTRACTS.is_dir():
        return out
    for path in sorted(CONTRACTS.glob("*.odcs.yaml")):
        try:
            with path.open() as f:
                out[path.name.replace(".odcs.yaml", "")] = yaml.safe_load(f)
        except Exception as e:  # noqa: BLE001 — a malformed contract must not
            print(f"  ! skipping {path.name}: {e}", flush=True)
    return out


def contract_element(contracts, table):
    """The (contract, schema-entry) describing `table`, or (None, None)."""
    ref = CONTRACT_FOR_TABLE.get(table)
    if not ref:
        return None, None
    contract = contracts.get(ref[0])
    if not contract:
        return None, None
    for entry in contract.get("schema", []) or []:
        if entry["name"] == ref[1]:
            return contract, entry
    return contract, None


def contract_description(contract, entry, fallback):
    """A description a human wrote, plus the limitations they wrote down.

    `limitations` is the field most worth surfacing: it is where a contract
    records what is KNOWN WRONG with a feed, and a catalog that shows the happy
    path while hiding that is actively misleading.
    """
    if not contract or not entry:
        return fallback
    parts = [entry.get("description", "").strip()]
    if gran := entry.get("dataGranularityDescription", "").strip():
        parts.append(f"**Granularity.** {gran}")
    if lim := (contract.get("description", {}) or {}).get("limitations", "").strip():
        parts.append(f"**Known limitations.** {lim}")
    parts.append(f"_Contract: {contract['name']} v{contract.get('version', '?')} "
                 f"(ODCS {contract.get('apiVersion', '?')})._")
    parts.append(fallback)
    return "\n\n".join(p for p in parts if p)


def contract_column_tags(entry):
    """column name -> OM tag FQNs, from the contract's classifications.

    ODCS `classification: pii` becomes OpenMetadata's built-in PII
    classification, which is what drives its masking and access policies —
    carrying it as prose in a description would look the same and do nothing.
    """
    tags = {}
    for prop in (entry or {}).get("properties", []) or []:
        fqns = []
        if str(prop.get("classification", "")).lower() == "pii":
            fqns.append("PII.Sensitive")
        if prop.get("criticalDataElement"):
            fqns.append("Tier.Tier1")
        if fqns:
            tags[prop["name"]] = fqns
    return tags


def apply_column_tags(columns, tags):
    """Attach tag labels to the OM column payloads, in place."""
    n = 0
    for col in columns:
        for fqn in tags.get(col["name"], []):
            col.setdefault("tags", []).append({
                "tagFQN": fqn, "labelType": "Manual",
                "state": "Confirmed", "source": "Classification"})
            n += 1
    return n


def contract_rule_summary(entry):
    """The quality rules, as a line a catalog user can read.

    OpenMetadata models executable tests as TestCases against a TestSuite, which
    needs a live connection it can run them through. These are the emulator's
    Delta tables, which OM cannot query directly — so the rules are surfaced as
    documented expectations, and contract_gates.py is what actually executes
    them. Saying so is better than creating TestCases that would never run.
    """
    rules = list(entry.get("quality", []) or [])
    for prop in entry.get("properties", []) or []:
        rules.extend(prop.get("quality", []) or [])
    if not rules:
        return ""
    named = []
    for r in rules:
        if r.get("metric"):
            bound = next((f"{k} {r[k]}" for k in
                          ("mustBe", "mustBeGreaterThan", "mustBeLessThan") if k in r), "")
            named.append(f"`{r['metric']} {bound}`".replace(" `", "`"))
        elif r.get("name"):
            named.append(f"`{r['name']}` (sql)")
    return ("**Contracted quality rules** (executed by "
            "`examples/medallion-pyspark/contract_gates.py`): " + ", ".join(named))


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
    # OpenMetadata REFUSES these without a length: "For column data types char,
    # varchar, binary, varbinary dataLength must not be null" — a 400 on the
    # whole table, not a warning on the column, so one binary column loses the
    # entire table from the catalog.
    #
    # Delta declares no length for `binary`, so there is nothing to carry
    # across. The width the emulator's own SQL analytics endpoint exposes is
    # used instead (VARBINARY(4000) — see internal/warehouse/reflect.go), which
    # at least makes the catalog agree with the surface a client queries.
    if col["dataType"] in LENGTH_REQUIRED:
        col["dataLength"] = DEFAULT_BINARY_LENGTH
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


def list_activity_lineage(fab, workspace_id):
    """Exact source/sink edges recorded by successful activity execution."""
    r = fab.get(f"{FABRIC}/v1/workspaces/{workspace_id}/lineage", timeout=15)
    if r.status_code != 200:
        return []
    return r.json().get("value", [])


def om_lineage(s, from_fqn, to_fqn, description="OneLake shortcut (fabric-emulator)"):
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
                             "lineageDetails": {"description": description}}},
              timeout=30)
    if r.status_code not in (200, 201):
        sys.exit(f"OM lineage {from_fqn} -> {to_fqn}: {r.status_code} {r.text[:300]}")
    return True


def export_label_taxonomy(fab, om):
    """Mirror the emulator's label taxonomy as an OM classification.

    Returns {label id -> tag FQN}. The whole taxonomy is exported, not only
    the labels currently in use: a classification a governance user can pick
    from is the point, and it is what makes the Purview gap visible offline.
    """
    r = fab.get(f"{FABRIC}/v1/admin/labels", timeout=15)
    if r.status_code != 200:
        print(f"govern-ingest: GET /v1/admin/labels -> {r.status_code}; "
              "no sensitivity labels exported", flush=True)
        return {}
    labels = r.json().get("labels", [])
    if not labels:
        return {}

    om_put(om, "classifications", {
        "name": LABEL_CLASSIFICATION,
        "displayName": "Fabric sensitivity",
        "description": (
            "Microsoft Fabric sensitivity labels, exported from the "
            "fabric-emulator by scripts/govern_ingest.py. Labels are Purview "
            "Information Protection objects rather than Atlas entities, so an "
            "Atlas-based Purview import does not carry them; this classification "
            "is how they reach the catalog. The taxonomy is emulator-provided — "
            "real Fabric sources it from Microsoft Purview."),
        # A Fabric item carries at most one sensitivity label, so the tags
        # under this classification are mutually exclusive by construction.
        "mutuallyExclusive": True,
    })
    for label in labels:
        om_put(om, "tags", {
            "name": label["name"],
            "classification": LABEL_CLASSIFICATION,
            "displayName": label["name"],
            "description": (
                f"Fabric sensitivity label {label['name']!r} "
                f"(id {label['id']}, sensitivity order {label['order']}; "
                "higher is more restrictive)."),
        })
    fqns = {label["id"]: f"{LABEL_CLASSIFICATION}.{label['name']}" for label in labels}
    print(f"govern-ingest: exported {len(fqns)} sensitivity label(s) as "
          f"classification '{LABEL_CLASSIFICATION}'", flush=True)
    return fqns


def item_label_tag(fab, workspace_id, item_id, tag_by_label):
    """The tag FQN for an item's sensitivity label, or None if it carries none.

    The label sits on the Item object as `sensitivityLabel: {id}` (the
    documented REST shape — top level, id only). List Items does not carry it,
    so the item is read individually.
    """
    r = fab.get(f"{FABRIC}/v1/workspaces/{workspace_id}/items/{item_id}", timeout=15)
    if r.status_code != 200:
        return None
    label_id = ((r.json().get("sensitivityLabel") or {}).get("id"))
    return tag_by_label.get(label_id) if label_id else None


def sync_schema_label_tag(om, schema_id, schema_fqn, tag_fqn):
    """Make the schema's sensitivity tag match the item's label exactly.

    Reports whether the entity ends up carrying a label tag.

    Why this is a PATCH rather than part of the upsert: OpenMetadata's
    create-or-update **adds** tags but never removes one that is absent from
    the payload. The governance e2e's negative control caught that — clearing
    a label with `bulkRemoveLabels` and re-ingesting left the old tag sitting
    on the entity, so a removed or downgraded label never reached the catalog.
    A JSON Patch does remove. `add` on `/tags` replaces the member when it is
    present and creates it when it is not (RFC 6902 §4.1), so the same
    operation works on a freshly created schema and on an updated one.

    Tags from other classifications are carried through untouched, exactly as
    OM returned them (so no field is invented on the way): an ingest that
    silently deletes hand-applied catalog tags is a governance bug of its own.

    labelType "Automated" is the reference's own definition — "used when a
    tool determined the tag label" — which is what an ingest is, as opposed to
    a person tagging inside OpenMetadata.
    """
    r = om.get(f"{OM}/api/v1/databaseSchemas/name/{urllib.parse.quote(schema_fqn)}",
               params={"fields": "tags"}, timeout=30)
    current = (r.json().get("tags") or []) if r.status_code == 200 else []
    desired = [t for t in current
               if not str(t.get("tagFQN", "")).startswith(LABEL_CLASSIFICATION + ".")]
    if tag_fqn:
        desired.append({"tagFQN": tag_fqn, "labelType": "Automated", "state": "Confirmed"})
    if {t["tagFQN"] for t in desired} != {t.get("tagFQN") for t in current}:
        resp = om.patch(
            f"{OM}/api/v1/databaseSchemas/{schema_id}",
            data=json.dumps([{"op": "add", "path": "/tags", "value": desired}]),
            headers={"Content-Type": "application/json-patch+json"}, timeout=30)
        if resp.status_code not in (200, 201):
            sys.exit(f"OM PATCH tags on {schema_fqn}: {resp.status_code} {resp.text[:300]}")
    return bool(tag_fqn)


def seed_demo(fab):
    """Create one workspace + lakehouse + a REAL Delta table, so a fresh stack
    has something to catalog.

    The table is written through delta-rs rather than faked, because the whole
    point of this script is that governance reads the bytes pipelines actually
    wrote. Column types mirror e2e/governance/run.py on purpose: a decimal, an
    array and a struct, so the demo catalog exercises the parts of the mapping
    that are easy to get wrong (a decimal(10,2) silently flattened to STRING is
    exactly the loss a governance user notices).
    """
    print(f"govern-ingest: emulator is empty, seeding demo workspace "
          f"'{DEMO_WORKSPACE}' (GOVERN_SEED_DEMO=0 to disable)")

    r = fab.post(f"{FABRIC}/v1/workspaces", json={"displayName": DEMO_WORKSPACE}, timeout=15)
    if r.status_code != 201:
        sys.exit(f"govern-ingest: seeding workspace failed: {r.status_code} {r.text[:300]}")
    ws = r.json()

    # Typed collection, same as the e2e. Item create may answer 202 (LRO), so
    # confirm the lakehouse exists by name before addressing it as a path.
    r = fab.post(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses",
                 json={"displayName": DEMO_LAKEHOUSE}, timeout=15)
    if r.status_code not in (201, 202):
        sys.exit(f"govern-ingest: seeding lakehouse failed: {r.status_code} {r.text[:300]}")
    deadline = time.time() + 60
    while time.time() < deadline:
        items = fab.get(f"{FABRIC}/v1/workspaces/{ws['id']}/items",
                        params={"type": "Lakehouse"}, timeout=15).json().get("value", [])
        if any(i["displayName"] == DEMO_LAKEHOUSE for i in items):
            break
        time.sleep(1)
    else:
        sys.exit(f"govern-ingest: lakehouse '{DEMO_LAKEHOUSE}' never appeared after create")

    from decimal import Decimal

    import pyarrow as pa
    from deltalake import write_deltalake

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
    write_deltalake(
        f"az://{DEMO_WORKSPACE}/{DEMO_LAKEHOUSE}.Lakehouse/Tables/{DEMO_TABLE}",
        tbl,
        storage_options={
            "azure_storage_account_name": "onelake",
            "azure_storage_token": entra_token("https://storage.azure.com/.default"),
            "azure_endpoint": f"{FABRIC}/onelake",
            "allow_invalid_certificates": "true",  # family self-signed TLS
        })
    print(f"govern-ingest: seeded {DEMO_WORKSPACE}/{DEMO_LAKEHOUSE}.Lakehouse/"
          f"Tables/{DEMO_TABLE} ({tbl.num_rows} rows via delta-rs)")


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
    if not workspaces and SEED_DEMO:
        seed_demo(fab)
        workspaces = fab.get(f"{FABRIC}/v1/workspaces", timeout=15).json()["value"]
    if not workspaces:
        sys.exit("govern-ingest: the emulator has no workspaces visible to this "
                 "principal — nothing to catalog, and seeding is off "
                 "(GOVERN_SEED_DEMO=0). (If you just seeded state, check the "
                 "emulator wasn't restarted: state is in-memory unless "
                 "FABRIC_DATA_DIR is set.)")
    # Before anything is tagged: the tags have to exist to be applied.
    tag_by_label = export_label_taxonomy(fab, om)

    n_db = n_schema = n_table = n_labelled = 0
    n_contracted = n_pii = 0
    contracts = load_contracts()
    if contracts:
        print(f"loaded {len(contracts)} ODCS contract(s) from {CONTRACTS}", flush=True)
    else:
        print(f"no ODCS contracts at {CONTRACTS} — cataloging shape only", flush=True)
    item_fqn = {}
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
            item_fqn[lh["id"]] = schema_fqn
            schema = om_put(om, "databaseSchemas", {
                "name": lh["displayName"], "database": db_fqn,
                "description": f"Lakehouse {lh['id']}",
            })
            n_schema += 1
            tag_fqn = item_label_tag(fab, ws["id"], lh["id"], tag_by_label)
            if sync_schema_label_tag(om, schema["id"], schema_fqn, tag_fqn):
                n_labelled += 1
                print(f"  labelled {schema_fqn} -> {tag_fqn}", flush=True)
            for tbl in list_tables(storage, ws["displayName"], lh["displayName"]):
                cols, version = delta_columns(ws["displayName"], lh["displayName"], tbl)
                machine = (f"Delta table (version {version}) in OneLake "
                           f"az://{ws['id']}/{lh['id']}/Tables/{tbl}")

                # Semantics from the data contract, where one covers this table.
                contract, entry = contract_element(contracts, tbl)
                desc = contract_description(contract, entry, machine)
                if entry and (rules := contract_rule_summary(entry)):
                    desc = f"{desc}\n\n{rules}"
                tagged = apply_column_tags(cols, contract_column_tags(entry))

                om_put(om, "tables", {
                    "name": tbl, "databaseSchema": schema_fqn,
                    "tableType": "Regular", "columns": cols,
                    "description": desc,
                })
                n_table += 1
                if contract and entry:
                    n_contracted += 1
                    n_pii += tagged
                note = (f", contract {contract['name']}, {tagged} column tag(s)"
                        if contract and entry else "")
                print(f"  cataloged {ws['displayName']}.{lh['displayName']}.{tbl} "
                      f"({len(cols)} columns, delta v{version}{note})", flush=True)

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

    # Third pass: exact table-to-table edges emitted by successful Copy
    # activities. Files paths remain queryable through the emulator endpoint,
    # but are not forced into OpenMetadata's table entity model.
    n_activity_edge = 0
    for ws in workspaces:
        for edge in list_activity_lineage(fab, ws["id"]):
            src_path = edge.get("sourcePath", "").strip("/")
            dst_path = edge.get("targetPath", "").strip("/")
            if not src_path.startswith("Tables/") or not dst_path.startswith("Tables/"):
                continue
            src_table, dst_table = src_path.split("/", 1)[1], dst_path.split("/", 1)[1]
            src_schema = item_fqn.get(edge.get("sourceItemId"))
            dst_schema = item_fqn.get(edge.get("targetItemId"))
            if not src_schema or not dst_schema:
                continue
            # Name the PRODUCER, not "Copy". This loop reads every exact edge
            # the emulator recorded, and since flow observability landed that
            # includes a warehouse build the TDS front watched, a Direct Lake
            # binding, and a step's own report — none of which is a Copy
            # activity. Describing them all as one would put a claim about
            # provenance into the catalog that the emulator never made.
            producer = edge.get("producer") or "Copy"
            if om_lineage(om, f"{src_schema}.{src_table}", f"{dst_schema}.{dst_table}",
                          f"Fabric {producer} {edge.get('activityName')} (fabric-emulator)"):
                n_activity_edge += 1
                print(f"  activity lineage {src_schema}.{src_table} -> "
                      f"{dst_schema}.{dst_table}", flush=True)

    print(f"govern-ingest: {n_db} database(s), {n_schema} schema(s), "
          f"{n_table} table(s), {n_short} shortcut(s), {n_edge} shortcut lineage edge(s), "
          f"{n_activity_edge} activity lineage edge(s), "
          f"{n_contracted} contract-described table(s), {n_pii} classified column(s), "
          f"{n_labelled} sensitivity-labelled schema(s) "
          f"-> {OM} service '{SERVICE}'", flush=True)


if __name__ == "__main__":
    main()
