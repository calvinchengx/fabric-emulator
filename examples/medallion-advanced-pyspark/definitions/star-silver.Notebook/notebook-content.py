# Fabric notebook source

# CELL ********************

# Materialise the identity resolution, so gold has a key to join on.
#
# `resolve.py` computes the identity graph, asserts it and writes nothing — the
# resolution exists there as a PROOF. A proof is not a table, and gold cannot join
# three sources on an argument. This notebook turns it into two tables plus the
# web fact grain, and a fourth table nobody joins:
#
#   * `silver_customer_xref`         — (source_system, source_id) -> customer_key
#   * `silver_customer_conformed`    — one row per customer_key, attributes survived
#   * `silver_web_order_lines`       — clean web lines carrying customer_key
#   * `silver_resolution_metrics`    — one row: what the INPUTS looked like
#
# THAT FOURTH TABLE IS WHY THIS IS A NOTEBOOK AND NOT A SCRIPT, so it is worth
# being explicit. The transform used to run in star_silver.py over Spark Connect,
# which no Fabric tenant exposes. Moving it into a notebook definition is what
# makes it run on a real pool — but the step's assertions need quantities that
# only exist INSIDE the transform: how many phone keys were too ambiguous to match
# on, how many ERP and web accounts came in. A notebook cannot return those
# through a job (real Fabric exposes no exit value for a REST-submitted run), and
# inventing an emulator-only channel would give us a green local test and nothing
# on a tenant. So they are written where every other output goes: a Delta table in
# the lakehouse. Portable by construction, and inspectable long after the run.
#
# `spark` is injected by the runtime. Nothing here imports the example's fixture
# package: a notebook runs on the pool, where only what the definition carries
# exists. Every assertion below is therefore an invariant of the TRANSFORM; the
# numbers the seeded generator implies are asserted by star_silver.py against
# these tables.

from pyspark.sql import functions as F

base = "abfs://{{WORKSPACE_ID}}@onelake.dfs.fabric.microsoft.com/{{LAKEHOUSE_ID}}/Tables"


def read(table):
    return spark.read.format("delta").load(f"{base}/{table}")


def write(df, table):
    df.write.format("delta").mode("overwrite").save(f"{base}/{table}")


def key(namespace, col):
    """A stable surrogate over a namespaced identity."""
    return F.sha2(F.concat(F.lit(namespace + ":"), col), 256)


# Country, conformed across FOUR spelling conventions.
#
# silver.py conforms POS's five variants and stops there, because POS is all it
# sees. Survivorship then used to coalesce that conformed value with the web
# store's RAW one, so `country` in the resolved dimension held ISO-2 for anyone
# POS knew, "United States" for the web-only, and NULL for the ERP-only —
# because ERP's country was never even selected. Three answers to one question,
# in the column whose entire job is to have one answer.
#
# Written out here rather than imported from the fixtures, for the reason
# silver.py gives: a rule derived from the generator's own COUNTRY_VARIANTS
# agrees with itself by construction, and a new upstream variant would conform
# silently instead of failing. Duplicated from silver.py for the same reason it
# is duplicated into the dbt model — it is silver's business rule, and silver
# has more than one implementation.
COUNTRY = {
    "US": "US", "USA": "US", "U.S.": "US", "UNITED STATES": "US",
    "GB": "GB", "GBR": "GB", "U.K.": "GB", "UK": "GB", "UNITED KINGDOM": "GB",
    "SG": "SG", "SGP": "SG", "SINGAPORE": "SG",
}
_conform = F.create_map([F.lit(x) for kv in COUNTRY.items() for x in kv])


def conform_country(col):
    """ISO-3166 alpha-2, or the upper-cased input if the rule does not know it.

    Falling through unchanged rather than to NULL is deliberate: an unknown
    spelling must stay visible so the assertion below fails on it, instead of
    being quietly erased into "we have no country for this person".
    """
    k = F.upper(F.trim(col))
    return F.coalesce(_conform[k], k)


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
                 F.col("full_name"),
                 conform_country(F.col("country")).alias("web_country")))

# ERP's country comes through CONFORMED like the others, where it used to be
# dropped on the floor. ERP spells countries ISO-3 (USA/GBR/SGP) — a fourth
# convention — and an ERP-only customer is someone no other system has ever
# seen, so this column is the only place their country can come from.
erp_now = (read("dim_customer_scd2").filter(F.col("is_current"))
           .select("erp_customer_id", "legal_name", "account_tier", "segment",
                   "credit_band", conform_country(F.col("country")).alias("erp_country"),
                   F.trim(F.col("phone").cast("string")).alias("phone_key")))

# An AMBIGUOUS key is not a match.
#
# POS phone numbers are not unique — the generator collides on a handful out of
# 100,000, which is realistic and would be far more common in a real CRM. If an
# ERP account's phone matches several POS customers, we cannot say WHICH person
# it is, and joining anyway does two bad things at once: it fans the row out, so
# one source record acquires several customer_keys and every downstream
# aggregate double-counts; and it asserts an identity nobody established.
#
# So a key that is not unique on the POS side is excluded from matching. The
# affected records then key on their own identity and join the unresolved
# cohorts, which is what resolve.py already says about people it cannot place.
# Guessing would be worse than not matching: a wrong merge is invisible, while
# an unmatched record is counted and reported.
def unambiguous(df, col):
    """The values of `col` that identify exactly one POS customer."""
    return (df.filter(F.col(col).isNotNull())
              .groupBy(col).agg(F.count("*").alias("_n"), F.first("customer_key").alias("customer_key"))
              .filter(F.col("_n") == 1)
              .select(col, "customer_key"))


pos_by_email = unambiguous(pos, "email_key")
pos_by_phone = unambiguous(pos, "phone_key")

# Left joins from web/erp onto those, never the reverse: a POS customer with no
# web or ERP counterpart must keep their key.
web_keyed = (web_c.join(pos_by_email, "email_key", "left")
             .withColumn("customer_key",
                         F.coalesce(F.col("customer_key"), key("web", F.col("email_key")))))

erp_keyed = (erp_now.join(pos_by_phone, "phone_key", "left")
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
    .join(erp_keyed.select("customer_key", "legal_name", "account_tier", "segment",
                           "credit_band", "erp_country"),
          "customer_key", "full_outer")
    .select(
        "customer_key",
        F.coalesce("full_name", "name", "legal_name").alias("name"),
        F.when(F.col("email") != "", F.col("email")).alias("email"),
        # POS first, then web, then ERP — all three now conformed, so the
        # precedence decides WHOSE ANSWER wins rather than which spelling
        # survives. ERP is appended rather than inserted: it can only fill rows
        # where neither of the other two holds a country, so no existing value
        # changes hands and the only rows affected are the ERP-only, which were
        # NULL before.
        F.coalesce("country", "web_country", "erp_country").alias("country"),
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

# --- invariants of the transform ---------------------------------------------
# Asserted here because a failing cell fails the job, and because none of these
# needs to know what the generator produced.

# Every source record maps to exactly one key, or the crosswalk is not a function
# and every downstream join fans out.
assert xref.count() == xref.select("source_system", "source_id").distinct().count(), \
    "a source record resolves to more than one customer_key"
assert conformed.select("customer_key").distinct().count() == conformed.count(), \
    "silver_customer_conformed is not one row per customer_key"
assert web_lines.filter(F.col("customer_key").isNull()).count() == 0, \
    "a web order line does not resolve to a customer"

# --- what the inputs looked like ---------------------------------------------
# One row, written as Delta like everything else. `amb_*` are the keys shared by
# more than one POS customer: a match nobody could safely make, which must be
# COUNTED rather than absorbed into a match rate. star_silver.py turns these into
# the assertions that need the fixture, and into what compare.py reads.
# An explicit schema rather than dict inference: one row is the case where
# inference buys nothing and engines disagree most.
metrics = spark.createDataFrame(
    [(erp_now.count(), web_c.count(),
      pos.filter(F.col("email_key").isNotNull()).count(), pos_by_email.count(),
      pos.filter(F.col("phone_key").isNotNull()).count(), pos_by_phone.count())],
    "erp_current long, web_customers long, pos_email_present long, "
    "pos_email_unambiguous long, pos_phone_present long, pos_phone_unambiguous long")
write(metrics, "silver_resolution_metrics")

print(f"materialised {xref.count()} xref rows, {conformed.count()} identities, "
      f"{web_lines.count()} web order lines")
