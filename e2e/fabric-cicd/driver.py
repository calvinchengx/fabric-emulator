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
TENANT = "11111111-1111-1111-1111-111111111111"
# entra-emulator's seeded confidential daemon app (public dev values).
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
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
    item_type_in_scope=["Notebook"],
    token_credential=cred,
)
publish_all_items(ws)

# Verify through the plain REST surface: item exists, definition round-trips.
r = requests.get(f"{FABRIC}/v1/workspaces/{ws_id}/items", headers=auth)
items = r.json()["value"]
assert len(items) == 1 and items[0]["type"] == "Notebook" and items[0]["displayName"] == "hello", items
r = requests.post(f"{FABRIC}/v1/workspaces/{ws_id}/items/{items[0]['id']}/getDefinition", headers=auth)
paths = sorted(p["path"] for p in r.json()["definition"]["parts"])
print(f"published definition parts: {paths}")
assert ".platform" in paths and "notebook-content.py" in paths, paths

# Publish is idempotent: a second run updates rather than duplicates.
publish_all_items(ws)
r = requests.get(f"{FABRIC}/v1/workspaces/{ws_id}/items", headers=auth)
assert len(r.json()["value"]) == 1, r.json()

print("FABRIC-CICD E2E: PASS")
