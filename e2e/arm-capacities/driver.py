#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.12"
# dependencies = [
#     "azure-identity==1.25.3",
#     "azure-mgmt-resource==26.0.0",
#     "azure-mgmt-fabric==1.1.0b1",
#     "requests==2.32.5",
# ]
# ///
"""Microsoft's azure-mgmt-fabric creates a capacity; fabric-emulator lists it.

Nothing here is emulator-specific beyond the configuration real Azure supports:
`base_url` / `credential_scopes` for ARM, `authority` + `disable_instance_discovery`
for identity, and a Fabric-audience token for GET /v1/capacities.
"""

import os
import sys
import time

import requests
from azure.identity import ClientSecretCredential
from azure.mgmt.fabric import FabricMgmtClient
from azure.mgmt.resource.resources import ResourceManagementClient

ARM = os.environ["ARM_URL"]
ENTRA = os.environ["ENTRA_URL"]
FABRIC = os.environ["FABRIC_URL"]
TENANT = os.environ["ARM_TENANT_ID"]
SUB = os.environ["ARM_SUBSCRIPTION_ID"]
CLIENT_ID = os.environ["ARM_CLIENT_ID"]
CLIENT_SECRET = os.environ["ARM_CLIENT_SECRET"]
SEED = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
SCOPES = ["https://management.azure.com/.default"]
RG = "fabric-arm-e2e"
CAP = "armcape2e"


def fail(msg):
    sys.exit(f"FAIL (arm-capacities): {msg}")


def wait_until(pred, what, timeout=30):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        last = pred()
        if last:
            return last
        time.sleep(0.4)
    fail(f"{what} never happened (last={last})")


def main():
    credential = ClientSecretCredential(
        tenant_id=TENANT, client_id=CLIENT_ID, client_secret=CLIENT_SECRET,
        authority=ENTRA, disable_instance_discovery=True)

    session = requests.Session()
    session.verify = os.environ.get("REQUESTS_CA_BUNDLE") or True

    def fabric_caps():
        tok = credential.get_token("https://api.fabric.microsoft.com/.default")
        r = session.get(f"{FABRIC}/v1/capacities",
                        headers={"Authorization": f"Bearer {tok.token}"}, timeout=10)
        if r.status_code != 200:
            fail(f"GET /v1/capacities = {r.status_code} {r.text[:300]}")
        return {c["id"]: c for c in r.json()["value"]}

    before = fabric_caps()
    if SEED not in before:
        fail(f"seeded capacity missing before ARM create: {list(before)}")
    print("-- 1. seeded capacity is listed")

    resources = ResourceManagementClient(
        credential, SUB, base_url=ARM, credential_scopes=SCOPES)
    resources.resource_groups.create_or_update(RG, {"location": "westeurope"})
    fabric = FabricMgmtClient(
        credential, SUB, base_url=ARM, credential_scopes=SCOPES)
    created = fabric.fabric_capacities.begin_create_or_update(
        RG, CAP, {
            "location": "westeurope",
            "sku": {"name": "F2", "tier": "Fabric"},
            "properties": {"administration": {"members": ["arm-e2e@example.com"]}},
        }).result()
    print(f"-- 2. ARM capacity created: {created.id}")

    arm_id = wait_until(
        lambda: next((i for i in fabric_caps() if i != SEED), None),
        "ARM capacity on GET /v1/capacities")
    got = fabric_caps()[arm_id]
    if got.get("sku") != "F2":
        fail(f"listed ARM capacity = {got}")
    print(f"-- 3. Fabric listed ARM capacity {arm_id} sku={got['sku']}")

    tok = credential.get_token("https://api.fabric.microsoft.com/.default")
    r = session.post(f"{FABRIC}/v1/workspaces",
                     headers={"Authorization": f"Bearer {tok.token}"},
                     json={"displayName": "on-arm-capacity", "capacityId": arm_id},
                     timeout=10)
    if r.status_code != 201:
        fail(f"create workspace = {r.status_code} {r.text[:300]}")
    if r.json().get("capacityId") != arm_id:
        fail(f"workspace not on ARM capacity: {r.json()}")
    print("-- 4. workspace created on the ARM capacity")

    fabric.fabric_capacities.begin_delete(RG, CAP).result()
    wait_until(lambda: arm_id not in fabric_caps(), "ARM capacity to vanish from Fabric")
    after = fabric_caps()
    if SEED not in after:
        fail("seeded capacity disappeared with the ARM row")
    print("-- 5. ARM row gone, seed remains")

    resources.resource_groups.begin_delete(RG).result()
    print("\nARM-CAPACITIES E2E: PASS — azure-mgmt-fabric create → GET /v1/capacities")


if __name__ == "__main__":
    main()
