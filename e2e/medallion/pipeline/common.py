"""Endpoints, tokens, and shared state for the medallion pipeline.

Every hop authenticates against entra-emulator with the seeded daemon service
principal, exactly as the tutorial (docs/28-tutorial-end-to-end.md) does — only
the endpoints differ (compose service names instead of localhost).
"""
import json
import os
import pathlib

import requests
import urllib3

urllib3.disable_warnings()

TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"  # seeded daemon SP
CLIENT_SECRET = "daemon-app-secret"  # intentionally public dev value

ENTRA = os.environ["ENTRA_URL"]
KV = os.environ["KV_URL"]
FABRIC = os.environ["FABRIC_REST_URL"]
TDS_SERVER = os.environ["TDS_SERVER"]
# The vault URI Fabric itself resolves an AKV reference against — an in-network
# address, since the emulator (not this client) performs that fetch.
KV_INTERNAL = os.environ.get("KV_INTERNAL_URL", KV)

FABRIC_AUD = "https://api.fabric.microsoft.com"
STORAGE_AUD = "https://storage.azure.com"
SQL_AUD = "https://database.windows.net"
VAULT_AUD = "https://vault.azure.net"
PBI_AUD = "https://analysis.windows.net/powerbi/api"

S = requests.Session()
S.verify = False

STATE = pathlib.Path(os.environ.get("PIPELINE_STATE", "/tmp/pipeline-state.json"))


def log(msg):
    print(f"==> {msg}", flush=True)


def ensure_app(app_id_uri, name):
    """Register a non-default audience in entra (409 = already there)."""
    r = S.post(f"{ENTRA}/admin/api/apps",
               json={"displayName": name, "appIdUri": app_id_uri, "isConfidential": False})
    assert r.status_code in (200, 201, 409), f"seed {app_id_uri}: {r.status_code} {r.text}"


def token(audience):
    r = S.post(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data={
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET, "scope": audience + "/.default"})
    r.raise_for_status()
    return r.json()["access_token"]


def fabric_headers():
    return {"Authorization": "Bearer " + token(FABRIC_AUD)}


def storage_options():
    """delta-rs storage options pointing at OneLake's account-prefixed path."""
    opts = {
        "azure_storage_account_name": "onelake",
        "azure_storage_token": token(STORAGE_AUD),
        "azure_endpoint": f"{FABRIC}/onelake",
    }
    if FABRIC.startswith("http://"):
        opts["azure_allow_http"] = "true"
    else:
        opts["azure_allow_invalid_certificates"] = "true"
    return opts


def tds_connect(database, token_value=None, timeout=60):
    """FedAuth over TDS: the pre-minted Azure-SQL token rides in
    SQL_COPT_SS_ACCESS_TOKEN (1256) — the same injection dbt-fabric performs, so
    the ODBC driver never runs MSAL. Encrypt=no because the TDS front terminates
    FedAuth without TLS (advertises ENCRYPT_NOT_SUP)."""
    import struct

    import pyodbc

    enc = (token_value or token(SQL_AUD)).encode("utf-16-le")
    return pyodbc.connect(
        "DRIVER={ODBC Driver 18 for SQL Server};"
        f"SERVER={TDS_SERVER};Database={database};"
        "Encrypt=no;TrustServerCertificate=yes",
        attrs_before={1256: struct.pack("<i", len(enc)) + enc}, timeout=timeout)


def save(**kv):
    state = load()
    state.update(kv)
    STATE.write_text(json.dumps(state, indent=2))


def load():
    return json.loads(STATE.read_text()) if STATE.exists() else {}
