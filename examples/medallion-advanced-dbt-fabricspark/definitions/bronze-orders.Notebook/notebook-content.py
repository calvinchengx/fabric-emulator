# Fabric notebook source

# CELL ********************

from pyspark.sql.functions import lit

# ABFS addresses OneLake by its full account host, exactly as a Fabric
# notebook does: abfs://<workspace>@onelake.dfs.fabric.microsoft.com/<item>/...
landing = "abfs://{{WORKSPACE_ID}}@onelake.dfs.fabric.microsoft.com/{{LAKEHOUSE_ID}}/Files/{{LANDING_DIR}}/orders.jsonl"
bronze = "abfs://{{WORKSPACE_ID}}@onelake.dfs.fabric.microsoft.com/{{LAKEHOUSE_ID}}/Tables/bronze_orders"

orders = spark.read.json(landing).withColumn("_landing_date", lit("{{LANDING_DATE}}"))
orders.write.format("delta").mode("overwrite").save(bronze)
print("bronze_orders rows:", orders.count())
