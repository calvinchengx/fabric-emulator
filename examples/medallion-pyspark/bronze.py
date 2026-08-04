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
import json

import source_system as src
from common import activity_runs, create_item, load, log, run_job, save

st = load()
lake, ws = st["lakehouse"], st["workspace"]
landing_dir = f"landing/contoso_pos/{st['landing_date']}"

# The notebook a Fabric author would write for the semi-structured feed: read
# landing with Spark, write Delta straight to OneLake.
NOTEBOOK = f'''# Fabric notebook source

# CELL ********************

from pyspark.sql.functions import lit

# ABFS addresses OneLake by its full account host, exactly as a Fabric
# notebook does: abfs://<workspace>@onelake.dfs.fabric.microsoft.com/<item>/...
landing = "abfs://{ws}@onelake.dfs.fabric.microsoft.com/{lake}/Files/{landing_dir}/orders.jsonl"
bronze = "abfs://{ws}@onelake.dfs.fabric.microsoft.com/{lake}/Tables/bronze_orders"

orders = spark.read.json(landing).withColumn("_landing_date", lit("{st['landing_date']}"))
orders.write.format("delta").mode("overwrite").save(bronze)
print("bronze_orders rows:", orders.count())
'''

nb = create_item("bronze-orders", "Notebook", {"notebook-content.py": NOTEBOOK})

# One pipeline, two activities — the shape a real medallion ingest takes.
# Built as a dict and serialised, not string-templated: a pipeline definition is
# nested JSON, and hand-escaping braces is how you get PipelineDefinitionInvalid.
lakehouse = {"linkedService": {"properties": {
    "type": "Lakehouse",
    "typeProperties": {"workspaceId": ws, "artifactId": lake}}}}

definition = {"properties": {"activities": [
    {"name": "IngestCustomers", "type": "Copy", "typeProperties": {
        "source": {"type": "DelimitedTextSource", "rootFolder": "Files",
                   "folderPath": landing_dir, "fileName": "customers.csv",
                   "datasetSettings": lakehouse},
        "sink": {"type": "LakehouseTableSink", "tableActionOption": "Overwrite",
                 "table": "bronze_customers",
                 "datasetSettings": lakehouse}}},
    {"name": "IngestOrders", "type": "TridentNotebook", "typeProperties": {
        "notebookId": nb}},
]}}

pl = create_item("bronze-ingest", "DataPipeline",
                 {"pipeline-content.json": json.dumps(definition)})
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
