"""The medallion pipeline, step by step.

Each function is one hop of the tutorial (docs/28-tutorial-end-to-end.md) and
asserts its own outcome, so a broken hop fails here rather than surfacing three
stages later as a wrong number.
"""
import base64
import datetime
import io
import json
import os
import subprocess
import time

import pandas as pd
from deltalake import DeltaTable, write_deltalake

import source_system as src
from common import (FABRIC, FABRIC_AUD, KV, KV_INTERNAL, PBI_AUD, S, SQL_AUD, STORAGE_AUD,
                    TDS_SERVER, VAULT_AUD, ensure_app, fabric_headers, load, log, save,
                    storage_options, tds_connect, token)


# --- 1. provision -----------------------------------------------------------
def provision():
    """Workspace + lakehouse + warehouse + a provisioned workspace identity."""
    H = fabric_headers()
    ws = S.post(f"{FABRIC}/v1/workspaces", headers=H,
                json={"displayName": "contoso-analytics"})
    assert ws.status_code == 201, f"workspace: {ws.status_code} {ws.text}"
    ws = ws.json()
    lh = S.post(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", headers=H,
                json={"displayName": "lake"})
    assert lh.status_code in (200, 201, 202), f"lakehouse: {lh.status_code} {lh.text}"
    wh = S.post(f"{FABRIC}/v1/workspaces/{ws['id']}/warehouses", headers=H,
                json={"displayName": "dw"})
    assert wh.status_code in (200, 201, 202), f"warehouse: {wh.status_code} {wh.text}"

    # The workspace identity is what fetches the Key Vault secret on Fabric's
    # side when the AKV-reference connection resolves.
    r = S.post(f"{FABRIC}/v1/workspaces/{ws['id']}/provisionIdentity", headers=H)
    assert r.status_code in (200, 202), f"provisionIdentity: {r.status_code} {r.text}"

    save(workspace=ws["id"], lakehouse=lh.json()["id"], warehouse=wh.json()["id"])
    log(f"provisioned workspace={ws['id']} lakehouse={lh.json()['id']} warehouse={wh.json()['id']}")


# --- 2. Key Vault -----------------------------------------------------------
def store_secret():
    """Put the source system's API key in Key Vault and bind it to Fabric as an
    AzureKeyVaultReference connection — which makes Fabric resolve it for real
    with a vault-audience workspace-identity token."""
    ensure_app(VAULT_AUD, "Azure Key Vault")
    vt = token(VAULT_AUD)
    r = S.put(f"{KV}/secrets/contoso-pos-api-key?api-version=7.4",
              headers={"Authorization": "Bearer " + vt}, json={"value": src.API_KEY})
    assert r.status_code in (200, 201), f"put secret: {r.status_code} {r.text}"

    st = load()
    r = S.post(f"{FABRIC}/v1/connections", headers=fabric_headers(), json={
        "displayName": "contoso-pos",
        "connectivityType": "ShareableCloud",
        "connectionDetails": {"type": "RestApi", "path": "https://pos.contoso.example/v2/export"},
        "credentialDetails": {"credentials": {
            "credentialType": "AzureKeyVaultReference",
            "workspaceId": st["workspace"],
            "vaultUri": KV_INTERNAL,
            "secretName": "contoso-pos-api-key"}}})
    assert r.status_code == 201, f"AKV connection: {r.status_code} {r.text}"
    conn_id = r.json()["id"]

    # Read it back: metadata only — the secret must never come back over the wire.
    listed = S.get(f"{FABRIC}/v1/connections", headers=fabric_headers())
    listed.raise_for_status()
    assert src.API_KEY not in listed.text, "connection listing leaked the secret value"
    row = next(c for c in listed.json()["value"] if c["id"] == conn_id)
    assert row["credentialDetails"]["credentialType"] == "AzureKeyVaultReference", row
    save(connection=conn_id)
    log(f"secret stored + AKV-reference connection {conn_id} (no secret in the read shape)")


# --- 3. extract → landing ---------------------------------------------------
def extract_to_landing():
    """Fetch the key from Key Vault (as notebookutils.credentials.getSecret
    does), pull the vendor export, and land it verbatim in OneLake."""
    vt = token(VAULT_AUD)
    r = S.get(f"{KV}/secrets/contoso-pos-api-key?api-version=7.4",
              headers={"Authorization": "Bearer " + vt})
    r.raise_for_status()
    api_key = r.json()["value"]

    # The vendor refuses a wrong key — prove the gate is real, not decorative.
    try:
        src.export("wrong-key")
        raise AssertionError("source system accepted a wrong API key")
    except PermissionError:
        pass

    ensure_app(STORAGE_AUD, "Azure Storage")
    st_tok = token(STORAGE_AUD)
    st = load()
    today = datetime.date.today().isoformat()
    for name, blob in src.export(api_key).items():
        path = f"Files/landing/contoso_pos/{today}/{name}"
        r = S.put(f"{FABRIC}/onelake/{st['workspace']}/{st['lakehouse']}/{path}",
                  data=blob, headers={"Authorization": "Bearer " + st_tok,
                                      "x-ms-blob-type": "BlockBlob"})
        assert r.status_code in (200, 201), f"land {path}: {r.status_code} {r.text}"
        log(f"landed {path} ({len(blob)} bytes)")
    save(landing_date=today)


# --- 4. bronze --------------------------------------------------------------
def bronze():
    """Append landing verbatim into Delta, with lineage columns. Bronze keeps
    everything — duplicates and malformed rows included."""
    st = load()
    opts = storage_options()

    def read_landing(name):
        path = f"Files/landing/contoso_pos/{st['landing_date']}/{name}"
        r = S.get(f"{FABRIC}/onelake/{st['workspace']}/{st['lakehouse']}/{path}",
                  headers={"Authorization": "Bearer " + token(STORAGE_AUD)})
        r.raise_for_status()
        return path, r.content

    def stamp(df, source_path):
        df["_source_path"] = source_path
        df["_landing_date"] = st["landing_date"]
        return df

    path, raw = read_landing("customers.csv")
    customers = stamp(pd.read_csv(io.BytesIO(raw)), path)
    path, raw = read_landing("orders.jsonl")
    orders = stamp(pd.DataFrame(json.loads(l) for l in raw.decode().splitlines()), path)

    base = f"az://{st['workspace']}/{st['lakehouse']}/Tables"
    write_deltalake(f"{base}/bronze_customers", customers, mode="append", storage_options=opts)
    write_deltalake(f"{base}/bronze_orders", orders, mode="append", storage_options=opts)

    assert len(customers) == src.EXPECTED_BRONZE_CUSTOMERS, len(customers)
    assert len(orders) == src.EXPECTED_BRONZE_ORDERS, len(orders)
    log(f"bronze: {len(customers)} customer rows, {len(orders)} order events")


# --- 5. silver --------------------------------------------------------------
COUNTRY = {"US": "US", "USA": "US", "GB": "GB", "U.K.": "GB", "SG": "SG"}


def silver():
    """Dedupe, conform, quarantine — the rules bronze deliberately does not apply."""
    st = load()
    opts = storage_options()
    base = f"az://{st['workspace']}/{st['lakehouse']}/Tables"

    def read(table):
        return DeltaTable(f"{base}/{table}", storage_options=opts).to_pandas()

    c = read("bronze_customers").drop_duplicates(subset=["customer_id"], keep="last").copy()
    c["email"] = c["email"].str.lower()
    c["country"] = c["country"].str.upper().str.strip().map(lambda v: COUNTRY.get(v, v))
    silver_customers = c[["customer_id", "name", "email", "country"]]

    o = read("bronze_orders").sort_values("event_seq")
    o = o.drop_duplicates(subset=["order_id"], keep="last").copy()  # latest event wins
    bad = (o["quantity"] <= 0) | o["unit_price"].isna()
    quarantine = o[bad].copy()
    o = o[~bad].copy()
    o["order_date"] = pd.to_datetime(o["order_date"])
    o["amount"] = o["quantity"] * o["unit_price"]
    silver_orders = o[["order_id", "customer_id", "order_date", "quantity",
                       "unit_price", "amount", "status"]]

    write_deltalake(f"{base}/silver_customers", silver_customers, mode="overwrite",
                    storage_options=opts)
    write_deltalake(f"{base}/silver_orders", silver_orders, mode="overwrite",
                    storage_options=opts)
    write_deltalake(f"{base}/silver_quarantine_orders", quarantine, mode="overwrite",
                    storage_options=opts)

    assert len(silver_customers) == src.EXPECTED_SILVER_CUSTOMERS, len(silver_customers)
    assert len(silver_orders) == src.EXPECTED_SILVER_ORDERS, len(silver_orders)
    assert len(quarantine) == src.EXPECTED_QUARANTINED, len(quarantine)
    assert set(silver_customers["country"]) == src.EXPECTED_COUNTRIES, set(silver_customers["country"])
    assert silver_orders["order_id"].is_unique, "silver_orders still has duplicate order ids"
    log(f"silver: {len(silver_customers)} customers, {len(silver_orders)} orders, "
        f"{len(quarantine)} quarantined")


# --- 6. reflection ----------------------------------------------------------
def reflect():
    """Connecting to the lakehouse database reflects its Delta into the SQL
    engine — silver becomes queryable T-SQL. Retries while the per-item database
    is created and brought online on first connect."""
    st = load()
    ensure_app(SQL_AUD, "Azure SQL")
    sql_tok = token(SQL_AUD)
    last = None
    for attempt in range(40):
        try:
            with tds_connect(st["lakehouse"], sql_tok, timeout=15) as c:
                rows = c.cursor().execute(
                    "SELECT status, COUNT(*) AS n, SUM(amount) AS revenue "
                    "FROM silver_orders GROUP BY status").fetchall()
            break
        except Exception as e:  # noqa: BLE001 — transient while the DB comes online
            last = e
            time.sleep(3)
    else:
        raise AssertionError(f"lakehouse reflection failed after retries: {last}")

    total = sum(float(r[2]) for r in rows)
    count = sum(int(r[1]) for r in rows)
    assert count == src.EXPECTED_SILVER_ORDERS, rows
    assert abs(total - src.EXPECTED_REVENUE) < 0.01, rows
    log(f"reflection: {[tuple(r) for r in rows]} (attempt {attempt + 1})")


# --- 7. gold with dbt -------------------------------------------------------
def gold(project_dir):
    """Microsoft's real dbt-fabric builds gold in the Warehouse over TDS, and
    `dbt build` runs the data-quality tests as part of the same graph."""
    st = load()
    sql_tok = token(SQL_AUD)

    # Warm the warehouse database the same way the lakehouse one was warmed.
    last = None
    for _ in range(40):
        try:
            with tds_connect(st["warehouse"], sql_tok, timeout=15) as c:
                assert c.cursor().execute("SELECT 1").fetchone()[0] == 1
            break
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(3)
    else:
        raise AssertionError(f"warehouse warmup failed: {last}")

    with open(os.path.join(project_dir, "profiles.yml"), "w") as f:
        f.write(f"""contoso_gold:
  target: dev
  outputs:
    dev:
      type: fabric
      driver: "ODBC Driver 18 for SQL Server"
      server: "{TDS_SERVER}"
      database: "{st['warehouse']}"
      schema: "dbo"
      authentication: "ActiveDirectoryAccessToken"
      access_token: "{sql_tok}"
      access_token_expires_on: 0
      encrypt: false
      trust_cert: true
      threads: 1
""")
    env = {**os.environ, "DBT_PROFILES_DIR": project_dir, "LAKEHOUSE_ID": st["lakehouse"]}
    rc = subprocess.run(["dbt", "build"], cwd=project_dir, env=env).returncode
    assert rc == 0, f"dbt build failed: exit {rc}"

    with tds_connect(st["warehouse"], sql_tok) as c:
        revenue = c.cursor().execute("SELECT SUM(revenue) FROM fct_daily_revenue").fetchone()[0]
        orders = c.cursor().execute("SELECT COUNT(*) FROM fct_orders").fetchone()[0]
    assert abs(float(revenue) - src.EXPECTED_REVENUE) < 0.01, revenue
    assert int(orders) == src.EXPECTED_SILVER_ORDERS, orders
    log(f"gold: dbt build green, revenue={float(revenue):.2f} over {orders} orders")


def gold_tests_catch_bad_data(project_dir):
    """The DQ bar is only real if it fails on bad data: push a duplicate +
    negative-amount order into silver, rebuild, and require dbt to reject it.
    Then restore silver so downstream steps see clean data."""
    st = load()
    opts = storage_options()
    base = f"az://{st['workspace']}/{st['lakehouse']}/Tables"
    good = DeltaTable(f"{base}/silver_orders", storage_options=opts).to_pandas()

    poisoned = pd.concat([good, good.head(1).assign(amount=-5.0)], ignore_index=True)
    write_deltalake(f"{base}/silver_orders", poisoned, mode="overwrite", storage_options=opts)
    try:
        with tds_connect(st["lakehouse"]):  # re-reflect the poisoned table
            pass
        env = {**os.environ, "DBT_PROFILES_DIR": project_dir, "LAKEHOUSE_ID": st["lakehouse"]}
        rc = subprocess.run(["dbt", "build"], cwd=project_dir, env=env).returncode
        assert rc != 0, "dbt build passed on data that violates the gold contract"
        log("DQ gate verified: dbt build fails on a duplicate + negative-amount order")
    finally:
        write_deltalake(f"{base}/silver_orders", good, mode="overwrite", storage_options=opts)
        with tds_connect(st["lakehouse"]):  # re-reflect the clean table
            pass
        env = {**os.environ, "DBT_PROFILES_DIR": project_dir, "LAKEHOUSE_ID": st["lakehouse"]}
        assert subprocess.run(["dbt", "build"], cwd=project_dir, env=env).returncode == 0, \
            "gold did not return to green after restoring silver"


# --- 8. semantic model ------------------------------------------------------
def semantic_model():
    """Publish gold as a SemanticModel item (TMSL + rows) and query it over the
    Power BI executeQueries wire — the readiness check for Power BI clients."""
    st = load()
    H = fabric_headers()

    with tds_connect(st["warehouse"]) as c:
        cur = c.cursor().execute(
            "SELECT order_date, country, orders, units, revenue FROM fct_daily_revenue")
        fact = [{"OrderDate": str(r[0])[:10], "Country": r[1], "Orders": int(r[2]),
                 "Units": int(r[3]), "Revenue": float(r[4])} for r in cur.fetchall()]
        cur = c.cursor().execute("SELECT customer_id, name, country FROM dim_customer")
        dim = [{"CustomerId": r[0], "Name": r[1], "Country": r[2]} for r in cur.fetchall()]
    assert fact and dim, (fact, dim)

    model = {
        "name": "ContosoRevenue",
        "compatibilityLevel": 1550,
        "model": {
            "culture": "en-US",
            "tables": [
                {"name": "Customer", "columns": [
                    {"name": "CustomerId", "dataType": "string", "sourceColumn": "CustomerId"},
                    {"name": "Name", "dataType": "string", "sourceColumn": "Name"},
                    {"name": "Country", "dataType": "string", "sourceColumn": "Country"}]},
                {"name": "Revenue", "columns": [
                    {"name": "OrderDate", "dataType": "string", "sourceColumn": "OrderDate"},
                    {"name": "Country", "dataType": "string", "sourceColumn": "Country"},
                    {"name": "Orders", "dataType": "int64", "sourceColumn": "Orders"},
                    {"name": "Units", "dataType": "int64", "sourceColumn": "Units"},
                    {"name": "Revenue", "dataType": "double", "sourceColumn": "Revenue"}],
                 "measures": [
                     {"name": "Total Revenue", "expression": "SUM(Revenue[Revenue])"},
                     {"name": "Total Units", "expression": "SUM(Revenue[Units])"},
                     {"name": "Revenue per Unit",
                      "expression": "DIVIDE([Total Revenue], [Total Units])"}]}],
            "relationships": [
                {"name": "Revenue_Customer", "fromTable": "Revenue", "fromColumn": "Country",
                 "toTable": "Customer", "toColumn": "Country"}]},
    }
    data = {"Customer": dim, "Revenue": fact}

    def part(path, obj):
        return {"path": path, "payloadType": "InlineBase64",
                "payload": base64.b64encode(json.dumps(obj).encode()).decode()}

    r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/items", headers=H, json={
        "displayName": "ContosoRevenue", "type": "SemanticModel",
        "definition": {"parts": [part("model.bim", model), part("data.json", data)]}})
    assert r.status_code in (201, 202), f"publish model: {r.status_code} {r.text}"
    if r.status_code == 201:
        dataset = r.json()["id"]
    else:
        op = r.headers["x-ms-operation-id"]
        for _ in range(60):
            status = S.get(f"{FABRIC}/v1/operations/{op}", headers=H).json()["status"]
            if status in ("Succeeded", "Failed"):
                break
            time.sleep(1)
        assert status == "Succeeded", f"publish operation {status}"
        dataset = S.get(f"{FABRIC}/v1/operations/{op}/result", headers=H).json()["id"]
    save(dataset=dataset)

    # Query it exactly as a Power BI REST client (or SemPy) would.
    ensure_app(PBI_AUD, "Power BI Service")
    pt = token(PBI_AUD)
    dax = ('EVALUATE SUMMARIZECOLUMNS(Customer[Country], '
           '"Revenue", [Total Revenue], "PerUnit", [Revenue per Unit])')
    r = S.post(f"{FABRIC}/v1.0/myorg/groups/{st['workspace']}/datasets/{dataset}/executeQueries",
               headers={"Authorization": "Bearer " + pt}, json={"queries": [{"query": dax}]})
    assert r.status_code == 200, f"executeQueries: {r.status_code} {r.text}"
    rows = r.json()["results"][0]["tables"][0]["rows"]
    assert rows, r.text

    total = sum(row["[Revenue]"] for row in rows)
    assert abs(total - src.EXPECTED_REVENUE) < 0.01, rows
    countries = {row["Customer[Country]"] for row in rows}
    assert countries == src.EXPECTED_COUNTRIES, countries
    log(f"semantic model {dataset}: DAX over executeQueries → {rows}")

    # A Power BI token is required: the control-plane token must be refused.
    r = S.post(f"{FABRIC}/v1.0/myorg/groups/{st['workspace']}/datasets/{dataset}/executeQueries",
               headers={"Authorization": "Bearer " + token(FABRIC_AUD)},
               json={"queries": [{"query": dax}]})
    assert r.status_code == 401, f"wrong-audience token was accepted: {r.status_code}"
    log("executeQueries rejects a non-Power BI audience token (401)")
