# Fabric notebook source

# CELL ********************

# Silver: dedupe, conform, quarantine — the rules bronze deliberately does not
# apply. Latest event wins per order, emails lower-cased, country codes
# conformed, malformed rows quarantined rather than dropped.
#
# THIS IS A NOTEBOOK, AND THAT IS THE POINT. The transform used to live in
# silver.py and dial Spark Connect, which no Fabric tenant exposes — so the step
# ran locally and could not run in production. Here the code is a Notebook
# DEFINITION, submitted as a job: the emulator's Spark agent executes it locally,
# and real Fabric executes it on the workspace's starter pool. Same file, same
# cells, both targets.
#
# `spark` is injected by the runtime, exactly as in a Fabric notebook. Nothing
# here imports the example's fixture package: a notebook runs on the pool, where
# only what the definition carries exists. The numbers this must produce are
# asserted by silver.py against the tables that land, which is the stronger
# check anyway — it verifies the artifact rather than an in-process DataFrame.

from pyspark.sql import Window
from pyspark.sql import functions as F

# ABFS addresses OneLake by its full account host, as a Fabric notebook does.
base = "abfs://{{WORKSPACE_ID}}@onelake.dfs.fabric.microsoft.com/{{LAKEHOUSE_ID}}/Tables"


def read(table):
    return spark.read.format("delta").load(f"{base}/{table}")


def write(df, table):
    df.write.format("delta").mode("overwrite").save(f"{base}/{table}")


# Every spelling the vendor emits, keyed by its uppercased/stripped form. This is
# silver's own business rule, written out rather than derived from the source
# system's own variant table on purpose: sharing that mapping would make the
# conformance check agree with itself, and a new variant appearing upstream would
# silently conform instead of failing.
COUNTRY = {
    "US": "US", "USA": "US", "U.S.": "US", "UNITED STATES": "US",
    "GB": "GB", "U.K.": "GB", "UK": "GB", "GBR": "GB", "UNITED KINGDOM": "GB",
    "SG": "SG", "SGP": "SG", "SINGAPORE": "SG",
}

# --- customers ---------------------------------------------------------------
# The Copy activity lands CSV as strings, so every column here is text; only the
# ones silver actually reasons about get touched. Silver keeps the customer
# record WIDE — narrowing to the columns gold needs would be the wrong place to
# do it, because the star's dimensions are a projection of silver, not the
# other way round.
c = read("bronze_customers").dropDuplicates(["customer_id"])
conform = F.create_map([F.lit(x) for kv in COUNTRY.items() for x in kv])
country_key = F.upper(F.trim(F.col("country")))
silver_customers = (
    c.withColumn("email", F.lower(F.trim(F.coalesce(F.col("email"), F.lit("")))))
     .withColumn("country", F.coalesce(conform[country_key], country_key))
)

# --- orders ------------------------------------------------------------------
# Latest event wins: rank each order's events by the vendor's own sequence and
# keep the last. `dropDuplicates` would keep an ARBITRARY row per key — correct
# only by luck, and silently wrong the day the redelivery carries a different
# status.
o = read("bronze_orders")
latest = Window.partitionBy("order_id").orderBy(F.col("event_seq").desc())
o = (o.withColumn("_rn", F.row_number().over(latest))
      .filter(F.col("_rn") == 1)
      .drop("_rn"))

bad = (F.col("quantity") <= 0) | F.col("unit_price").isNull()
quarantine = o.filter(bad)
clean = (o.filter(~bad)
          .withColumn("order_date", F.to_date("order_date"))
          .withColumn("amount", F.col("quantity") * F.col("unit_price")))

silver_orders = clean.select(
    "order_id", "customer_id", "product_id", "order_date", "channel",
    "store_id", "currency", "discount_pct", "tax_rate", "shipping_fee",
    "payment_method", "is_gift", "promo_code", "quantity",
    "unit_price", "amount", "status")

write(silver_customers, "silver_customers")
write(silver_orders, "silver_orders")
write(quarantine, "silver_quarantine_orders")

# Invariants of the TRANSFORM, asserted here because a failing cell fails the
# job — and because these need no knowledge of the fixture's exact numbers, which
# is what keeps this notebook runnable on any tenant. The row counts belong to
# silver.py, which knows what the generator produced.
n_orders = silver_orders.count()
assert silver_orders.select("order_id").distinct().count() == n_orders, \
    "silver_orders still has duplicate order ids after the dedupe"
# The unresolvable people are still here, not quietly dropped: a resolution step
# downstream claiming 100% would be lying, and this cohort is what proves it.
assert silver_customers.filter(F.col("email") == "").count() > 0, \
    "the missing-email cohort vanished"

print(f"silver: {silver_customers.count()} customers, {n_orders} orders, "
      f"{quarantine.count()} quarantined")
