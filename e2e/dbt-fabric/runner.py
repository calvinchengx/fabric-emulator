#!/usr/bin/env python3
"""In-container driver: provision a workspace + warehouse in fabric-emulator,
mint tokens from entra-emulator, template profiles.yml, then run Microsoft's
real dbt-fabric adapter through debug -> seed -> run -> test against the
emulator's TDS warehouse surface via the Microsoft ODBC Driver 18.

Two token audiences are needed: the Fabric REST provisioning calls take a
control-plane token (`api.fabric.microsoft.com`); the TDS connection takes an
Azure-SQL token (`database.windows.net`) — the FedAuth audience the emulator's
TDS front validates. dbt-fabric's `ActiveDirectoryAccessToken` mode injects that
pre-minted token straight into pyodbc's access-token attribute, so the ODBC
driver performs FedAuth without ever running MSAL (no authority redirect).
"""
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

ENTRA = os.environ["ENTRA_URL"]  # http://entra-emulator:8443
FABRIC_REST = os.environ["FABRIC_REST_URL"]  # http://fabric-emulator:80
TDS_SERVER = os.environ["TDS_SERVER"]  # fabric-emulator,1433
DAEMON_CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
REST_AUDIENCE = "https://api.fabric.microsoft.com"
TDS_AUDIENCE = "https://database.windows.net"  # the FedAuth audience the TDS front validates

PROJECT = "/project"


def log(msg):
    print(f"==> {msg}", flush=True)


def http(method, url, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, {"error": e.read().decode(errors="replace")}


def warmup(database, tds_token):
    """Prove the ODBC Driver 18 -> emulator TDS -> SQL Server relay end to end
    with a bare SELECT 1, retrying while the per-item database is created and
    brought online. Uses the same access-token injection dbt does."""
    import struct
    import time

    import pyodbc

    # SQL_COPT_SS_ACCESS_TOKEN (1256): 4-byte length + UTF-16-LE token (JWT is
    # ASCII, so utf-16-le matches dbt's byte-interleave).
    enc = tds_token.encode("utf-16-le")
    attrs = {1256: struct.pack("<i", len(enc)) + enc}
    base = (
        "DRIVER={ODBC Driver 18 for SQL Server};"
        f"SERVER={TDS_SERVER};Database={database};"
        "Encrypt=no;TrustServerCertificate=yes"
    )
    last = None
    for i in range(40):
        try:
            with pyodbc.connect(base, attrs_before=attrs, timeout=15) as c:
                if c.cursor().execute("SELECT 1").fetchone()[0] == 1:
                    log(f"warmup OK: ODBC Driver 18 -> TDS -> SQL Server SELECT 1 (attempt {i + 1})")
                    break
        except Exception as e:  # noqa: BLE001 — transient while the DB comes online
            last = e
            time.sleep(3)
    else:
        sys.exit(f"warmup failed after retries; last error: {last}")


def mint(audience):
    status, tok = http("POST", f"{ENTRA}/admin/api/tokens",
                       body={"clientId": DAEMON_CLIENT_ID, "audience": audience})
    token = tok.get("accessToken") or tok.get("token")
    if status // 100 != 2 or not token:
        sys.exit(f"token forge failed ({audience}): {status} {tok}")
    return token


def main():
    # 1) Provision a workspace (creator -> Admin) + a Warehouse item over REST.
    rest_token = mint(REST_AUDIENCE)
    log("creating workspace + warehouse")
    status, ws = http("POST", f"{FABRIC_REST}/v1/workspaces", rest_token,
                      {"displayName": "dbt_fabric_e2e"})
    if status // 100 != 2:
        sys.exit(f"workspace create failed: {status} {ws}")
    wsid = ws["id"]
    status, wh = http("POST", f"{FABRIC_REST}/v1/workspaces/{wsid}/warehouses", rest_token,
                      {"displayName": "e2e_wh"})
    if status // 100 != 2:
        sys.exit(f"warehouse create failed: {status} {wh}")
    whid = wh["id"]
    log(f"workspace={wsid} warehouse={whid}")

    # 2) Mint the Azure-SQL (FedAuth) token dbt injects into the ODBC driver.
    tds_token = mint(TDS_AUDIENCE)

    # 3) Template profiles.yml. ActiveDirectoryAccessToken => the token rides in
    #    pyodbc attrs_before, no MSAL. encrypt=false: the TDS front advertises
    #    ENCRYPT_NOT_SUP (terminates FedAuth without TLS), matching go-mssqldb's
    #    encrypt=disable path.
    profile = f"""dbt_fabric_e2e:
  target: dev
  outputs:
    dev:
      type: fabric
      driver: "ODBC Driver 18 for SQL Server"
      server: "{TDS_SERVER}"
      database: "{whid}"
      schema: "dbo"
      authentication: "ActiveDirectoryAccessToken"
      access_token: "{tds_token}"
      access_token_expires_on: 0
      encrypt: false
      trust_cert: true
      threads: 1
"""
    os.makedirs(PROJECT, exist_ok=True)
    with open(os.path.join(PROJECT, "profiles.yml"), "w") as f:
        f.write(profile)

    # 3b) Warm up the connection: the first connect makes the emulator create +
    #     start the per-item database on the sidecar, which can be slow. Prove
    #     the ODBC Driver 18 -> TDS -> SQL Server relay works for a bare SELECT 1
    #     (retrying while the database comes online) so dbt then runs against a
    #     warm database.
    warmup(whid, tds_token)

    # 4) Drive the real dbt through the emulator's TDS front + ODBC Driver 18.
    env = {**os.environ, "DBT_PROFILES_DIR": PROJECT}
    for cmd in (["dbt", "debug"], ["dbt", "seed", "--full-refresh"], ["dbt", "run"], ["dbt", "test"]):
        log(" ".join(cmd))
        rc = subprocess.run(cmd, cwd=PROJECT, env=env).returncode
        if rc != 0:
            sys.exit(f"stage failed ({' '.join(cmd)}): exit {rc}")
    log("all dbt stages passed")


if __name__ == "__main__":
    main()
