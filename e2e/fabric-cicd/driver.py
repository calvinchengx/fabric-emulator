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
  FABRIC_FORCE_LRO               set when run.py started the emulator with the
                                 forced-async toggle; adds the LRO witness at
                                 the bottom and flips the synchronous
                                 assertions to their asynchronous halves
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

FORCE_LRO = os.environ.get("FABRIC_FORCE_LRO", "").lower() not in ("", "0", "false")
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


def follow_lro(resp, what, deadline=120):
    """Walk the documented 202 contract by hand and return the result body.

    Deliberately follows `Location` rather than building operation URLs, because
    that is the only thing the reference promises a client: the header names the
    STATE url while the operation runs and the RESULT url once it succeeds.
    A harness that constructs `/v1/operations/{id}/result` itself would pass
    against an emulator that never moved the header, which is the divergence
    this whole leg exists to catch.
    """
    assert resp.status_code == 202, f"{what}: expected 202, got {resp.status_code} {resp.text}"
    for header in ("Location", "x-ms-operation-id", "Retry-After"):
        assert resp.headers.get(header), f"{what}: 202 without a {header} header: {dict(resp.headers)}"
    url, end, states = resp.headers["Location"], time.time() + deadline, []
    while True:
        state = requests.get(url, headers=auth)
        assert state.status_code == 200, (state.status_code, state.text)
        body = state.json()
        states.append(body["status"])
        if body["status"] == "Succeeded":
            break
        assert body["status"] in ("NotStarted", "Running"), f"{what}: {body}"
        assert state.headers.get("Retry-After"), f"{what}: a running operation must say when to poll again"
        assert state.headers.get("Location") == url, f"{what}: Location moved before the operation finished"
        assert time.time() < end, f"{what}: never left {body['status']}"
        time.sleep(0.2)
        url = state.headers["Location"]
    result_url = state.headers.get("Location")
    assert result_url and result_url != url, f"{what}: a succeeded operation must point at its result"
    result = requests.get(result_url, headers=auth)
    assert result.status_code == 200, (result.status_code, result.text)
    return result.json(), states


def parts_of(item_id):
    resp = requests.post(f"{FABRIC}/v1/workspaces/{ws_id}/items/{item_id}/getDefinition", headers=auth)
    # The two documented outcomes of one API. Asserting the status EXACTLY,
    # rather than accepting either, is what makes the pair of CI legs mean
    # something: without it a leg whose FABRIC_FORCE_LRO never reached the
    # server would pass as the async witness.
    if FORCE_LRO:
        body, _ = follow_lro(resp, "getDefinition")
    else:
        assert resp.status_code == 200, f"getDefinition: expected 200, got {resp.status_code}"
        body = resp.json()
    return sorted(p["path"] for p in body["definition"]["parts"])


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


def create_item_raw(name, item_type="Notebook"):
    """createItem with no definition — the case the reference documents as
    201 *or* 202, and the one the emulator used to answer synchronously
    always."""
    return requests.post(f"{FABRIC}/v1/workspaces/{ws_id}/items", headers=auth,
                         json={"displayName": name, "type": item_type})


def faults(**body):
    resp = requests.post(f"{FABRIC}/_emulator/faults", json=body)
    assert resp.status_code == 200, (resp.status_code, resp.text)


if not FORCE_LRO:
    # The half a default emulator takes, asserted here so the two legs are a
    # PAIR rather than one leg and a hope: 201 with the item in the body.
    resp = create_item_raw("sync-outcome")
    assert resp.status_code == 201, f"expected the synchronous outcome, got {resp.status_code}"
    assert resp.json()["displayName"] == "sync-outcome", resp.text
    print("synchronous outcome: createItem answered 201 with the item")
else:
    # --- the witness: Microsoft's client drives the forced-async outcome -----
    #
    # Everything above already ran under FABRIC_FORCE_LRO, so fabric-cicd
    # published three items, round-tripped their definitions and republished
    # them entirely through 202 + Location + poll + /result. Its poll loop is
    # its own (_common/_fabric_endpoint.py) and knows nothing about this
    # emulator. What follows pins down the parts a green publish alone would
    # not distinguish.

    # 1. The flag really reached THIS server. Without this, a leg whose env var
    #    was misspelled would publish happily against a synchronous emulator and
    #    report itself as the async witness — the same shape of false green as a
    #    URL matched but never fetched.
    resp = create_item_raw("async-outcome")
    body, states = follow_lro(resp, "createItem")
    assert body["displayName"] == "async-outcome", body
    assert body["id"], body
    print(f"forced outcome: createItem answered 202, /result carried the item (states={states})")

    # 2. A genuinely RUNNING operation, not one that succeeds on the first poll.
    #    The contract's moving parts — Retry-After, Location staying on the state
    #    url, then moving to /result — only exist while there is something to
    #    wait for, so an emulator that completed instantly would witness none of
    #    them. follow_lro asserts each of those on every intermediate poll.
    faults(lroDelaySeconds=2)
    body, states = follow_lro(create_item_raw("slow-outcome"), "createItem (delayed)")
    assert body["displayName"] == "slow-outcome", body
    assert "Running" in states or "NotStarted" in states, \
        f"the delay did not produce an intermediate state, so nothing polled through one: {states}"
    print(f"delayed outcome: the client polled through {states}")

    # 3. The same delay, through Microsoft's client rather than through mine.
    #    This is the arm that would catch a Retry-After the emulator sends and a
    #    real client refuses, or a status word only this harness accepts.
    publish_all_items(ws)
    print("fabric-cicd republished everything through a genuinely running LRO")
    faults(lroDelaySeconds=0)

    # 4. THE NEGATIVE HALF, and the one that proves the client is reading OUR
    #    operation body rather than treating any 202 as success: fail the next
    #    operation and fabric-cicd must SURFACE it. An emulator whose failed
    #    operation carried the wrong shape would leave the client polling a
    #    status it does not recognise until its own timeout, or — worse —
    #    reporting a publish that never happened.
    faults(failNextOperations=1)
    try:
        publish_all_items(ws)
    except Exception as e:  # noqa: BLE001 — the type is fabric-cicd's business
        detail = f"{e}\n{getattr(e, 'additional_info', '') or ''}"
    else:
        raise SystemExit("fabric-cicd reported success for an operation the emulator failed")
    assert "OperationFailed" in detail, \
        f"the client did not surface the emulator's own errorCode: {detail}"
    print("failed outcome: fabric-cicd raised with the emulator's errorCode")

    # And the failure was injected, not terminal — one more publish succeeds,
    # so arm 4 cannot pass by having broken the workspace.
    faults(failNextOperations=0)
    publish_all_items(ws)
    print("recovered: the next publish succeeded")

print("FABRIC-CICD E2E: PASS")
