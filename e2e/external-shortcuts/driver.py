import json
import urllib.error
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT = "00d88624-f0d7-46f6-a641-6232c2608928"
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
# ONE CONNECTION PER TARGET KIND, WITH ITS OWN CREDENTIAL, because a Fabric
# connection is TYPED and the type constrains both halves. This
# used to be a single "anonymous-object-store" reused for both shortcuts, which
# no tenant would accept: the creation method and its parameters differ per
# connector (`AzureDataLakeStorage` takes server+path, `AmazonS3.Storage` takes
# url+roleArn), so a connection cannot be kind-agnostic. Types and parameter
# names below are from a real tenant's
# GET /v1/connections/supportedConnectionTypes, 2026-08-11.
targets = [
    ("adlsGen2", "adls", "http://external-store:8080/adls", "/container/folder", b"from-adls-gen2",
     {"type": "AzureDataLakeStorage", "creationMethod": "AzureDataLakeStorage",
      "parameters": [{"dataType": "Text", "name": "server", "value": "external-store"},
                     {"dataType": "Text", "name": "path", "value": "/adls"}]},
     # Key, not SharedAccessSignature: a SAS REPLACES the request query string
     # (internal/onelake), which rewrites the fixture's path. Key sends a
     # header the fixture ignores, and the tenant lists both for this connector.
     {"credentialType": "Key", "key": "fixture-key"}),
    ("amazonS3", "s3", "http://external-store:8080/s3", "/bucket/prefix", b"from-amazon-s3",
     {"type": "AmazonS3", "creationMethod": "AmazonS3.Storage",
      "parameters": [{"dataType": "Text", "name": "url", "value": "http://external-store:8080/s3"},
                     {"dataType": "Text", "name": "roleArn", "value": ""}]},
     {"credentialType": "Basic", "username": "fixture", "password": "fixture"}),
]
for kind, name, location, subpath, expected, details, credential in targets:
    connection = request("POST", f"{FABRIC}/v1/connections", {
        "displayName": f"external-{name}", "connectivityType": "ShareableCloud",
        "connectionDetails": details,
        "credentialDetails": {"credentials": credential},
    }, fabric_token)
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
