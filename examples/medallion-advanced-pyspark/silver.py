"""Silver: dedupe, conform, quarantine — the rules bronze deliberately does not
apply. Latest event wins per order, emails lower-cased and case-folded, country
codes conformed, malformed rows quarantined rather than dropped.

**This runs on Spark**, over Spark Connect against the engine `docker compose up`
attaches (Sail). That is the point of the step as much as the transforms are:
Lakehouse-to-Lakehouse ETL on Fabric is Spark work. This step used to read the
Delta into pandas and transform on the client, which is a fine way to move ten
thousand rows and the wrong shape entirely for a hundred million — the data
never has to leave the lakehouse, and here it does not.

The same transform written as **dbt-fabricspark models** is in
`../medallion-dbt-fabricspark`, which builds the identical silver declaratively and then
compares the two. Imperative Spark or declarative dbt is a real choice a Fabric
team makes; neither example claims it is the only one.

Silver keeps the customer record WIDE. Narrowing to the handful of columns gold
needs would be the wrong place to do it: silver is the conformed customer-360,
and the star's dimensions are a projection of it, not the other way round.
"""
import source_system as src
from common import SPARK_REMOTE, load, log

st = load()
assert SPARK_REMOTE, "SPARK_REMOTE is empty — no Spark engine is attached"

from pyspark.sql import SparkSession, Window  # noqa: E402
from pyspark.sql import functions as F  # noqa: E402

spark = SparkSession.builder.remote(SPARK_REMOTE).getOrCreate()

import time as _time  # noqa: E402
_t0 = _time.time()  # build clock: the transform starts here

base = (f"abfs://{st['workspace']}@onelake.dfs.fabric.microsoft.com"
        f"/{st['lakehouse']}/Tables")


def read(table):
    return spark.read.format("delta").load(f"{base}/{table}")


def write(df, table):
    df.write.format("delta").mode("overwrite").save(f"{base}/{table}")


# Every spelling the vendor emits, keyed by its uppercased/stripped form. This
# is silver's own business rule, written out rather than derived from
# source_system.COUNTRY_VARIANTS on purpose: importing the generator's mapping
# would make the conformance assert agree with itself, and a new variant
# appearing upstream would silently conform instead of failing here.
COUNTRY = {
    "US": "US", "USA": "US", "U.S.": "US", "UNITED STATES": "US",
    "GB": "GB", "U.K.": "GB", "UK": "GB", "GBR": "GB", "UNITED KINGDOM": "GB",
    "SG": "SG", "SGP": "SG", "SINGAPORE": "SG",
}

# --- customers ---------------------------------------------------------------
# The Copy activity lands CSV as strings, so every column here is text; only the
# ones silver actually reasons about get touched.
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

n_customers = silver_customers.count()
n_orders = silver_orders.count()
n_quarantine = quarantine.count()
countries = {r["country"] for r in silver_customers.select("country").distinct().collect()}

assert n_customers == src.EXPECTED_SILVER_CUSTOMERS, n_customers
assert n_orders == src.EXPECTED_SILVER_ORDERS, n_orders
assert n_quarantine == src.EXPECTED_QUARANTINED, n_quarantine
assert countries == src.EXPECTED_COUNTRIES, countries
assert len(silver_customers.columns) == src.EXPECTED_CUSTOMER_COLUMNS, silver_customers.columns
assert silver_orders.select("order_id").distinct().count() == n_orders, \
    "silver_orders still has duplicate order ids"
# The unresolvable people are still here, not quietly dropped — web_bronze.py
# depends on them existing to prove a resolution step that claims 100% is lying.
assert silver_customers.filter(F.col("email") == "").count() > 0, \
    "the missing-email cohort vanished"

log(f"silver (PySpark): {n_customers:,} customers x {len(silver_customers.columns)} cols, "
    f"{n_orders:,} orders, {n_quarantine:,} quarantined")

# A machine-readable summary of what this tool produced, so
# ../medallion-dbt-fabricspark can compare its declarative build against this
# imperative one without either example importing the other's code.
import json  # noqa: E402
import pathlib  # noqa: E402

pathlib.Path(__file__).resolve().parent.joinpath("silver_summary.json").write_text(
    json.dumps({
        "engine": "PySpark (Spark Connect)",
        "target": "Lakehouse (Delta in OneLake)",
        "compute": "Sail (Rust Spark Connect, no JVM)",
        "build_seconds": round(_time.time() - _t0, 2),
        "rows": {"silver_customers": n_customers, "silver_orders": n_orders,
                 "silver_quarantine_orders": n_quarantine},
        # Empty, and that is the finding rather than an omission: Spark SQL
        # needs no statement rewriting on the wire. The Warehouse half of this
        # example does (docs/29-tsql-parity.md, T6 and T8).
        "dialect_adaptations": [],
    }, indent=2))
