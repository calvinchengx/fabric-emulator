"""Register this medallion in OpenMetadata: the ontology, the glossary, the
metrics, the contracts, and the lineage.

Everything here is DERIVED, never authored twice. The domain and data product
come from what the pipeline built, the business terms and column meanings come
from the ODCS contracts in `contracts/`, the quality rules come from those same
contracts, and the lineage comes from what the emulator recorded while the run
happened. A catalog whose semantics are typed in a second time is a catalog
that drifts from the pipeline by the end of the first sprint.

WHY THIS STEP EXISTS SEPARATELY FROM `scripts/govern_ingest.py`: that script is
a general walker — point it at any emulator and it catalogs whatever it finds.
This step knows what it built, which lets it say things a walker cannot: that
`fct_daily_revenue` carries the measure the semantic model serves, that
`dim_customer_360` is the resolved-identity dimension and `dim_customer` is
not, and which contract governs which table.

OpenMetadata is OPTIONAL. `docker compose --profile governance up` starts it;
without it this step skips rather than fails, because the medallion is not
about the catalog and a reader who has not opted in should not hit a wall.

The one thing this step will not do is invent. OM models executable tests as
TestCases against a live connection it can query, which it does not have for
these Delta tables — so the contract's rules are registered as a **DataContract**
with `odcsQualityRules`, which is exactly what they are: a stated expectation,
executed by contract_gates.py, recorded here so a catalog user can see what the
data promises. Rules are not fabricated into tests that would never run.
"""
import base64
import os

import requests
import yaml
from common import FABRIC, S, fabric_headers, load, log

OM = os.environ.get("OM_URL", "http://localhost:8585")
HERE = os.path.dirname(os.path.abspath(__file__))
CONTRACTS = os.path.join(HERE, "contracts")

# The catalog's shape. A Fabric workspace is a database; the lakehouse and the
# warehouse are its schemas — which is what they are on the SQL surface too, so
# a catalog user and a SQL user see the same names.
SERVICE = "fabric-emulator"
DOMAIN = "contoso-sales"
GLOSSARY = "Contoso Sales"

# Which contract governs which table. The mapping is explicit because a name
# match would be a guess: `dim_customer` and `dim_customer_360` are both real
# and only one of them is the resolved-identity dimension.
CONTRACT_FOR = {
    "bronze_customers": ("landing-contoso-pos", "customers"),
    "bronze_orders": ("landing-contoso-pos", "orders"),
    "bronze_web_customers": ("landing-contoso-web", "customers"),
    "bronze_web_order_lines": ("landing-contoso-web", "orders"),
    "bronze_erp_changes": ("landing-contoso-erp", "changes"),
    "bronze_fx_rates": ("reference-data", "fx_rates"),
    "bronze_product_hierarchy": ("reference-data", "product_hierarchy"),
    "silver_customers": ("silver-sales", "silver_customers"),
    "silver_orders": ("silver-sales", "silver_orders"),
    "dim_customer": ("gold-sales", "dim_customer"),
    "dim_customer_360": ("gold-sales", "dim_customer_360"),
    "fct_orders": ("gold-sales", "fct_orders"),
    "fct_daily_revenue": ("gold-sales", "fct_daily_revenue"),
}

# The measures gold publishes. A metric is not a column — it is a column plus
# how you aggregate it, which is precisely the thing a column entity cannot
# say and the thing a report writer needs. Expressions are the SQL gold really
# supports, so a consumer can check them rather than trust them.
METRICS = [
    ("daily_revenue", "SUM", "DOLLARS", "DAY",
     "select sum(revenue) from fct_daily_revenue where order_date = ?",
     "Revenue for one trading day, across both selling channels. The measure "
     "the semantic model serves and Power BI reads."),
    ("orders_per_day", "COUNT", "TRANSACTIONS", "DAY",
     "select sum(orders) from fct_daily_revenue where order_date = ?",
     "Orders counted at order grain, not line grain — a five-line order is one."),
    ("resolved_customers", "COUNT", "COUNT", "DAY",
     "select count(*) from dim_customer_360",
     "People, not source records. 129,526 identities resolved from 168,800 "
     "rows across three systems; the gap IS the resolution."),
    ("multi_source_customers", "COUNT", "COUNT", "DAY",
     "select count(*) from dim_customer_360 where source_count > 1",
     "Customers at least two systems know about. The number that silently "
     "goes to zero if resolution stops matching and every other total still "
     "balances."),
]


def om_session():
    """Log in, or return None when OpenMetadata is not running."""
    try:
        r = requests.post(f"{OM}/api/v1/users/login", timeout=10,
                          json={"email": "admin@open-metadata.org",
                                "password": base64.b64encode(b"admin").decode()})
        r.raise_for_status()
    except (requests.RequestException, ValueError):
        return None
    s = requests.Session()
    s.headers["Authorization"] = "Bearer " + r.json()["accessToken"]
    return s


def put(s, path, body):
    r = s.put(f"{OM}/api/v1/{path}", json=body, timeout=30)
    assert r.status_code in (200, 201), f"PUT {path} -> {r.status_code} {r.text[:300]}"
    return r.json()


def load_contracts():
    out = {}
    for name in sorted(os.listdir(CONTRACTS)):
        if name.endswith(".odcs.yaml"):
            with open(os.path.join(CONTRACTS, name)) as f:
                out[name.replace(".odcs.yaml", "")] = yaml.safe_load(f)
    return out


def element(contracts, table):
    """The (contract, schema entry) governing `table`, or (None, None)."""
    key = CONTRACT_FOR.get(table)
    if not key:
        return None, None
    contract = contracts.get(key[0])
    if not contract:
        return None, None
    for entry in contract.get("schema", []) or []:
        if entry.get("name") == key[1]:
            return contract, entry
    return contract, None


# OpenMetadata's ODCSQualityRule enums, from its own OpenAPI spec
# (GET /swagger.json). They are ODCS's vocabulary but NOT all of it: ODCS
# defines `columnCount`, which OM has no member for, and a rule carrying it is
# rejected with a bare `400 Invalid request format` naming nothing.
OM_METRICS = {"rowCount", "nullValues", "invalidValues", "duplicateValues",
              "missingValues", "uniqueValues", "distinctValues", "completeness",
              "freshness"}
OM_DIMENSIONS = {"accuracy", "completeness", "conformity", "consistency",
                 "coverage", "timeliness", "uniqueness",
                 "ac", "cp", "cf", "cs", "cv", "tm", "uq"}


def odcs_rules(entry):
    """Every quality rule on the object and its properties, in the shape OM's
    DataContract already speaks.

    OM's ODCSQualityRule carries most of ODCS's vocabulary — `metric` and
    `dimension` from overlapping enums, the same mustBe/validValues comparators
    — so this is mostly a rename rather than a translation.

    Where the two DIVERGE the rule is kept but demoted to `type: text` with the
    unsupported metric named in its description. Two alternatives were worse:
    coercing `columnCount` onto a member OM does have would state a rule the
    contract never made, and dropping it would quietly shrink the contract while
    everything still looked registered. A reader sees every rule the contract
    states, and sees which ones OM cannot model as structured metrics.
    """
    keep = {"mustBe", "mustNotBe", "mustBeGreaterThan", "mustBeGreaterOrEqualTo",
            "mustBeLessThan", "mustBeLessOrEqualTo", "mustBeBetween",
            "mustNotBeBetween", "validValues"}
    out = []
    rules = list(entry.get("quality", []) or [])
    for prop in entry.get("properties", []) or []:
        for r in prop.get("quality", []) or []:
            rules.append({**r, "column": prop["name"]})
    for r in rules:
        rule = {k: r[k] for k in ("type", "name", "description", "metric",
                                  "column", "query", "dimension", "unit",
                                  "businessImpact") if r.get(k)}
        rule.setdefault("type", "library")
        for k in keep:
            if k in r:
                rule[k] = r[k]
        # Demote rather than coerce or drop — see this function's docstring.
        metric = rule.get("metric")
        if metric and metric not in OM_METRICS:
            rule["type"] = "text"
            rule.pop("metric")
            rule["description"] = (
                f"[metric `{metric}`, which OpenMetadata does not model] "
                + rule.get("description", "")).strip()
        if rule.get("dimension") and rule["dimension"] not in OM_DIMENSIONS:
            rule.pop("dimension")
        # arguments.validValues is where ODCS puts an accepted-values domain.
        args = r.get("arguments") or {}
        if "validValues" in args:
            rule["validValues"] = args["validValues"]
        out.append(rule)
    return out


# SQL Server's type names → OpenMetadata's. Anything unmapped is reported
# rather than silently flattened to STRING: a decimal that arrives as text is
# exactly the loss a governance user notices, and a guess here would hide it.
SQL_TO_OM = {
    "bigint": "BIGINT", "int": "INT", "smallint": "SMALLINT", "tinyint": "TINYINT",
    "bit": "BOOLEAN", "float": "DOUBLE", "real": "FLOAT",
    "decimal": "DECIMAL", "numeric": "DECIMAL", "money": "DECIMAL",
    "date": "DATE", "datetime": "DATETIME", "datetime2": "DATETIME",
    "datetimeoffset": "DATETIME", "time": "TIME",
    "char": "CHAR", "nchar": "CHAR", "varchar": "VARCHAR", "nvarchar": "VARCHAR",
    "text": "TEXT", "ntext": "TEXT", "uniqueidentifier": "UUID",
    "varbinary": "BINARY", "binary": "BINARY",
}


def sql_columns(database):
    """Every table and its columns, read from the SQL surface.

    The lakehouse endpoint reflects silver's Delta into queryable T-SQL and the
    warehouse holds gold, so one query shape covers both layers — and it is the
    same view a SQL consumer gets, rather than a second description of the data
    that could disagree with it.
    """
    from common import tds_connect
    out = {}
    with tds_connect(database) as conn:
        rows = conn.cursor().execute("""
            SELECT t.name, c.name, ty.name, c.max_length, c.precision, c.scale, c.is_nullable
            FROM sys.tables t
            JOIN sys.columns c ON c.object_id = t.object_id
            JOIN sys.types ty ON ty.user_type_id = c.user_type_id
            ORDER BY t.name, c.column_id""").fetchall()
    for tname, cname, ctype, maxlen, prec, scale, nullable in rows:
        col = {"name": cname, "dataType": SQL_TO_OM.get(ctype.lower(), "UNKNOWN"),
               "dataTypeDisplay": ctype}
        if col["dataType"] in ("VARCHAR", "CHAR", "BINARY"):
            col["dataLength"] = 4000 if maxlen in (-1, None) else max(1, int(maxlen))
        if col["dataType"] == "DECIMAL":
            col["precision"], col["scale"] = int(prec), int(scale)
        out.setdefault(tname, []).append(col)
    return out


def register_tables(om, st, ws_name):
    """Catalog the medallion's own tables, with the contract's meaning attached.

    A Fabric workspace becomes a database and each item a schema, so `lake` and
    `dw` are the same names a SQL client connects to. Column descriptions come
    from the contract that governs the table — the catalog says what the
    business meaning is because the contract already stated it.
    """
    contracts = load_contracts()
    put(om, "services/databaseServices", {
        "name": SERVICE, "serviceType": "CustomDatabase",
        "description": "Microsoft Fabric, emulated locally (fabric-emulator).",
        "connection": {"config": {"type": "CustomDatabase",
                                  "sourcePythonClass": "fabric_emulator"}}})
    put(om, "databases", {"name": ws_name, "service": SERVICE,
                          "description": f"Fabric workspace {ws_name!r}."})
    known = {}
    for item_key, schema in (("lakehouse", st["lakehouse_name"]), ("warehouse", "dw")):
        put(om, "databaseSchemas", {
            "name": schema, "database": f"{SERVICE}.{ws_name}",
            "description": ("Lakehouse — landing, bronze and silver as Delta."
                            if item_key == "lakehouse" else
                            "Warehouse — the conformed gold star, T-SQL.")})
        try:
            tables = sql_columns(st[item_key])
        except Exception as e:  # noqa: BLE001 — a missing surface is not fatal
            log(f"  ! {schema}: {type(e).__name__} reading columns — skipped ({e})")
            continue
        for tname, cols in sorted(tables.items()):
            contract, entry = element(contracts, tname)
            by_name = {p["name"]: p for p in (entry or {}).get("properties", []) or []}
            for c in cols:
                prop = by_name.get(c["name"])
                if prop and (prop.get("description") or "").strip():
                    c["description"] = prop["description"].strip()
                if prop and prop.get("classification") == "pii":
                    c["tags"] = [{"tagFQN": "PII.Sensitive", "source": "Classification",
                                  "labelType": "Manual", "state": "Confirmed"}]
            body = {"name": tname, "databaseSchema": f"{SERVICE}.{ws_name}.{schema}",
                    "columns": cols, "domains": [DOMAIN]}
            if entry and (entry.get("description") or "").strip():
                body["description"] = entry["description"].strip()
            known[tname] = put(om, "tables", body)
        log(f"  schema {schema!r}: {len(tables)} table(s), "
            f"{sum(len(c) for c in tables.values())} column(s)")
    return known


def main():
    om = om_session()
    if om is None:
        log("OpenMetadata is not running — skipping the catalog step. "
            "Start it with `docker compose --profile governance up -d`.")
        return

    st = load()
    contracts = load_contracts()
    ws_name = st["workspace_name"]

    # --- ontology: a domain, and the data product served out of it -----------
    # Consumer-aligned, because gold exists for a consumer outside the pipeline
    # (the semantic model, and Power BI behind it). Calling it source-aligned
    # would misdescribe who it is shaped for.
    put(om, "domains", {
        "name": DOMAIN, "displayName": "Contoso Sales", "domainType": "Consumer-aligned",
        "description": "Sales across Contoso's three selling and record-keeping "
                       "systems — POS, the web store, and the ERP — conformed to one "
                       "customer identity and one order-line grain."})
    put(om, "dataProducts", {
        "name": "contoso-sales-star", "displayName": "Contoso Sales star",
        "domains": [DOMAIN],
        "description": "The conformed star served from the Warehouse: a resolved "
                       "customer dimension, an order-line fact spanning both selling "
                       "channels, and the daily revenue aggregate the semantic model "
                       "reads. This is the layer downstream consumers may depend on."})
    log(f"ontology: domain {DOMAIN!r} + data product 'contoso-sales-star'")

    # --- glossary: the business vocabulary, straight out of the contracts ----
    # A term per contracted object, and a term per property that carries a
    # businessName — which is the contract author saying "this column has a
    # meaning a reader needs", so it is exactly the set worth publishing.
    gloss = put(om, "glossaries", {
        "name": GLOSSARY,
        "description": "Business vocabulary for the Contoso sales medallion, derived "
                       "from the ODCS contracts in examples/medallion-advanced-pyspark/"
                       "contracts/ — the contracts are the source of truth, this is "
                       "their projection into the catalog."})
    terms = 0
    for cname, contract in contracts.items():
        for entry in contract.get("schema", []) or []:
            desc = (entry.get("description") or "").strip()
            if not desc:
                continue
            put(om, "glossaryTerms", {
                "name": entry.get("businessName") or entry["name"],
                "glossary": GLOSSARY, "description": desc,
                "synonyms": [entry["name"]] if entry.get("businessName") else []})
            terms += 1
            for prop in entry.get("properties", []) or []:
                if not prop.get("businessName") or not (prop.get("description") or "").strip():
                    continue
                put(om, "glossaryTerms", {
                    "name": prop["businessName"], "glossary": GLOSSARY,
                    "description": prop["description"].strip(),
                    "synonyms": [prop["name"]]})
                terms += 1
    log(f"glossary {GLOSSARY!r}: {terms} business term(s) from {len(contracts)} contracts")

    # --- metrics: what gold measures ----------------------------------------
    for name, mtype, unit, grain, expr, desc in METRICS:
        put(om, "metrics", {
            "name": name, "description": desc, "metricType": mtype,
            "unitOfMeasurement": unit, "granularity": grain,
            "domains": [DOMAIN],
            "metricExpression": {"language": "SQL", "code": expr}})
    log(f"metrics: {len(METRICS)} measure(s) registered with their SQL")

    # --- data contracts: the rules, as rules ---------------------------------
    # This is the part a description string cannot do. OM's DataContract carries
    # ODCS quality rules natively, so `duplicateValues mustBe 0` arrives as a
    # structured expectation a catalog user can filter and a tool can read —
    # rather than prose saying the same thing.
    known = register_tables(om, st, ws_name)
    registered = rules_total = 0
    for table, tbl in sorted(known.items()):
        contract, entry = element(contracts, table)
        if not entry:
            continue
        rules = odcs_rules(entry)
        if not rules:
            continue
        put(om, "dataContracts", {
            "name": f"{table}-contract",
            "displayName": f"{contract.get('name', table)} — {entry['name']}",
            "entity": {"id": tbl["id"], "type": "table"},
            "domains": [DOMAIN],
            "description": (entry.get("description") or "").strip() or
                           f"ODCS contract governing {table}.",
            "odcsQualityRules": rules})
        registered += 1
        rules_total += len(rules)
    if registered:
        log(f"contracts: {registered} DataContract(s) carrying {rules_total} ODCS rule(s)")
    else:
        log("contracts: no catalogued tables matched a contract — run "
            "`docker compose run --rm govern-ingest` first to populate the catalog")

    # --- lineage: what the emulator recorded, with how it knows it ------------
    # Every edge carries its producer into the catalog. `Warehouse` means the
    # TDS front watched the engine accept the statement; `Reported` means a step
    # said so. A catalog that flattens those into one arrow has thrown away the
    # only thing that tells a consumer how much to trust the graph.
    fq = {name: t["fullyQualifiedName"] for name, t in known.items()}
    edges = S.get(f"{FABRIC}/v1/workspaces/{st['workspace']}/lineage",
                  headers=fabric_headers(), timeout=30).json().get("value", [])
    sent = skipped = 0
    by_producer = {}
    for e in edges:
        src, dst = e.get("sourcePath", ""), e.get("targetPath", "")
        if not src.startswith("Tables/") or not dst.startswith("Tables/"):
            skipped += 1  # a landing FILE has no table entity to point at
            continue
        s_name, d_name = src.split("/", 1)[1], dst.split("/", 1)[1]
        if s_name not in fq or d_name not in fq:
            skipped += 1
            continue
        producer = e.get("producer") or "Copy"
        r = om.put(f"{OM}/api/v1/lineage", timeout=30, json={"edge": {
            "fromEntity": {"id": known[s_name]["id"], "type": "table"},
            "toEntity": {"id": known[d_name]["id"], "type": "table"},
            "lineageDetails": {"description":
                               f"Fabric {producer} — {e.get('activityName') or 'pipeline'} "
                               f"(fabric-emulator)"}}})
        assert r.status_code in (200, 201), (r.status_code, r.text[:200])
        sent += 1
        by_producer[producer] = by_producer.get(producer, 0) + 1
    log(f"lineage: {sent} table→table edge(s) — "
        + ", ".join(f"{k}×{v}" for k, v in sorted(by_producer.items()))
        + f"; {skipped} skipped (file sources, or a table outside the catalog)")

    # --- assert it arrived ---------------------------------------------------
    # The step asserts its own result, like every other step here. A 201 means
    # OM accepted the write; only a read proves it is retrievable, and a
    # contract is addressed by an FQN namespaced under its table, not by name.
    d = om.get(f"{OM}/api/v1/domains/name/{DOMAIN}", timeout=20)
    assert d.status_code == 200, (d.status_code, d.text[:200])
    # `glossary` filters by ID, not by name — passing the name returns an empty
    # page with a 200, which reads exactly like "nothing was stored".
    g = om.get(f"{OM}/api/v1/glossaryTerms?glossary={gloss['id']}&limit=200", timeout=20)
    assert g.status_code == 200 and len(g.json().get("data", [])) >= terms, \
        f"glossary read back {len(g.json().get('data', []))} of {terms} terms"
    m = om.get(f"{OM}/api/v1/metrics?limit=100", timeout=20).json().get("data", [])
    got = {x["name"] for x in m}
    want = {name for name, *_ in METRICS}
    assert want <= got, f"metrics missing from the catalog: {want - got}"
    if registered:
        dc = om.get(f"{OM}/api/v1/dataContracts?fields=odcsQualityRules&limit=100",
                    timeout=20).json().get("data", [])
        back = sum(len(c.get("odcsQualityRules") or []) for c in dc)
        assert back >= rules_total, f"read back {back} rules, registered {rules_total}"
    # Lineage is read back through OM's own graph API, not from what we sent:
    # a 200 on the write says OM accepted an edge, only the graph says it can
    # be traversed. Gold is the interesting end — if the warehouse hops did not
    # land, the star looks like it came from nowhere.
    if sent:
        star = fq.get("fct_daily_revenue") or fq.get("fct_orders")
        gid = known["fct_daily_revenue" if "fct_daily_revenue" in fq else "fct_orders"]["id"]
        # /lineage/table/{id}, not /lineage/getLineage — the latter exists in
        # the spec and answers 500 here.
        lg = om.get(f"{OM}/api/v1/lineage/table/{gid}"
                    f"?upstreamDepth=3&downstreamDepth=1", timeout=30)
        assert lg.status_code == 200, (lg.status_code, lg.text[:200])
        ups = lg.json().get("upstreamEdges", [])
        assert ups, f"{star} has no upstream in OpenMetadata's graph"
        log(f"lineage verified: {star.split('.')[-1]} has {len(ups)} upstream edge(s) "
            f"in OM's graph")
    log(f"catalog verified by read-back: domain, {terms} terms, "
        f"{len(want)} metrics, {rules_total} contracted rules")


if __name__ == "__main__":
    main()
