"""Drive the real fabric-cicd tool against fabric-emulator.

The emulator's TLS cert covers api.fabric.microsoft.com, and fabric-cicd's
URL validator accepts that hostname on any port — so DNS is pinned to
127.0.0.1 in-process (like curl --resolve) and both of fabric-cicd's API
roots point at the emulator. Auth is a custom azure-core TokenCredential
doing client credentials against entra-emulator.

Requires (see run.py):
  FABRIC_PORT / ENTRA_PORT       emulator ports (default 19443 / 18443)
  REQUESTS_CA_BUNDLE             fabric-emulator's cert.pem
  FABRIC_API_ROOT_URL            https://api.fabric.microsoft.com:$FABRIC_PORT
  DEFAULT_API_ROOT_URL           same (fabric-cicd's Power BI root)
"""

import os
import socket
import time

# Pin api.fabric.microsoft.com -> 127.0.0.1 before anything opens sockets.
_real_getaddrinfo = socket.getaddrinfo


def _pinned(host, *args, **kw):
    if host == "api.fabric.microsoft.com":
        host = "127.0.0.1"
    return _real_getaddrinfo(host, *args, **kw)


socket.getaddrinfo = _pinned

import requests  # noqa: E402
from azure.core.credentials import AccessToken  # noqa: E402
from fabric_cicd import FabricWorkspace, change_log_level, publish_all_items  # noqa: E402

if os.environ.get("FABRIC_CICD_DEBUG"):
    change_log_level("DEBUG")

ENTRA = f"https://localhost:{os.environ.get('ENTRA_PORT', '18443')}"
FABRIC = f"https://api.fabric.microsoft.com:{os.environ.get('FABRIC_PORT', '19443')}"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
# entra-emulator's seeded confidential daemon app (public dev values).
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"


class EmulatorCredential:
    """azure-core TokenCredential backed by entra-emulator client credentials."""

    def get_token(self, *scopes, **kwargs):
        r = requests.post(
            f"{ENTRA}/{TENANT}/oauth2/v2.0/token",
            data={
                "grant_type": "client_credentials",
                "client_id": CLIENT_ID,
                "client_secret": CLIENT_SECRET,
                "scope": "https://api.fabric.microsoft.com/.default",
            },
            verify=False,  # entra's self-signed cert; harness-only
        )
        r.raise_for_status()
        return AccessToken(r.json()["access_token"], int(time.time()) + 3600)


cred = EmulatorCredential()
token = cred.get_token().token
auth = {"Authorization": f"Bearer {token}"}

# Create the target workspace as the SP (it becomes Admin). fabric-cicd
# requires a capacity; the emulator auto-assigns its seeded default.
r = requests.post(
    f"{FABRIC}/v1/workspaces",
    json={"displayName": "cicd-target"},  # capacity auto-assigned by the emulator
    headers=auth,
)
r.raise_for_status()
ws_id = r.json()["id"]
print(f"workspace: {ws_id}")

ws = FabricWorkspace(
    workspace_id=ws_id,
    repository_directory=os.path.join(os.path.dirname(os.path.abspath(__file__)), "repo"),
    item_type_in_scope=["Notebook", "VariableLibrary", "DataPipeline"],
    token_credential=cred,
)
publish_all_items(ws)

# Verify through the plain REST surface: items exist, definitions round-trip.
r = requests.get(f"{FABRIC}/v1/workspaces/{ws_id}/items", headers=auth)
items = {i["displayName"]: i for i in r.json()["value"]}
assert set(items) == {"hello", "envLib", "medallion"}, items
assert items["hello"]["type"] == "Notebook", items["hello"]
assert items["envLib"]["type"] == "VariableLibrary", items["envLib"]
assert items["medallion"]["type"] == "DataPipeline", items["medallion"]


def parts_of(item_id):
    resp = requests.post(f"{FABRIC}/v1/workspaces/{ws_id}/items/{item_id}/getDefinition", headers=auth)
    resp.raise_for_status()
    return sorted(p["path"] for p in resp.json()["definition"]["parts"])


paths = parts_of(items["hello"]["id"])
print(f"published notebook parts: {paths}")
assert ".platform" in paths and "notebook-content.py" in paths, paths

# The Variable Library round-trips as its own multi-file definition, INCLUDING
# the nested valueSets/ directory — a part path with a directory in it, which
# is the shape a flat definition model would silently flatten or drop.
paths = parts_of(items["envLib"]["id"])
print(f"published variable library parts: {paths}")
assert "variables.json" in paths, paths
assert "settings.json" in paths, paths
assert "valueSets/qat.json" in paths, paths

# --- the witness: a library variable RESOLVES in a published pipeline --------
#
# The pipeline compares the resolved value against a run parameter and FAILS
# when they differ, so a completed run is evidence the value was right rather
# than evidence nothing errored. Both items were published by Microsoft's own
# client from a git-shaped repository; nothing below hand-builds a definition.
def run_pipeline(expected):
    base = f"{FABRIC}/v1/workspaces/{ws_id}/items/{items['medallion']['id']}/jobs/instances"
    resp = requests.post(f"{base}?jobType=Pipeline", headers=auth,
                         json={"executionData": {"parameters": {"expected": expected}}})
    assert resp.status_code == 202, (resp.status_code, resp.text)
    jid = resp.headers["Location"].rsplit("/", 1)[-1]
    deadline = time.time() + 60
    while True:
        got = requests.get(f"{base}/{jid}", headers=auth).json()
        if got.get("status") not in ("NotStarted", "InProgress"):
            return got.get("status")
        assert time.time() < deadline, f"pipeline job never finished: {got}"
        time.sleep(0.2)


status = run_pipeline("Files/bronze")
print(f"default value set -> {status}")
assert status == "Completed", f"the library variable did not resolve to Files/bronze ({status})"

# The witness can fail. Without this the green above proves nothing.
status = run_pipeline("Files/somewhere-else")
assert status != "Completed", "the pipeline passed against a value the library never declared"
print("negative arm -> Failed, as it must")

# THE ENVIRONMENT SWITCH, through the same published definitions: activate the
# other value set and the SAME pipeline resolves the other value.
resp = requests.patch(f"{FABRIC}/v1/workspaces/{ws_id}/variableLibraries/{items['envLib']['id']}",
                      headers=auth, json={"properties": {"activeValueSetName": "qat"}})
assert resp.status_code == 200, (resp.status_code, resp.text)
status = run_pipeline("Files/bronze-qat")
print(f"qat value set -> {status}")
assert status == "Completed", f"the qat override did not take effect ({status})"

# Publish is idempotent: a second run updates rather than duplicates.
publish_all_items(ws)
r = requests.get(f"{FABRIC}/v1/workspaces/{ws_id}/items", headers=auth)
assert len(r.json()["value"]) == 3, r.json()

print("FABRIC-CICD E2E: PASS")
