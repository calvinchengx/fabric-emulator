"""Materialise the resolution, so gold has a key to join on.

`resolve.py` computes the identity graph, asserts it and writes nothing — the
resolution exists there as a PROOF. A proof is not a table, and gold cannot join
three sources on an argument. This step turns it into two tables plus the web
fact grain:

  * `silver_customer_xref`      — (source_system, source_id) -> customer_key
  * `silver_customer_conformed` — one row per customer_key, attributes survived
  * `silver_web_order_lines`    — clean web lines carrying customer_key
  * `silver_resolution_metrics` — one row describing what the INPUTS held

THE TRANSFORM IS A NOTEBOOK: `definitions/star-silver.Notebook/`. It used to run
here over Spark Connect, which no Fabric tenant exposes, so this step could never
have run in production. Now it deploys the definition, submits a `RunNotebook`
job, and asserts against the tables that land — verifying the artifact rather than
a DataFrame it built itself.

The metrics table is how the numbers this step needs get out of the notebook. Its
assertions depend on quantities that exist only INSIDE the transform (how many
phone keys were shared by more than one POS customer, how many ERP and web
accounts arrived), and a notebook cannot return those through a job: real Fabric
exposes no exit value for a REST-submitted run. A Delta table is portable by
construction, and materialising run metrics is something real teams do anyway.

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
import json
import pathlib

import erp_system as erp
import source_system as src
import web_store as web
from common import (
    create_item_from_definition,
    load,
    log,
    report_lineage,
    run_job,
    storage_options,
    tables_uri,
)
from deltalake import DeltaTable

st = load()

nb = create_item_from_definition(
    "star-silver.Notebook",
    WORKSPACE_ID=st["workspace"], LAKEHOUSE_ID=st["lakehouse"])
jid, status = run_job(nb, "RunNotebook")
assert status == "Completed", f"star-silver notebook run {jid}: {status}"

# --- read back what landed ----------------------------------------------------
# delta-rs, not Spark: a different reader from the writer, so a Spark-shaped
# misunderstanding of the Delta log cannot agree with itself.
opts = storage_options()


def table(name):
    return DeltaTable(f"{tables_uri()}/{name}", storage_options=opts).to_pandas()


xref = table("silver_customer_xref")
conformed = table("silver_customer_conformed")
web_lines = table("silver_web_order_lines")
m = table("silver_resolution_metrics").iloc[0]

n_xref, n_conformed, n_lines = len(xref), len(conformed), len(web_lines)

# --- what the materialisation has to preserve --------------------------------
# Every source record maps to exactly one key, or the crosswalk is not a function
# and every downstream join fans out. (The notebook asserts this too; both are
# cheap, and the failure reads differently from each side.)
assert n_xref == len(xref.drop_duplicates(["source_system", "source_id"])), \
    "a source record resolves to more than one customer_key"
assert conformed["customer_key"].nunique() == n_conformed, \
    "silver_customer_conformed is not one row per customer_key"

# The unplaceable cohorts survive as their own identities. resolve.py proves they
# exist; this proves materialising did not quietly merge them away.
#
# The invariant, NOT a fixture total. Contoso ERP is a change log: a delete closes
# a customer's last version, so a deleted ERP-only customer has history rows and no
# current one, and correctly never reaches the star. Comparing a live count against
# EXPECTED_ERP_ONLY_COUNT — which counts everyone the source ever had — asserts
# that the dead are still here. What must hold is that no CURRENT account is lost:
# each one either bridged to POS or stands alone.
in_pos, in_web, in_erp = conformed["in_pos"], conformed["in_web"], conformed["in_erp"]
web_only = int((~in_pos & in_web).sum())
erp_only = int((~in_pos & in_erp).sum())
erp_bridged = int((in_pos & in_erp).sum())
web_bridged = int((in_pos & in_web).sum())

assert erp_bridged + erp_only == int(m["erp_current"]), \
    f"ERP accounts lost in the join: {erp_bridged} + {erp_only} != {int(m['erp_current'])}"
assert web_bridged + web_only == int(m["web_customers"]), \
    f"web accounts lost in the join: {web_bridged} + {web_only} != {int(m['web_customers'])}"

# Deletes and ambiguous keys can only SHRINK these cohorts, never grow them — an
# excess means the join invented an identity.
assert erp_only <= erp.EXPECTED_ERP_ONLY_COUNT, (erp_only, erp.EXPECTED_ERP_ONLY_COUNT)

# The exact cohort, as the star sees it: EXPECTED_ERP_ONLY_COUNT less the
# soft-deleted, plus the accounts whose phone is ambiguous in POS and so cannot
# match. The invariants above cannot see a wrong SPLIT between bridged and only —
# 100 accounts moving from one cohort to the other satisfies them both. This
# number catches that, and is computed in the fixture with the SAME `!= 1`
# ambiguity rule the transform applies, so a change to the resolution rule that is
# not carried into the fixture SHOULD fail here: the cohort really did change.
assert erp_only == erp.EXPECTED_ERP_ONLY_CURRENT, \
    (erp_only, erp.EXPECTED_ERP_ONLY_CURRENT)
assert web_only <= web.EXPECTED_WEB_ONLY_EMAIL_COUNT, \
    (web_only, web.EXPECTED_WEB_ONLY_EMAIL_COUNT)

assert n_lines == web.EXPECTED_WEB_CLEAN_LINES, (n_lines, web.EXPECTED_WEB_CLEAN_LINES)
assert web_lines["customer_key"].isnull().sum() == 0, \
    "a web order line does not resolve to a customer"

# CONFORMANCE, asserted rather than assumed. Four source conventions reach this
# column — POS's five variants, the web store's full names, ERP's ISO-3 — and the
# transform falls unknown spellings THROUGH unchanged, so a convention nobody
# taught it appears here as itself and fails. That is the whole point of not
# mapping to NULL: silent erasure would leave this set looking correct.
#
# NULL is absent from the expected set deliberately: every identity now reaches at
# least one system that states a country, so a NULL means a cohort lost its
# country on the way through survivorship.
countries = set(conformed["country"].dropna().unique()) | (
    {None} if conformed["country"].isnull().any() else set())
assert countries == src.EXPECTED_COUNTRIES, (sorted(map(str, countries)),
                                             sorted(src.EXPECTED_COUNTRIES))

multi = int((conformed["source_count"] > 1).sum())
# Say how many keys were too ambiguous to match on, rather than letting the match
# rate quietly absorb them. A phone shared by two customers is not a match nobody
# made — it is a match nobody could safely make.
amb_phone = int(m["pos_phone_present"]) - int(m["pos_phone_unambiguous"])
amb_email = int(m["pos_email_present"]) - int(m["pos_email_unambiguous"])
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
# Identity resolution really does read all three customer sets to write both the
# xref and the conformed dimension: survivorship is a full outer join, so that
# cross product is the truth. The web order-line grain is a SEPARATE movement over
# the web catalogue and lines, joined to the xref for its customer_key — and it
# never touched the ERP dimension.
_lake = st["lakehouse"]
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
