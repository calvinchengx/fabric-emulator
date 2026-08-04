# Fabric notebook source
#
# The silver transform, as a Fabric author would write it: read bronze Delta
# from OneLake over ABFS, apply the rules bronze deliberately does not, write
# silver Delta back. Nothing here knows it is running against an emulator.
#
# {{WORKSPACE_ID}} and {{LAKEHOUSE_ID}} are substituted by import_items.py
# before `fab import` uploads this. A definition on disk cannot contain GUIDs
# that only exist after provisioning, which is the same reason Microsoft's
# fabric-cicd has a find_replace step — see the README.

# METADATA ********************
# META {
# META   "kernel_info": { "name": "synapse_pyspark" },
# META   "language_info": { "name": "python" }
# META }

# CELL ********************

from pyspark.sql import functions as F

BASE = "abfs://{{WORKSPACE_ID}}@onelake.dfs.fabric.microsoft.com/{{LAKEHOUSE_ID}}/Tables"

# Every spelling the vendor emits, keyed by its uppercased/stripped form. This
# is silver's OWN business rule, written out rather than imported from the
# generator: sharing the mapping would make the conformance check agree with
# itself, and a new upstream spelling would silently conform instead of failing.
COUNTRY = {
    "US": "US", "USA": "US", "U.S.": "US", "UNITED STATES": "US",
    "GB": "GB", "U.K.": "GB", "UK": "GB", "GBR": "GB", "UNITED KINGDOM": "GB",
    "SG": "SG", "SGP": "SG", "SINGAPORE": "SG",
}

# CELL ********************

bronze = spark.read.format("delta").load(f"{BASE}/bronze_customers")

# The Copy activity lands CSV as strings, so every column is text; only the
# ones silver reasons about get touched.
conform = F.create_map([F.lit(x) for kv in COUNTRY.items() for x in kv])
country_key = F.upper(F.trim(F.col("country")))

silver = (
    bronze.dropDuplicates(["customer_id"])
    .withColumn("email", F.lower(F.trim(F.coalesce(F.col("email"), F.lit("")))))
    .withColumn("country", F.coalesce(conform[country_key], country_key))
)

silver.write.format("delta").mode("overwrite").save(f"{BASE}/silver_customers")

# CELL ********************

# Exit with the facts the caller needs to check, as JSON. A notebook that exits
# with a bare row count can only be checked for one thing, and readback.py wants
# to assert on the conformance too.
import json

rows = spark.read.format("delta").load(f"{BASE}/silver_customers")
countries = sorted(r["country"] for r in rows.select("country").distinct().collect())
mssparkutils.notebook.exit(json.dumps({
    "silver_customers": rows.count(),
    "countries": countries,
}))
