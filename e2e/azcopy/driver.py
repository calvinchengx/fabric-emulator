"""e2e: drive Microsoft's real `azcopy` against fabric-emulator's OneLake Blob
surface. Uploads a multi-block file, downloads it back byte-identical, and
confirms the DFS surface sees the same object — one storage substrate, two
dialects, exercised by the actual tool.

Auth: azcopy always drives its token through MSAL, which validates the
authority against the public login.microsoftonline.com — unreachable offline.
So instead of making azcopy mint a token, we hand it one we already forged from
entra-emulator, via AZCOPY_OAUTH_TOKEN_INFO with `_token_refresh_source:
TokenStore`. That is azcopy's static-token mode (used for MSI/Service-Fabric
token brokering): it returns the supplied access_token as-is while it is not
near expiry, with no AAD round-trip. Same Storage-audience token the ADLS SDK
suite forges — only the delivery differs.

Addressing is azurite path-style: `https://localhost:PORT/onelake/{workspace}/
{item}/...`, where the account is the literal `onelake` and the container is the
workspace. --from-to nudges azcopy since a localhost host isn't auto-detected as
Blob, and --trusted-microsoft-suffixes whitelists the host:port so azcopy is
willing to attach the bearer to it."""
import json
import os
import ssl
import subprocess
import sys
import urllib.parse
import urllib.request

ENTRA = f"https://localhost:{os.environ.get('ENTRA_PORT', '18543')}"
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19543")
# `localhost` (not 127.0.0.1) so the blob URL host matches both the cert SAN and
# the --trusted-microsoft-suffixes entry azcopy checks before attaching a bearer.
FABRIC = f"https://localhost:{FABRIC_PORT}"
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
AZCOPY = os.environ["AZCOPY_BIN"]
WORK = os.environ["AZCOPY_WORK"]

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE  # self-signed harness cert (our own control-plane calls)


def post_json(url, body, token=None):
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, context=_CTX) as r:
        return json.loads(r.read() or b"{}")


def fabric_token():
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": "daemon-app-secret",
        "scope": "https://api.fabric.microsoft.com/.default"}).encode()
    with urllib.request.urlopen(
            urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form), context=_CTX) as r:
        return json.loads(r.read())["access_token"]


def storage_token():
    t = post_json(f"{ENTRA}/admin/api/tokens", {"clientId": CLIENT_ID, "audience": "https://storage.azure.com"})
    return t.get("access_token") or t["token"]


def azcopy(*args):
    env = {
        **os.environ,
        # static-token mode: azcopy uses this access_token verbatim, no AAD call.
        "AZCOPY_OAUTH_TOKEN_INFO": json.dumps({
            "access_token": STORAGE, "refresh_token": "",
            "expires_in": "3600", "expires_on": "4102444800", "not_before": "0",
            "resource": "https://storage.azure.com", "token_type": "Bearer",
            "_tenant": TENANT, "_ad_endpoint": ENTRA, "_token_refresh_source": "TokenStore"}),
        "SSL_CERT_FILE": os.environ["AZCOPY_CA_BUNDLE"],  # trust the emulator's self-signed CA (Go honours this)
        "AZCOPY_LOG_LOCATION": os.path.join(WORK, "azlog"),
        "AZCOPY_JOB_PLAN_LOCATION": os.path.join(WORK, "azplan"),
    }
    r = subprocess.run(
        [AZCOPY, *args, "--trusted-microsoft-suffixes", f"localhost:{FABRIC_PORT}", "--output-type=text"],
        env=env, capture_output=True, text=True)
    sys.stdout.write(r.stdout)
    if r.returncode != 0 or "Final Job Status: Completed" not in r.stdout:
        sys.stderr.write(r.stderr)
        raise RuntimeError("azcopy failed")


FABRIC_TOKEN = fabric_token()
STORAGE = storage_token()

ws = post_json(f"{FABRIC}/v1/workspaces", {"displayName": "azws"}, FABRIC_TOKEN)
post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, FABRIC_TOKEN)
print(f"workspace: {ws['id']}")

# A deterministic multi-block payload: 12 MiB with a 4 MiB block size forces
# azcopy through Put Block + Put Block List (not a single Put Blob), exercising
# the emulator's block-staging commit path.
payload = (bytes(range(256)) * 4096) * 12  # 12 MiB, deterministic
src = os.path.join(WORK, "src.bin")
with open(src, "wb") as f:
    f.write(payload)

blob = "lake.Lakehouse/Files/azcopy/data.bin"
dest = f"{FABRIC}/onelake/{ws['id']}/{blob}"

# 1. Upload with azcopy (multi-block commit).
azcopy("copy", src, dest, "--from-to=LocalBlob", "--block-size-mb=4")
print(f"azcopy uploaded {len(payload)} bytes (multi-block)")

# 2. Download it back with azcopy; bytes must be identical.
out = os.path.join(WORK, "out.bin")
azcopy("copy", dest, out, "--from-to=BlobLocal")
with open(out, "rb") as f:
    got = f.read()
assert got == payload, f"azcopy round-trip changed bytes: {len(got)} vs {len(payload)}"
print("azcopy download: OK (bytes identical)")

# 3. The same object is visible through the DFS surface — one substrate.
req = urllib.request.Request(
    f"{FABRIC}/{ws['id']}?resource=filesystem&recursive=true",
    headers={"Authorization": "Bearer " + STORAGE, "Host": "onelake.dfs.fabric.microsoft.com"})
with urllib.request.urlopen(req, context=_CTX) as r:
    dfs = [p["name"] for p in json.loads(r.read())["paths"]]
assert any(n.endswith("data.bin") for n in dfs), dfs
print(f"DFS surface sees the azcopy-written blob: {len(dfs)} paths")

print("AZCOPY E2E: PASS")
