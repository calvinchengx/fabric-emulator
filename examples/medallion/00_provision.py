"""Provision the workspace, lakehouse, warehouse, and workspace identity."""
from common import FABRIC, S, fabric_headers, log, save

H = fabric_headers()


def create(url, body):
    """Fail loudly with the emulator's error, not a KeyError three lines later.
    Display names are unique per workspace, so a second run on a dirty stack
    lands here — see Cleanup in the tutorial."""
    r = S.post(url, headers=H, json=body)
    assert r.status_code in (200, 201, 202), f"{url} -> {r.status_code} {r.text}"
    return r.json()


ws = create(f"{FABRIC}/v1/workspaces", {"displayName": "contoso-analytics"})
lh = create(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"})
wh = create(f"{FABRIC}/v1/workspaces/{ws['id']}/warehouses", {"displayName": "dw"})

# The workspace identity is what fetches the Key Vault secret on Fabric's side
# when the AKV-reference connection resolves.
r = S.post(f"{FABRIC}/v1/workspaces/{ws['id']}/provisionIdentity", headers=H)
assert r.status_code in (200, 202), f"provisionIdentity: {r.status_code} {r.text}"

save(workspace=ws["id"], lakehouse=lh["id"], warehouse=wh["id"])
log(f"provisioned workspace={ws['id']} lakehouse={lh['id']} warehouse={wh['id']}")
