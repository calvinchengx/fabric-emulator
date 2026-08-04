"""Provision with Microsoft's CLI: a workspace on a capacity, and a lakehouse.

`fab mkdir` is the whole of it. Compare examples/medallion-pyspark/provision.py,
which POSTs to `/v1/workspaces` and `/v1/workspaces/{id}/lakehouses` and then
has to remember the GUIDs that came back. Here the CLI resolves names to GUIDs
on every call, so nothing needs remembering.
"""
import sys

import fabctl as fab

if fab.exists(fab.WORKSPACE):
    sys.exit(
        f"{fab.WORKSPACE} already exists. Display names are unique per tenant, "
        f"so a second run lands here. This stack keeps its store in memory:\n"
        f"    docker compose down && docker compose up -d\n"
        f"clears it completely."
    )

fab.run("mkdir", fab.WORKSPACE, "-P", f"capacityName={fab.CAPACITY}")
fab.run("mkdir", fab.LAKEHOUSE)

# Read it back through a DIFFERENT verb than the one that created it. `mkdir`
# reporting success only says the request was accepted; `exists` says the
# control plane will now answer for it.
assert fab.exists(fab.WORKSPACE), "workspace created but does not exist"
assert fab.exists(fab.LAKEHOUSE), "lakehouse created but does not exist"

fab.log(f"workspace  {fab.WORKSPACE}  id={fab.item_id(fab.WORKSPACE)}")
fab.log(f"lakehouse  {fab.LAKEHOUSE}  id={fab.item_id(fab.LAKEHOUSE)}")
