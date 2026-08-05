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
import source_system as src
import web_store as web
from common import SPARK_REMOTE, load, log, report_lineage

st = load()
assert SPARK_REMOTE, "SPARK_REMOTE is empty — no Spark engine is attached"

from pyspark.sql import SparkSession  # noqa: E402
from pyspark.sql import functions as F  # noqa: E402

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
#
# The invariant, NOT a fixture total. Contoso ERP is a change log: a delete
# closes a customer's last version, so a deleted ERP-only customer has history
# rows and no current one, and correctly never reaches the star. Comparing a
# live count against EXPECTED_ERP_ONLY_COUNT — which counts everyone the source
# ever had — asserts that the dead are still here. What must hold is that no
# CURRENT account is lost: each one either bridged to POS or stands alone.
web_only = conformed.filter(~F.col("in_pos") & F.col("in_web")).count()
erp_only = conformed.filter(~F.col("in_pos") & F.col("in_erp")).count()
erp_bridged = conformed.filter(F.col("in_pos") & F.col("in_erp")).count()
web_bridged = conformed.filter(F.col("in_pos") & F.col("in_web")).count()

assert erp_bridged + erp_only == erp_now.count(), \
    f"ERP accounts lost in the join: {erp_bridged} + {erp_only} != {erp_now.count()}"
assert web_bridged + web_only == web_c.count(), \
    f"web accounts lost in the join: {web_bridged} + {web_only} != {web_c.count()}"

# Deletes and ambiguous keys can only SHRINK these cohorts, never grow them —
# an excess means the join invented an identity.
assert erp_only <= erp.EXPECTED_ERP_ONLY_COUNT, (erp_only, erp.EXPECTED_ERP_ONLY_COUNT)

# The exact cohort, as the star sees it: EXPECTED_ERP_ONLY_COUNT less the
# soft-deleted, plus the accounts whose phone is ambiguous in POS and so cannot
# match. The invariants above cannot see a wrong SPLIT between bridged and only
# — 100 accounts moving from one cohort to the other satisfies them both. This
# number catches that, and is computed in the fixture with the SAME `!= 1`
# ambiguity rule this step applies, so a change to the resolution rule that is
# not carried into the fixture SHOULD fail here: the cohort really did change.
assert erp_only == erp.EXPECTED_ERP_ONLY_CURRENT, \
    (erp_only, erp.EXPECTED_ERP_ONLY_CURRENT)
assert web_only <= web.EXPECTED_WEB_ONLY_EMAIL_COUNT, \
    (web_only, web.EXPECTED_WEB_ONLY_EMAIL_COUNT)

assert n_lines == web.EXPECTED_WEB_CLEAN_LINES, (n_lines, web.EXPECTED_WEB_CLEAN_LINES)
assert web_lines.filter(F.col("customer_key").isNull()).count() == 0, \
    "a web order line does not resolve to a customer"

# CONFORMANCE, asserted rather than assumed. Four source conventions reach this
# column — POS's five variants, the web store's full names, ERP's ISO-3 — and
# conform_country falls unknown spellings THROUGH unchanged, so a convention
# nobody taught it appears here as itself and fails. That is the whole point of
# not mapping to NULL: silent erasure would leave this set looking correct.
#
# NULL is absent from the expected set deliberately: every identity now reaches
# at least one system that states a country, so a NULL means a cohort lost its
# country on the way through survivorship.
countries = {r["country"] for r in conformed.select("country").distinct().collect()}
assert countries == src.EXPECTED_COUNTRIES, (sorted(map(str, countries)),
                                             sorted(src.EXPECTED_COUNTRIES))

multi = conformed.filter(F.col("source_count") > 1).count()
# Say how many keys were too ambiguous to match on, rather than letting the
# match rate quietly absorb them. A phone shared by two customers is not a
# match nobody made — it is a match nobody could safely make.
amb_phone = pos.filter(F.col("phone_key").isNotNull()).select("phone_key").count() \
    - pos_by_phone.count()
amb_email = pos.filter(F.col("email_key").isNotNull()).select("email_key").count() \
    - pos_by_email.count()
log(f"materialised: {n_xref:,} source records -> {n_conformed:,} identities "
    f"({multi:,} multi-source, {web_only:,} web-only, {erp_only:,} erp-only)")
if amb_phone or amb_email:
    log(f"ambiguous keys excluded from matching: {amb_phone:,} phone, "
        f"{amb_email:,} email — shared by more than one POS customer, so no "
        f"match could be made safely")
log(f"web fact grain: {n_lines:,} clean order lines, all resolved to a customer_key")

# The resolution is the advanced example's whole claim, so it belongs in the
# graph — reported as the derivations the code actually computes.
#
# Identity resolution really does read all three customer sets to write both
# the xref and the conformed dimension: survivorship is a full outer join, so
# that cross product is the truth. The web order-line grain is a SEPARATE
# movement over the web catalogue and lines, joined to the xref for its
# customer_key — and it never touched the ERP dimension.
_lake = load()["lakehouse"]
report_lineage("star_silver", [
    ([(_lake, "Tables/silver_customers"), (_lake, "Tables/bronze_web_customers"),
      (_lake, "Tables/dim_customer_scd2")],
     [(_lake, "Tables/silver_customer_xref"), (_lake, "Tables/silver_customer_conformed")]),
    ([(_lake, "Tables/bronze_web_products"), (_lake, "Tables/bronze_web_order_lines"),
      (_lake, "Tables/silver_customer_xref")],
     [(_lake, "Tables/silver_web_order_lines")]),
])

# --- what compare.py reads ----------------------------------------------------
# The advanced pair's claim is stronger than the simple pair's. There, two silver
# engines are shown to agree on SILVER. Here the question is whether the engine
# choice perturbs the IDENTITY RESOLUTION built on top of it — a harder thing to
# get right and a quieter thing to get wrong, because the cohorts can shift
# between each other while every row count stays put.
#
# This step is byte-identical in both examples (scripts/check_example_parity.py
# enforces it), so any difference in these numbers came from silver, which is the
# only thing that differs. That is what makes the comparison attributable.
#
# The example NAMES ITSELF from its directory rather than carrying an engine
# label: a hardcoded label would be the one line that differs between two files
# required to be identical.
import json  # noqa: E402
import pathlib  # noqa: E402

_here = pathlib.Path(__file__).resolve().parent
_here.joinpath("star_silver_summary.json").write_text(json.dumps({
    "example": _here.name,
    "rows": {
        "silver_customer_xref": n_xref,
        "silver_customer_conformed": n_conformed,
        "silver_web_order_lines": n_lines,
    },
    # The cohorts, not just the totals. `multi_source + web_only + erp_only` can
    # hold steady while a hundred people move between them, and a row-count
    # comparison would report that as agreement.
    "cohorts": {
        "multi_source": multi,
        "web_only": web_only,
        "erp_only": erp_only,
        "erp_bridged": erp_bridged,
        "web_bridged": web_bridged,
    },
    # An ambiguous key is a match nobody could safely make. If one engine's
    # silver produced a different number of them, the two resolutions are not
    # comparable however well their totals line up.
    "ambiguous_keys_excluded": {"phone": amb_phone, "email": amb_email},
    "countries": sorted(countries),
}, indent=2))
