"""Provision the workspace, lakehouse, warehouse, and workspace identity."""
from common import FABRIC, WORKSPACE_NAME, S, fabric_headers, log, post_and_wait, save

H = fabric_headers()


def create(url, body):
    """Fail loudly with the target's error, not a KeyError three lines later.
    Display names are unique per workspace, so a second run on a dirty stack
    lands here — see Cleanup in the tutorial.

    `post_and_wait` because real Fabric creates a Warehouse ASYNCHRONOUSLY: 202
    with an operation and no body, where a development stack answers 201 with the
    item. Reading `.json()["id"]` off that 202 is a TypeError on a tenant and
    nothing locally — found by running this against a real trial."""
    return post_and_wait(url, body)


ws = create(f"{FABRIC}/v1/workspaces", {"displayName": WORKSPACE_NAME})
lh = create(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"})
wh = create(f"{FABRIC}/v1/workspaces/{ws['id']}/warehouses", {"displayName": "dw"})

# The workspace identity is what fetches the Key Vault secret on Fabric's side
# when the AKV-reference connection resolves.
r = S.post(f"{FABRIC}/v1/workspaces/{ws['id']}/provisionIdentity", headers=H)
assert r.status_code in (200, 202), f"provisionIdentity: {r.status_code} {r.text}"

# The lakehouse's display NAME as well as its id: Spark addresses it by name
# (it is the schema dbt-fabricspark writes into), where the REST and TDS
# surfaces both address it by GUID. examples/medallion-dbt-fabricspark needs the name.
save(workspace=ws["id"], lakehouse=lh["id"], warehouse=wh["id"],
     lakehouse_name=lh["displayName"], workspace_name=ws["displayName"])
log(f"provisioned workspace={ws['id']} lakehouse={lh['id']} warehouse={wh['id']}")
