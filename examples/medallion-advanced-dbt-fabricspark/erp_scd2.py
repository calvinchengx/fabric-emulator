"""A5 — turn the ERP change log into a slowly-changing dimension (SCD2).

A snapshot feed can only ever tell you what is true now. A change log can tell
you what was true on any date, and that is the whole reason ERP exists in this
example: `dim_customer_scd2` carries one row per VERSION, with `valid_from`,
`valid_to` and `is_current`, so a fact can be joined to the version of the
customer that applied when the fact happened.

Two decisions are load-bearing, and both are easy to get wrong:

  * **Order by `effective_date`, not `capture_seq`.** They disagree: a backfill
    is captured last but takes effect in the middle of that customer's history.
    Ordering by capture order gives a "current" version that was superseded
    months earlier. This step MEASURES the disagreement rather than asserting
    it — if the fixture ever stopped exercising the hazard, the assert fails.
  * **A delete closes history, it does not erase it.** A `D` event ends the
    open version and adds none of its own. Treating `D` as "drop the customer"
    would silently destroy every past version too.
"""
import erp_system as erp
from common import log, storage_options, tables_uri
from deltalake import DeltaTable, write_deltalake

opts = storage_options()
base = tables_uri()

ch = DeltaTable(f"{base}/bronze_erp_changes", storage_options=opts).to_pandas()
assert len(ch) == erp.EXPECTED_ERP_CHANGE_EVENTS, len(ch)

# --- the hazard, measured ----------------------------------------------------
# The last row per customer by capture order vs by business order. If those
# agreed, ordering by capture_seq would be harmless and this step would be
# teaching nothing.
by_capture = (ch.sort_values("capture_seq").groupby("erp_customer_id")
                .tail(1).set_index("erp_customer_id")["capture_seq"].sort_index())
by_effective = (ch.sort_values(["effective_date", "capture_seq"])
                  .groupby("erp_customer_id")
                  .tail(1).set_index("erp_customer_id")["capture_seq"].sort_index())
# .tail(1) keeps original row order, which differs between the two sorts — align
# on the key before comparing, or pandas refuses (and rightly so).
disagree = int((by_capture != by_effective).sum())
assert disagree == erp.EXPECTED_LATE_ARRIVING, (disagree, erp.EXPECTED_LATE_ARRIVING)
log(f"capture order and business order disagree for {disagree:,} customers "
    f"— ordering by capture_seq would pick a superseded version for each")

# --- build the dimension -----------------------------------------------------
# Business order within each customer. capture_seq only breaks ties on the same
# effective_date, where it IS the right tiebreak: same day, later capture wins.
ch = ch.sort_values(["erp_customer_id", "effective_date", "capture_seq"]).copy()

# The next event's effective date closes the current version — including when
# that next event is the delete, which is how a close gets its end date.
ch["closes_at"] = ch.groupby("erp_customer_id")["effective_date"].shift(-1)

versions = ch[ch["op"] != "D"].copy()
versions["valid_from"] = versions["effective_date"]
versions["valid_to"] = versions["closes_at"]
versions["is_current"] = versions["valid_to"].isna()
versions["version_no"] = versions.groupby("erp_customer_id").cumcount() + 1

scd2 = versions[["erp_customer_id", "phone", "version_no", "valid_from", "valid_to",
                 "is_current", "legal_name", "account_tier", "segment",
                 "credit_band", "account_status", "payment_terms_days", "country"]]

# reset_index: the frame was filtered, and a non-contiguous index would be
# persisted as a phantom `__index_level_0__` column in the Delta table.
write_deltalake(f"{base}/dim_customer_scd2", scd2.reset_index(drop=True),
                mode="overwrite", storage_options=opts)

assert len(scd2) == erp.EXPECTED_SCD2_VERSIONS, len(scd2)
assert int(scd2["is_current"].sum()) == erp.EXPECTED_SCD2_CURRENT, int(scd2["is_current"].sum())

# Exactly one open version per surviving customer — an SCD2 with two open rows
# for the same key is the classic bug, and it is silent until someone joins.
open_per_customer = scd2[scd2["is_current"]].groupby("erp_customer_id").size()
assert open_per_customer.max() == 1, open_per_customer.value_counts().to_dict()

# Deleted accounts keep their history and have NO open version.
closed = erp.EXPECTED_ERP_CUSTOMERS - erp.EXPECTED_SCD2_CURRENT
assert scd2["erp_customer_id"].nunique() == erp.EXPECTED_ERP_CUSTOMERS, \
    scd2["erp_customer_id"].nunique()

# History is contiguous: each version starts exactly where the previous ended.
chk = scd2.sort_values(["erp_customer_id", "version_no"]).copy()
chk["prev_to"] = chk.groupby("erp_customer_id")["valid_to"].shift(1)
gap = chk[chk["version_no"] > 1]
assert (gap["valid_from"] == gap["prev_to"]).all(), "SCD2 history has a gap or overlap"

log(f"dim_customer_scd2: {len(scd2):,} versions over "
    f"{scd2['erp_customer_id'].nunique():,} customers "
    f"({int(scd2['is_current'].sum()):,} current, {closed:,} closed by a soft delete)")

# --- point-in-time lookup: what the dimension is FOR --------------------------
asof = "2026-03-01"
pit = scd2[(scd2["valid_from"] <= asof)
           & ((scd2["valid_to"].isna()) | (scd2["valid_to"] > asof))]
assert pit["erp_customer_id"].is_unique, "point-in-time lookup returned two rows for a customer"
log(f"as of {asof}: {len(pit):,} customers had a version in effect "
    f"(one row each, which is the property that makes the join safe)")
