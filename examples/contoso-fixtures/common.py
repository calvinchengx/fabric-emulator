"""Endpoints, tokens, and shared state for the medallion example.

Every hop authenticates against entra-emulator with the seeded daemon service
principal — the same trust relationships as production Azure.

Endpoints default to the local developer stack (`docker compose up`, self-signed
TLS on localhost) and are overridable by environment variable, so the same code
runs unchanged inside a container against compose service names. That is how
`e2e/medallion` drives this example in CI.
"""
import json
import os
import pathlib

import requests
import urllib3

urllib3.disable_warnings()  # the family serves self-signed TLS

TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"  # seeded daemon SP
CLIENT_SECRET = "daemon-app-secret"  # intentionally public dev value

ENTRA = os.environ.get("ENTRA_URL", "https://localhost:8443")
KV = os.environ.get("KV_URL", "https://localhost:8444")
FABRIC = os.environ.get("FABRIC_REST_URL", "https://localhost:9443")
TDS_SERVER = os.environ.get("TDS_SERVER", "localhost,1433")
# Fabric resolves an AKV reference server-side, so the vault URI it stores must
# be reachable from the *emulator*, which is not always where you are. Running
# these steps on your machine against `docker compose up`, `localhost:8444` is
# the vault as *you* reach it — the emulator container cannot follow it back
# out. So the default is the compose service name, which is correct whether the
# step runs on the host or in a container alongside it. Override for any other
# topology (the CI harness points it at plain HTTP; a bare-metal vault at your
# own address).
KV_INTERNAL = os.environ.get("KV_INTERNAL_URL", "https://keyvault-emulator:8444")

# The Spark engine 04_engine.py drives the queued notebook run onto. Default is
# Sail as `docker compose up` publishes it; the CI harness uses the service name.
SPARK_REMOTE = os.environ.get("SPARK_REMOTE", "sc://localhost:50051")

FABRIC_AUD = "https://api.fabric.microsoft.com"
STORAGE_AUD = "https://storage.azure.com"
SQL_AUD = "https://database.windows.net"
VAULT_AUD = "https://vault.azure.net"
PBI_AUD = "https://analysis.windows.net/powerbi/api"

S = requests.Session()
S.verify = False

# Anchored on the CALLING example's directory, not on this file's. This module
# is shared by both medallion examples, so `HERE` would resolve to the fixture
# package — and state.json would be written inside site-packages, with every
# example silently sharing one. Each example is run from its own directory, so
# the working directory is the right anchor.
HERE = pathlib.Path.cwd()
STATE = pathlib.Path(os.environ.get("PIPELINE_STATE", HERE / "state.json"))
GOLD_PROJECT = os.environ.get("GOLD_PROJECT", str(HERE / "gold"))
# Display names are unique per emulator, so two examples provisioning against
# one stack must not both ask for "contoso-analytics". examples/medallion-spark
# overrides this; nothing else needs to.
WORKSPACE_NAME = os.environ.get("WORKSPACE_NAME", "contoso-analytics")


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
    """delta-rs options pointing at OneLake's account-prefixed Blob path."""
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


def tables_uri():
    st = load()
    return f"az://{st['workspace']}/{st['lakehouse']}/Tables"


def tds_connect(database, token_value=None, timeout=60):
    """FedAuth over TDS: the pre-minted Azure-SQL token rides in
    SQL_COPT_SS_ACCESS_TOKEN (1256) — the exact injection dbt-fabric performs, so
    the ODBC driver never runs MSAL. Encrypt=no because the TDS front terminates
    FedAuth without TLS (it advertises ENCRYPT_NOT_SUP)."""
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


# --- pipeline orchestration --------------------------------------------------
# The emulator executes DataPipeline activities for real: a Copy moves the data
# itself. A Notebook activity is different — the emulator parses the notebook
# into cells and records a Pending run, then an ENGINE executes those cells and
# reports back. That split is why these helpers exist.

def create_item(display_name, item_type, parts):
    """Create an item with a definition, resolving the LRO if the create is async."""
    import base64
    import time

    H = fabric_headers()
    st = load()
    body = {"displayName": display_name, "type": item_type, "definition": {"parts": [
        {"path": p, "payloadType": "InlineBase64",
         "payload": base64.b64encode(c.encode() if isinstance(c, str) else c).decode()}
        for p, c in parts.items()]}}
    r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/items", headers=H, json=body)
    assert r.status_code in (201, 202), f"create {item_type}: {r.status_code} {r.text}"
    if r.status_code == 201:
        return r.json()["id"]
    op = r.headers["x-ms-operation-id"]
    for _ in range(60):
        status = S.get(f"{FABRIC}/v1/operations/{op}", headers=H).json()["status"]
        if status in ("Succeeded", "Failed"):
            break
        time.sleep(1)
    assert status == "Succeeded", f"create {item_type}: operation {status}"
    return S.get(f"{FABRIC}/v1/operations/{op}/result", headers=H).json()["id"]


def run_job(item_id, job_type, body=None):
    """Start a job and wait for it to reach a terminal state. Returns (job_id, status)."""
    import time

    H = fabric_headers()
    st = load()
    base = f"{FABRIC}/v1/workspaces/{st['workspace']}/items/{item_id}/jobs/instances"
    r = S.post(f"{base}?jobType={job_type}", headers=H, json=body)
    assert r.status_code in (200, 202), f"run {job_type}: {r.status_code} {r.text}"
    jid = r.headers["Location"].rsplit("/", 1)[-1]
    for _ in range(120):
        body = S.get(f"{base}/{jid}", headers=H).json()
        status = body.get("status")
        if status in ("Completed", "Failed", "Cancelled"):
            if status == "Failed":
                # A bare "Failed" is useless to a reader. Surface which activity
                # broke and why, which the interpreter already recorded.
                detail = body.get("failureReason", {})
                try:
                    for r in activity_runs(item_id, jid):
                        if r.get("status") != "Succeeded":
                            log(f"activity {r.get('activityName')!r} {r.get('status')}: {r.get('error')}")
                except Exception:  # noqa: BLE001 — diagnostics must not mask the failure
                    pass
                log(f"{job_type} job {jid} failed: {detail}")
            return jid, status
        time.sleep(1)
    raise SystemExit(f"{job_type} job {jid} never reached a terminal state")


def activity_runs(item_id, job_id):
    """The per-activity results the interpreter recorded for a pipeline run."""
    st = load()
    r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/items/{item_id}"
               f"/jobs/instances/{job_id}/queryactivityruns", headers=fabric_headers(), json={})
    r.raise_for_status()
    return r.json().get("value", r.json())


def lineage_edges():
    """Every data-movement edge the emulator recorded for this workspace."""
    st = load()
    r = S.get(f"{FABRIC}/v1/workspaces/{st['workspace']}/lineage", headers=fabric_headers())
    r.raise_for_status()
    return r.json().get("value", [])
