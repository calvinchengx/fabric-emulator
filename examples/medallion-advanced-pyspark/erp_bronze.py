"""A4 — bronze for the ERP change log and the reference feeds.

Three Copy activities in one DataPipeline, and all three read **Parquet**. That
is the point of this step as much as the data is: 03 exercised the Copy
activity's delimited-text path, and this exercises its columnar one. The
emulator reads the Parquet itself and commits the rows as Delta — no
client-side data movement, and it reports `rowsCopied` back for each activity.

Nothing is interpreted here. The change log lands as a change log: `I`/`U`/`D`
events with their capture sequence, still out of order where the connector
delivered them that way. Turning that into a dimension with history is 24's
job, and 24 has to do it from what actually arrived.
"""
import json

import erp_system as erp
import reference_data as ref
from common import activity_runs, create_item, load, log, run_job, save

st = load()
lake, ws = st["lakehouse"], st["workspace"]
erp_dir = f"landing/contoso_erp/{st['erp_landing_date']}"
ref_dir = f"landing/reference/{st['erp_landing_date']}"

lakehouse = {"linkedService": {"properties": {
    "type": "Lakehouse",
    "typeProperties": {"workspaceId": ws, "artifactId": lake}}}}


def copy_activity(name, folder, filename, table):
    """A Copy whose source is Parquet rather than delimited text."""
    return {"name": name, "type": "Copy", "typeProperties": {
        "source": {"type": "ParquetSource", "rootFolder": "Files",
                   "folderPath": folder, "fileName": filename,
                   "datasetSettings": lakehouse},
        "sink": {"type": "LakehouseTableSink", "tableActionOption": "Overwrite",
                 "table": table, "datasetSettings": lakehouse}}}


definition = {"properties": {"activities": [
    copy_activity("IngestErpChanges", erp_dir, "changes.parquet", "bronze_erp_changes"),
    copy_activity("IngestFxRates", ref_dir, "fx_rates.parquet", "bronze_fx_rates"),
    copy_activity("IngestProductHierarchy", ref_dir, "product_hierarchy.parquet",
                  "bronze_product_hierarchy"),
]}}

pl = create_item("erp-reference-ingest", "DataPipeline",
                 {"pipeline-content.json": json.dumps(definition)})
jid, status = run_job(pl, "Pipeline")
assert status == "Completed", f"erp/reference pipeline: {status}"

runs = {r["activityName"]: r for r in activity_runs(pl, jid)}
expected = {
    "IngestErpChanges": erp.EXPECTED_ERP_CHANGE_EVENTS,
    "IngestFxRates": ref.EXPECTED_FX_ROWS,
    "IngestProductHierarchy": ref.EXPECTED_PRODUCTS,
}
for activity, want in expected.items():
    out = runs[activity]["output"]
    # A Parquet source must land as ROWS, not as an opaque byte copy — a
    # rowsCopied of None would mean the emulator fell back to moving the file.
    assert out.get("rowsCopied") == want, f"{activity}: {out}"
    log(f"Copy ({activity}) landed {out['rowsCopied']:,} rows from Parquet")

save(erp_pipeline=pl)
