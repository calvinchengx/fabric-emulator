import json
import urllib.error
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT = "cccccccc-0000-0000-0000-000000000002"
SECRET = "daemon-app-secret"


def request(method, url, body=None, token=None):
    headers = {}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    with urllib.request.urlopen(urllib.request.Request(url, data=data, headers=headers, method=method)) as r:
        raw = r.read()
        return json.loads(raw) if raw else None


def token(scope):
    data = urllib.parse.urlencode({"grant_type": "client_credentials", "client_id": CLIENT,
                                   "client_secret": SECRET, "scope": scope}).encode()
    req = urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=data,
                                 headers={"Content-Type": "application/x-www-form-urlencoded"})
    with urllib.request.urlopen(req) as r:
        return json.load(r)["access_token"]


try:
    request("POST", f"{ENTRA}/admin/api/apps", {
        "displayName": "Azure Storage", "appIdUri": "https://storage.azure.com", "isConfidential": False})
except urllib.error.HTTPError as error:
    if error.code != 409:
        raise

fabric_token = token("https://api.fabric.microsoft.com/.default")
storage_token = token("https://storage.azure.com/.default")
ws = request("POST", f"{FABRIC}/v1/workspaces", {"displayName": "external-ws"}, fabric_token)
lake = request("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, fabric_token)
connection = request("POST", f"{FABRIC}/v1/connections", {
    "displayName": "anonymous-object-store", "connectivityType": "ShareableCloud",
    "credentialDetails": {"credentials": {"credentialType": "Anonymous"}},
}, fabric_token)

targets = [
    ("adlsGen2", "adls", "http://external-store:8080/adls", "/container/folder", b"from-adls-gen2"),
    ("amazonS3", "s3", "http://external-store:8080/s3", "/bucket/prefix", b"from-amazon-s3"),
]
for kind, name, location, subpath, expected in targets:
    request("POST", f"{FABRIC}/v1/workspaces/{ws['id']}/items/{lake['id']}/shortcuts", {
        "path": "Files", "name": name,
        "target": {kind: {"location": location, "subpath": subpath,
                           "connectionId": connection["id"]}},
    }, fabric_token)
    req = urllib.request.Request(
        f"http://onelake.dfs.fabric.microsoft.com/{ws['id']}/{lake['id']}/Files/{name}/data.txt",
        headers={"Authorization": "Bearer " + storage_token})
    with urllib.request.urlopen(req) as response:
        got = response.read()
    assert got == expected, (kind, got)
    print(f"{kind}: {got.decode()}", flush=True)

print("EXTERNAL SHORTCUTS E2E: PASS", flush=True)
