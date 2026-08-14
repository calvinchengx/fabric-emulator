#!/usr/bin/env python3
"""Microsoft's own `az` CLI, unmodified, driving Fabric REST and the Power BI
admin activity log against this fabric-emulator.

entra-emulator is login.microsoftonline.com and fabric-emulator is
api.fabric.microsoft.com (compose aliases, :443) because MSAL drops a
non-443 port from the authority — the same constraint the Az.Accounts
deployment-pipeline job measured.

`az login` still lists subscriptions even with `--allow-no-subscriptions`.
A JSON 404 from entra's tenant-prefixed path (docs/23, enough for Az.Accounts
`-SkipContextPopulation`) is `(not_found) No such API route` here. A one-route
HTTPS stub answers `GET /subscriptions` with `{"value":[]}` — the same stub
entra's own az-cli job runs. It is not a witness of ARM.

    python3 /driver.py   # inside the azure-cli image
"""
from __future__ import annotations

import base64
import json
import os
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
from datetime import UTC, datetime, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import NoReturn

TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
SP_CLIENT = "00d88624-f0d7-46f6-a641-6232c2608928"
SP_SECRET = "daemon-app-secret"
CLOUD = "FabricEmulatorCloud"
FABRIC = "https://api.fabric.microsoft.com"
FABRIC_AUD = "https://api.fabric.microsoft.com"
PBI_AUD = "https://analysis.windows.net/powerbi/api"
CAPACITY = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
AUTHORITY = "https://login.microsoftonline.com"

AZ_ENV = {
    **os.environ,
    "AZURE_CONFIG_DIR": str(Path(tempfile.mkdtemp(prefix="az-rest-config."))),
    # Private-cloud switch the CLI documents. Without it MSAL probes
    # login.microsoftonline.com's instance-discovery document — which in this
    # network IS entra, but the flag is the documented one rather than a bet
    # that entra's discovery doc matches whatever this CLI release expects.
    "AZURE_CORE_INSTANCE_DISCOVERY": "false",
    "AZURE_CORE_COLLECT_TELEMETRY": "0",
}

_ARM_STUB: ThreadingHTTPServer | None = None


def fail(msg: str) -> NoReturn:
    sys.exit(f"FAIL: {msg}")


def mint_localhost_tls(dir: Path) -> tuple[Path, Path]:
    """Leaf + key for the ARM stub. openssl ships in the azure-cli image."""
    cert_path, key_path = dir / "stub.pem", dir / "stub.key"
    r = subprocess.run(
        ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-sha256", "-days", "1",
         "-nodes", "-subj", "/CN=localhost",
         "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1",
         "-keyout", str(key_path), "-out", str(cert_path)],
        capture_output=True, text=True)
    if r.returncode != 0 or not cert_path.is_file():
        fail(f"openssl mint localhost cert: {(r.stderr or r.stdout)[:1500]}")
    return cert_path, key_path


class _ArmStub(BaseHTTPRequestHandler):
    """Enough ARM for `az login` to finish. Not a witness of ARM.

    `--allow-no-subscriptions` still lists subscriptions first. A connection
    refused or entra's JSON 404 (`No such API route`) crashes the CLI
    before the flag can apply. An empty list is the honest answer: this
    cloud has no ARM. Same stub entra's az-cli job runs.
    """

    def log_message(self, fmt, *args):
        return

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path.startswith("/subscriptions"):
            body = b'{"value":[]}'
        elif path.startswith("/metadata/endpoints"):
            port = self.server.server_address[1]
            body = json.dumps({
                "authentication": {
                    "loginEndpoint": AUTHORITY + "/",
                    "audiences": [
                        "https://management.core.windows.net/",
                        "https://management.azure.com/",
                    ],
                },
                "graphEndpoint": AUTHORITY + "/",
                "resourceManager": f"https://127.0.0.1:{port}",
            }).encode()
        else:
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def start_arm_stub(cert: Path, key: Path) -> ThreadingHTTPServer:
    srv = ThreadingHTTPServer(("127.0.0.1", 0), _ArmStub)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(cert, key)
    srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def harvest_cert(host: str, timeout: float = 90) -> str:
    """The emulator's leaf, so az verifies TLS instead of turning it off."""
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        try:
            pem = ssl.get_server_certificate((host, 443))
            if "BEGIN CERTIFICATE" in pem:
                return pem
        except OSError as e:
            last = str(e)
        time.sleep(1)
    fail(f"{host}:443 never presented a certificate ({last})")


def az(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    cmd = ["az", *args]
    print(f"    $ {' '.join(cmd)}", flush=True)
    r = subprocess.run(cmd, env=AZ_ENV, capture_output=True, text=True)
    if check and r.returncode != 0:
        fail(f"{' '.join(cmd)}\n{(r.stderr or r.stdout)[:2000]}")
    return r


def parse_json(raw: str):
    """Decode JSON. Empty / JSON null (az rest on a 202 with no body) → {}.
    az rest wraps a 4xx as `ERROR: Conflict({...})` — pull the object out."""
    text = (raw or "").strip()
    if not text or text in ("null", "None"):
        return {}
    try:
        val = json.loads(text)
    except json.JSONDecodeError:
        start, end = text.find("{"), text.rfind("}")
        if start < 0 or end <= start:
            return None
        try:
            val = json.loads(text[start:end + 1])
        except json.JSONDecodeError:
            return None
    return {} if val is None else val


def unwrap_error(blob) -> dict:
    """az rest wraps a 4xx body; Fabric's envelope is either bare or nested."""
    if not isinstance(blob, dict):
        return {}
    err = blob.get("error")
    if isinstance(err, dict):
        return err
    return blob


def az_rest(method: str, url: str, body=None, resource: str = FABRIC_AUD,
            allow_error: bool = False):
    """One `az rest` call. Success → decoded JSON ({} if the body was empty).
    `allow_error` returns the error envelope rather than exiting, for the 409
    the name-reservation claim is about — az rest treats 4xx as failure."""
    cmd = ["az", "rest", "--method", method, "--uri", url,
           "--resource", resource, "-o", "json"]
    if body is not None:
        tmp = Path(tempfile.mkdtemp(prefix="az-rest-body.")) / "body.json"
        tmp.write_text(json.dumps(body), encoding="utf-8")
        cmd.extend(["--body", f"@{tmp}"])
    print(f"    $ az rest -m {method} {url}", flush=True)
    r = subprocess.run(cmd, env=AZ_ENV, capture_output=True, text=True)
    if r.returncode != 0:
        blob = parse_json(r.stderr)
        if blob is None:
            blob = parse_json(r.stdout)
        if allow_error:
            return unwrap_error(blob) if isinstance(blob, dict) else {
                "raw": (r.stderr or r.stdout)[:1500]}
        fail(f"az rest {method} {url}\n{(r.stderr or r.stdout)[:2000]}")
    blob = parse_json(r.stdout)
    if blob is None:
        fail(f"az rest {method} {url}: not JSON: {(r.stdout or r.stderr)[:500]}")
    return blob


def v1(path: str) -> str:
    return f"{FABRIC}/v1/{path.lstrip('/')}"


def login() -> None:
    global _ARM_STUB
    print("-- 0. wait for TLS, trust the emulator leaves, register the cloud")
    entra_pem = harvest_cert("login.microsoftonline.com")
    fabric_pem = harvest_cert("api.fabric.microsoft.com")
    tls_dir = Path(tempfile.mkdtemp(prefix="az-rest-tls."))
    stub_cert, stub_key = mint_localhost_tls(tls_dir)
    _ARM_STUB = start_arm_stub(stub_cert, stub_key)
    arm = f"https://127.0.0.1:{_ARM_STUB.server_address[1]}"
    ca = Path(tempfile.mkdtemp(prefix="az-rest-ca.")) / "ca.pem"
    ca.write_text(entra_pem + "\n" + fabric_pem + "\n" + stub_cert.read_text(),
                  encoding="utf-8")
    AZ_ENV["REQUESTS_CA_BUNDLE"] = str(ca)
    AZ_ENV["SSL_CERT_FILE"] = str(ca)
    print(f"   CA bundle {ca} ({ca.stat().st_size} bytes); ARM stub {arm}")

    az("cloud", "unregister", "--name", CLOUD, check=False)
    # ResourceManagerUrl is mandatory. entra's SPA answers unknown ROOT GETs
    # with HTML (docs/23); Python `az login` then lists subscriptions and a
    # JSON 404 is still fatal. Point it at the local stub instead.
    az("cloud", "register", "--name", CLOUD,
       "--skip-endpoint-discovery",
       "--endpoint-resource-manager", arm + "/",
       "--endpoint-active-directory", AUTHORITY + "/",
       "--endpoint-active-directory-resource-id", FABRIC_AUD,
       "--endpoint-microsoft-graph-resource-id", f"{AUTHORITY}/graph/")
    az("cloud", "set", "--name", CLOUD)

    print("-- 1. az login --service-principal against entra-emulator")
    az("login", "--service-principal", "-u", SP_CLIENT, "-p", SP_SECRET,
       "--tenant", TENANT, "--allow-no-subscriptions")
    tok = az("account", "get-access-token", "--resource", FABRIC_AUD, "-o", "json")
    payload = json.loads(tok.stdout)
    if not payload.get("accessToken"):
        fail(f"no Fabric token: {payload}")
    print("   the CLI holds a Fabric-audience token")


def find_named(items: list, name: str, field: str = "displayName") -> dict:
    for it in items:
        if it.get(field) == name:
            return it
    fail(f"{name!r} not in {[i.get(field) for i in items]}")


def poll_item(list_url: str, name: str, field: str = "displayName",
              envelope: str = "value", tries: int = 20) -> dict:
    """Follow a 202 by listing until the named item exists. LRO delay is 0
    by default, so this is usually one GET; the loop is for the rare poll
    that races CompleteAt."""
    for _ in range(tries):
        page = az_rest("get", list_url)
        items = page.get(envelope) or page.get("value") or []
        for it in items:
            if it.get(field) == name:
                return it
        time.sleep(0.3)
    fail(f"{name!r} never appeared at {list_url}")


def driver() -> None:
    login()

    print("-- 2. capacities: list, unassign, assign")
    cap = find_named(az_rest("get", v1("capacities")).get("value") or [],
                     "Emulator Capacity")
    if cap.get("id") != CAPACITY or cap.get("state") != "Active" or cap.get("sku") != "F64":
        fail(f"seeded capacity = {cap}")
    ws = az_rest("post", v1("workspaces"), {"displayName": "az-rest-ws"})
    wsid = ws["id"]
    if ws.get("capacityId") != CAPACITY:
        fail(f"auto-assign capacityId = {ws.get('capacityId')}")
    az_rest("post", v1(f"workspaces/{wsid}/unassignFromCapacity"))
    got = az_rest("get", v1(f"workspaces/{wsid}"))
    if got.get("capacityId"):
        fail(f"capacityId survived unassign: {got.get('capacityId')}")
    az_rest("post", v1(f"workspaces/{wsid}/assignToCapacity"), {"capacityId": CAPACITY})
    got = az_rest("get", v1(f"workspaces/{wsid}"))
    if got.get("capacityId") != CAPACITY:
        fail(f"capacityId after assign = {got.get('capacityId')}")
    print("   listed, unassigned, reassigned")

    print("-- 3. folders: create, nest, list")
    parent = az_rest("post", v1(f"workspaces/{wsid}/folders"), {"displayName": "Notebooks"})
    nested = az_rest("post", v1(f"workspaces/{wsid}/folders"),
                     {"displayName": "Processing", "parentFolderId": parent["id"]})
    folders = az_rest("get", v1(f"workspaces/{wsid}/folders")).get("value") or []
    if len(folders) < 2 or nested.get("parentFolderId") != parent["id"]:
        fail(f"folders = {folders}")
    print(f"   {len(folders)} folders, nested parent matches")

    print("-- 4. role assignments: grant Viewer, list, delete")
    grant = az_rest("post", v1(f"workspaces/{wsid}/roleAssignments"),
                    {"principal": {"id": "az-rest-viewer", "type": "User"}, "role": "Viewer"})
    ras = az_rest("get", v1(f"workspaces/{wsid}/roleAssignments")).get("value") or []
    if not any(r.get("id") == grant.get("id") and r.get("role") == "Viewer" for r in ras):
        fail(f"grant missing from list: {ras}")
    az_rest("delete", v1(f"workspaces/{wsid}/roleAssignments/{grant['id']}"))
    after = az_rest("get", v1(f"workspaces/{wsid}/roleAssignments")).get("value") or []
    if any(r.get("id") == grant.get("id") for r in after):
        fail("grant survived delete")
    print("   Viewer granted and removed")

    print("-- 5. report definition round-trip (PBIR bytes)")
    layout = b'{"$schema":"https://developer.microsoft.com/json-schemas/fabric/item/report/definition/report/1.0.0/schema.json","sections":[{"name":"page1","visualContainers":[]}]}'
    binding = b'{"version":"1.0","datasetReference":{"byPath":{"path":"../sales.SemanticModel"}}}'
    parts = [
        {"path": "report.json", "payload": base64.b64encode(layout).decode(), "payloadType": "InlineBase64"},
        {"path": "definition.pbir", "payload": base64.b64encode(binding).decode(), "payloadType": "InlineBase64"},
    ]
    az_rest("post", v1(f"workspaces/{wsid}/reports"),
            {"displayName": "sales-report", "definition": {"parts": parts}})
    report = poll_item(v1(f"workspaces/{wsid}/reports"), "sales-report")
    got_def = az_rest("post", v1(f"workspaces/{wsid}/reports/{report['id']}/getDefinition"))
    back = {p["path"]: base64.b64decode(p["payload"]) for p in got_def["definition"]["parts"]}
    if back.get("report.json") != layout or back.get("definition.pbir") != binding:
        fail(f"report definition did not round-trip: {list(back)}")
    print("   getDefinition returned the same PBIR parts")

    print("-- 6. deleted display name held (409 isRetriable)")
    nb = az_rest("post", v1(f"workspaces/{wsid}/notebooks"), {"displayName": "reserved-nb"})
    az_rest("delete", v1(f"workspaces/{wsid}/notebooks/{nb['id']}"))
    err = az_rest("post", v1(f"workspaces/{wsid}/notebooks"),
                  {"displayName": "reserved-nb"}, allow_error=True)
    if err.get("errorCode") != "ItemDisplayNameNotAvailableYet":
        fail(f"recreate reserved name: {err}")
    if err.get("isRetriable") is not True:
        fail(f"isRetriable = {err.get('isRetriable')!r}, want true")
    print("   409 ItemDisplayNameNotAvailableYet isRetriable=true")

    print("-- 7. job scheduler CRUD + list instances + queryactivityruns")
    pipe = az_rest("post", v1(f"workspaces/{wsid}/items"),
                   {"displayName": "nightly", "type": "DataPipeline"})
    wait = json.dumps({"properties": {"activities": [
        {"name": "Pause", "type": "Wait", "typeProperties": {"waitTimeInSeconds": 0}},
    ]}}).encode()
    az_rest("post", v1(f"workspaces/{wsid}/items/{pipe['id']}/updateDefinition"),
            {"definition": {"parts": [
                {"path": "pipeline-content.json",
                 "payload": base64.b64encode(wait).decode(),
                 "payloadType": "InlineBase64"},
            ]}})
    start = (datetime.now(UTC) + timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    end = (datetime.now(UTC) + timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    sched = az_rest("post", v1(f"workspaces/{wsid}/items/{pipe['id']}/jobs/Pipeline/schedules"),
                    {"configuration": {
                        "type": "Cron", "interval": 10,
                        "startDateTime": start, "endDateTime": end,
                        "localTimeZoneId": "UTC",
                    }})
    if not sched.get("id") or sched.get("enabled") is False:
        fail(f"schedule shape: {sched}")
    one = az_rest("get", v1(f"workspaces/{wsid}/items/{pipe['id']}/jobs/Pipeline/schedules/{sched['id']}"))
    if one.get("id") != sched["id"]:
        fail(f"get schedule = {one}")
    listed = az_rest("get", v1(f"workspaces/{wsid}/items/{pipe['id']}/jobs/Pipeline/schedules"))
    if not any(s.get("id") == sched["id"] for s in (listed.get("value") or [])):
        fail(f"schedule not listed: {listed}")
    az_rest("delete", v1(f"workspaces/{wsid}/items/{pipe['id']}/jobs/Pipeline/schedules/{sched['id']}"))

    az_rest("post", v1(f"workspaces/{wsid}/items/{pipe['id']}/jobs/instances?jobType=Pipeline"), {})
    instance = poll_item(v1(f"workspaces/{wsid}/items/{pipe['id']}/jobs/instances"),
                         "Manual", field="invokeType")
    if instance.get("invokeType") != "Manual":
        fail(f"on-demand instance = {instance}")
    detail = None
    for _ in range(20):
        detail = az_rest(
            "post",
            v1(f"workspaces/{wsid}/items/{pipe['id']}/jobs/instances/{instance['id']}/queryactivityruns"),
            {}, allow_error=True)
        if isinstance(detail, dict) and "value" in detail:
            break
        time.sleep(0.3)
    else:
        fail(f"queryactivityruns: {detail}")
    names = {a.get("activityName") for a in (detail.get("value") or []) if isinstance(a, dict)}
    if "Pause" not in names:
        fail(f"queryactivityruns missing Pause: {detail}")
    print("   schedule CRUD, Manual instance listed, Pause in queryactivityruns")

    print("-- 8. admin: tenant settings, workspaces, items, capacity overrides, domains, labels")
    settings = az_rest("get", v1("admin/tenantsettings"))
    by_name = {s["settingName"]: s for s in settings.get("value") or []}
    for name, group in {
        "AdminApisIncludeDetailedMetadata": "AdminApiSettings",
        "DatamartTenant": "DatamartSettings",
        "CertifyDatasets": "ExportAndSharing",
    }.items():
        s = by_name.get(name)
        if not s or s.get("tenantSettingGroup") != group or not s.get("title"):
            fail(f"tenant setting {name} = {s}")
    updated = az_rest("post", v1("admin/tenantsettings/DatamartTenant/update"),
                      {"enabled": False})
    wrapped = updated.get("tenantSettings") or []
    if len(wrapped) != 1 or wrapped[0].get("enabled"):
        fail(f"tenantsettings update = {updated}")

    admin_ws = az_rest("get", v1("admin/workspaces"))
    found = None
    for w in admin_ws.get("workspaces") or []:
        if w.get("id") == wsid:
            found = w
            break
    if not found or found.get("name") != "az-rest-ws":
        fail(f"admin workspaces uses `name`, got {found}")
    if found.get("type") != "Workspace" or found.get("state") != "Active":
        fail(f"admin workspace type/state = {found}")

    admin_items = az_rest("get", v1("admin/items"))
    entities = admin_items.get("itemEntities")
    if not isinstance(entities, list) or not any(e.get("workspaceId") == wsid for e in entities):
        fail(f"admin items envelope is itemEntities; got keys {list(admin_items)}")

    ov = az_rest("post",
                 v1(f"admin/capacities/{CAPACITY}/delegatedTenantSettingOverrides/GitIntegration/update"),
                 {"enabled": False})
    ovs = ov.get("overrides") or []
    if len(ovs) != 1 or ovs[0].get("settingName") != "GitIntegration" or ovs[0].get("enabled"):
        fail(f"capacity override = {ov}")
    listed_ov = az_rest("get", v1("admin/capacities/delegatedTenantSettingOverrides"))
    if not any(c.get("id") == CAPACITY for c in (listed_ov.get("value") or [])):
        fail(f"override list = {listed_ov}")

    domain = az_rest("post", v1("admin/domains"), {"displayName": "az-rest-domain"})
    domains = az_rest("get", v1("admin/domains")).get("domains") or []
    if not any(d.get("id") == domain["id"] for d in domains):
        fail(f"created domain not listed: {domains}")
    az_rest("delete", v1(f"admin/domains/{domain['id']}"))

    # Seeded taxonomy (store/labels.go). GET /v1/admin/labels is an emulator
    # affordance, not a Fabric API; a stranger already holds the Purview id.
    confidential = "11111111-0000-4000-8000-00000000000c"
    live_nb = az_rest("post", v1(f"workspaces/{wsid}/notebooks"), {"displayName": "labelled-nb"})
    set_res = az_rest("post", v1("admin/items/bulkSetLabels"),
                      {"labelId": confidential, "items": [{"id": live_nb["id"]}]})
    if len(set_res.get("successfulItems") or []) != 1:
        fail(f"bulkSetLabels = {set_res}")
    item = az_rest("get", v1(f"workspaces/{wsid}/items/{live_nb['id']}"))
    if (item.get("sensitivityLabel") or {}).get("id") != confidential:
        fail(f"item label = {item.get('sensitivityLabel')}")
    az_rest("post", v1("admin/items/bulkRemoveLabels"), {"items": [{"id": live_nb["id"]}]})
    item = az_rest("get", v1(f"workspaces/{wsid}/items/{live_nb['id']}"))
    if item.get("sensitivityLabel", {}).get("id"):
        fail(f"label survived removal: {item.get('sensitivityLabel')}")
    print("   admin envelopes, override, domain, labels")

    print("-- 9. activityevents (Power BI admin path)")
    day = datetime.now(UTC).strftime("%Y-%m-%d")
    q = urllib.parse.urlencode({
        "startDateTime": f"'{day}T00:00:00Z'",
        "endDateTime": f"'{day}T23:59:59Z'",
    }, safe="':")
    events_url = f"{FABRIC}/v1.0/myorg/admin/activityevents?{q}"
    # Prefer the Power BI audience this path documents; fall back to Fabric
    # if this entra pin will not mint it — the Go tests accept either.
    events = az_rest("get", events_url, resource=PBI_AUD, allow_error=True)
    if "activityEventEntities" not in events:
        events = az_rest("get", events_url)
    ops = {e.get("Operation") for e in (events.get("activityEventEntities") or [])}
    for need in ("CreateWorkspace", "CreateArtifact"):
        if need not in ops:
            fail(f"no {need} in activityevents; saw {sorted(ops)}")
    print(f"   {len(events.get('activityEventEntities') or [])} events, including CreateWorkspace")

    print("-- 10. workspace-identity handshake (entra is the sibling)")
    ident_ws = az_rest("post", v1("workspaces"), {"displayName": "identity-ws"})
    az_rest("post", v1(f"workspaces/{ident_ws['id']}/provisionIdentity"))
    shape = None
    for _ in range(20):
        shape = az_rest("get", v1(f"workspaces/{ident_ws['id']}"))
        ident = shape.get("workspaceIdentity") or {}
        if ident.get("applicationId") and ident.get("servicePrincipalId"):
            break
        time.sleep(0.3)
    else:
        fail(f"provisionIdentity did not stamp workspaceIdentity: {shape}")
    az_rest("post", v1(f"workspaces/{ident_ws['id']}/deprovisionIdentity"))
    for _ in range(20):
        shape = az_rest("get", v1(f"workspaces/{ident_ws['id']}"))
        if not shape.get("workspaceIdentity"):
            break
        time.sleep(0.3)
    else:
        fail(f"workspaceIdentity survived deprovision: {shape.get('workspaceIdentity')}")
    print("   provisioned, identity on the workspace, deprovisioned")

    print("\nAZ REST E2E: PASS — Microsoft's az CLI drove Fabric REST and activityevents")


def main() -> int:
    try:
        driver()
        return 0
    except SystemExit:
        raise
    except subprocess.CalledProcessError as e:
        print(f"FAIL: {e}", file=sys.stderr)
        return e.returncode or 1
    finally:
        az("cloud", "set", "--name", "AzureCloud", check=False)
        az("cloud", "unregister", "--name", CLOUD, check=False)
        if _ARM_STUB is not None:
            _ARM_STUB.shutdown()


if __name__ == "__main__":
    sys.exit(main())
