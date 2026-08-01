"""Materialise the resolution, so gold has a key to join on.

`resolve.py` computes the identity graph, asserts it and writes nothing — the
resolution exists there as a PROOF. A proof is not a table, and gold cannot join
three sources on an argument. This step turns it into two tables plus the web
fact grain:

  * `silver_customer_xref`      — (source_system, source_id) -> customer_key
  * `silver_customer_conformed` — one row per customer_key, attributes survived
  * `silver_web_order_lines`    — clean web lines carrying customer_key

POS is the spine, and not by preference: every edge runs through it. Web joins
POS on email, ERP joins POS on phone, and Web and ERP share no key at all, so a
person known to all three is keyed by their POS identity. Someone POS has never
seen is keyed on their own source's identity rather than dropped — the whole
point of resolve.py is that three cohorts cannot be placed, and losing them here
would quietly undo that.

Keys are a deterministic hash of a namespaced identity string, not a sequence.
A rerun therefore produces the same keys with no registry to carry, which is
what lets the star be rebuilt incrementally. The cost is stated plainly: the key
is a FUNCTION of the identity, so a POS-only customer who later supplies an
email is re-keyed and looks like a new person. Real MDM keeps a persisted
registry and records merge/split events; pretending this is that would be worse
than saying so.
"""
import erp_system as erp
import web_store as web
from common import SPARK_REMOTE, load, log

st = load()
assert SPARK_REMOTE, "SPARK_REMOTE is empty — no Spark engine is attached"

from pyspark.sql import functions as F  # noqa: E402
from pyspark.sql import SparkSession  # noqa: E402

spark = SparkSession.builder.remote(SPARK_REMOTE).getOrCreate()

base = (f"abfs://{st['workspace']}@onelake.dfs.fabric.microsoft.com"
        f"/{st['lakehouse']}/Tables")


def read(table):
    return spark.read.format("delta").load(f"{base}/{table}")


def write(df, table):
    df.write.format("delta").mode("overwrite").save(f"{base}/{table}")


def key(namespace, col):
    """A stable surrogate over a namespaced identity."""
    return F.sha2(F.concat(F.lit(namespace + ":"), col), 256)


# --- the spine ---------------------------------------------------------------
pos = (read("silver_customers")
       .select("customer_id", "name", "email", "country", "phone")
       # silver already lower-cased and trimmed email; "" is the missing marker
       # rather than NULL, and it must NOT be treated as a joinable value.
       .withColumn("email_key", F.when(F.col("email") != "", F.col("email")))
       .withColumn("phone_key", F.trim(F.col("phone").cast("string")))
       .withColumn("customer_key", key("pos", F.col("customer_id"))))

web_c = (read("bronze_web_customers")
         .select(F.lower(F.trim("email")).alias("email_key"),
                 F.col("full_name"), F.col("country").alias("web_country")))

erp_now = (read("dim_customer_scd2").filter(F.col("is_current"))
           .select("erp_customer_id", "legal_name", "account_tier", "segment",
                   "credit_band", F.trim(F.col("phone").cast("string")).alias("phone_key")))

# Web inherits the POS key where the email matches, and mints its own where it
# does not. A left join from web onto pos, never the reverse: a POS customer
# with no web account must keep their key.
web_keyed = (web_c.join(pos.select("email_key", "customer_key"), "email_key", "left")
             .withColumn("customer_key",
                         F.coalesce(F.col("customer_key"), key("web", F.col("email_key")))))

erp_keyed = (erp_now.join(pos.select("phone_key", "customer_key"), "phone_key", "left")
             .withColumn("customer_key",
                         F.coalesce(F.col("customer_key"), key("erp", F.col("erp_customer_id")))))

# --- the crosswalk -----------------------------------------------------------
xref = (
    pos.select(F.lit("contoso_pos").alias("source_system"),
               F.col("customer_id").alias("source_id"), "customer_key")
    .unionByName(web_keyed.select(F.lit("contoso_web").alias("source_system"),
                                  F.col("email_key").alias("source_id"), "customer_key"))
    .unionByName(erp_keyed.select(F.lit("contoso_erp").alias("source_system"),
                                  F.col("erp_customer_id").alias("source_id"), "customer_key"))
)

# --- survivorship ------------------------------------------------------------
# Which system wins is a business decision, so it is written down rather than
# implied by a join order. Web is where a customer maintains their own profile;
# POS is a till system capturing whatever the cashier typed; ERP holds the legal
# and commercial truth and nothing else does.
#
# Falling THROUGH a null is the rule that matters: a higher-priority source that
# simply does not hold an attribute must not blank out one that does.
conformed = (
    pos.select("customer_key", "customer_id", "name", "email", "country", "phone")
    .join(web_keyed.select("customer_key", "full_name", "web_country"), "customer_key", "full_outer")
    .join(erp_keyed.select("customer_key", "legal_name", "account_tier", "segment", "credit_band"),
          "customer_key", "full_outer")
    .select(
        "customer_key",
        F.coalesce("full_name", "name", "legal_name").alias("name"),
        F.when(F.col("email") != "", F.col("email")).alias("email"),
        F.coalesce("country", "web_country").alias("country"),
        "phone", "account_tier", "segment", "credit_band",
        F.col("customer_id").isNotNull().alias("in_pos"),
        F.col("full_name").isNotNull().alias("in_web"),
        F.col("legal_name").isNotNull().alias("in_erp"))
    .withColumn("source_count",
                F.col("in_pos").cast("int") + F.col("in_web").cast("int")
                + F.col("in_erp").cast("int"))
)

# --- the web fact grain ------------------------------------------------------
# Cancelled orders and lines pointing at a product the catalogue does not carry
# are excluded from the FACT, not deleted: bronze still holds every one of them.
products = read("bronze_web_products").select("product_id")
lines = read("bronze_web_order_lines")
web_lines = (
    lines.join(F.broadcast(products), "product_id", "left_semi")
         .filter(F.col("order_status") != "cancelled")
         .withColumn("email_key", F.lower(F.trim("email")))
         .join(xref.filter(F.col("source_system") == "contoso_web")
                   .select(F.col("source_id").alias("email_key"), "customer_key"),
               "email_key", "left")
         # placed_at carries a source offset; normalise to UTC so a day means
         # one thing across channels. Some orders change calendar day here, and
         # that is the correct answer rather than a rounding artefact.
         .withColumn("order_date", F.to_date(F.to_utc_timestamp(F.col("placed_at"), "UTC")))
         .withColumn("amount", F.col("quantity") * F.col("unit_price"))
         .select("web_order_id", "line_no", "customer_key", "product_id", "order_date",
                 "quantity", "unit_price", "amount",
                 F.lit("web").alias("channel"))
)

write(xref, "silver_customer_xref")
write(conformed, "silver_customer_conformed")
write(web_lines, "silver_web_order_lines")

# --- what the materialisation has to preserve --------------------------------
n_xref, n_conformed = xref.count(), conformed.count()
n_lines = web_lines.count()

# Every source record maps to exactly one key, or the crosswalk is not a
# function and every downstream join fans out.
assert xref.count() == xref.select("source_system", "source_id").distinct().count(), \
    "a source record resolves to more than one customer_key"
assert conformed.select("customer_key").distinct().count() == n_conformed, \
    "silver_customer_conformed is not one row per customer_key"

# The unplaceable cohorts survive as their own identities. resolve.py proves
# they exist; this proves materialising did not quietly merge them away.
web_only = conformed.filter(~F.col("in_pos") & F.col("in_web")).count()
erp_only = conformed.filter(~F.col("in_pos") & F.col("in_erp")).count()
assert web_only == web.EXPECTED_WEB_ONLY_EMAIL_COUNT, (web_only, web.EXPECTED_WEB_ONLY_EMAIL_COUNT)
assert erp_only == erp.EXPECTED_ERP_ONLY_COUNT, (erp_only, erp.EXPECTED_ERP_ONLY_COUNT)

assert n_lines == web.EXPECTED_WEB_CLEAN_LINES, (n_lines, web.EXPECTED_WEB_CLEAN_LINES)
assert web_lines.filter(F.col("customer_key").isNull()).count() == 0, \
    "a web order line does not resolve to a customer"

multi = conformed.filter(F.col("source_count") > 1).count()
log(f"materialised: {n_xref:,} source records -> {n_conformed:,} identities "
    f"({multi:,} multi-source, {web_only:,} web-only, {erp_only:,} erp-only)")
log(f"web fact grain: {n_lines:,} clean order lines, all resolved to a customer_key")
