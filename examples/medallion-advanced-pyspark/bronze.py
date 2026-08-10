"""Bronze: land the raw export into Delta tables, keeping everything —
duplicates and the malformed row included.

Both hops run as activities in a real **DataPipeline**, which is how a Fabric
shop builds this, and which exercises the emulator rather than routing around
it:

  * `customers.csv` → `Tables/bronze_customers` with a **Copy activity**. The
    emulator executes this itself — it reads the CSV, commits the rows as Delta,
    and records a lineage edge. No client-side data movement at all.
  * `orders.jsonl` → `Tables/bronze_orders` with a **Notebook activity**. Copy
    cannot do this one: JSON Lines is not a tabular source it parses into a
    table, and semi-structured input is exactly what a notebook is for.

Fabric's notebook activity is SYNCHRONOUS: the pipeline gates on the notebook's
terminal state, which is what makes `dependsOn: Succeeded` mean anything. With a
Spark agent attached the emulator drives the run itself and the activity reports
that terminal state, exactly as a Fabric pipeline does. `engine.py` still shows
the other half of the contract — the `notebookRunResult` callback an external
engine uses.
"""

import source_system as src
from common import activity_runs, create_item_from_definition, load, log, run_job, save

st = load()
lake, ws = st["lakehouse"], st["workspace"]
landing_dir = f"landing/contoso_pos/{st['landing_date']}"

# The items are deployed from `definitions/`, which holds them in Fabric's own
# SOURCE FORMAT — `<display name>.<Type>/` with the definition files and
# `.platform`, exactly what Git integration writes and fabric-cicd deploys
# (docs/46-artifact-persistence.md). The committed files carry {{TOKENS}} because
# a definition names the workspace and lakehouse it reads by GUID and those do
# not exist until provision.py has run; substituting at deploy is what
# fabric-cicd's own find_replace parameter file is for.
nb = create_item_from_definition(
    "bronze-orders.Notebook",
    WORKSPACE_ID=ws, LAKEHOUSE_ID=lake,
    LANDING_DIR=landing_dir, LANDING_DATE=st["landing_date"])

# The same, for the pipeline: one Copy activity for the structured feed and a
# TridentNotebook activity for the semi-structured one. NOTEBOOK_ID is a
# placeholder because the notebook above only just acquired its id — the ordinary
# case in a real deployment, where items reference each other by GUID.
pl = create_item_from_definition(
    "bronze-ingest.DataPipeline",
    WORKSPACE_ID=ws, LAKEHOUSE_ID=lake,
    LANDING_DIR=landing_dir, NOTEBOOK_ID=nb)
jid, status = run_job(pl, "Pipeline")
assert status == "Completed", f"bronze pipeline: {status}"

runs = {r["activityName"]: r for r in activity_runs(pl, jid)}

# The Copy really moved rows — the emulator did the work and says how many.
copy_out = runs["IngestCustomers"]["output"]
assert copy_out["rowsCopied"] == src.EXPECTED_BRONZE_CUSTOMERS, copy_out
log(f"Copy activity landed {copy_out['rowsCopied']} customer rows in Tables/bronze_customers")

# The activity gated on the notebook: it reports the run's terminal state, not
# a queued one. A pipeline cannot depend on an activity that always says
# "Pending", which is why Fabric's is synchronous.
nb_out = runs["IngestOrders"]["output"]
assert nb_out["status"] == "Completed", f"expected a finished run: {nb_out}"
save(bronze_pipeline=pl, orders_notebook=nb, orders_job=nb_out["jobInstanceId"])
log(f"Notebook activity ran {nb_out['jobInstanceId']} to completion")
