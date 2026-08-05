"""A6 — resolve three customer sets into one, transitively.

Each system knows people by a different key, and no key spans all three:

    Contoso POS  <--email-->  Contoso Web
         ^
         | phone
         v
    Contoso ERP  <--- no shared key at all ---> Contoso Web

So an ERP account can only ever reach a Web account by travelling THROUGH POS.
That is the structural point of having a third source: a two-system example can
always be joined, and never has to confront the fact that identity is a graph.

The honest part of this step is what it refuses to claim. Three cohorts cannot
be resolved, and each is unreachable for a different reason:

  * POS customers with **no email** — invisible to Web no matter how well ERP
    knows them, because the POS-to-Web edge does not exist for them.
  * ERP accounts with **no POS phone match** — no first hop, so no path at all.
  * Web accounts that exist **only** on the web — never seen by either of the
    other two.

A resolution step that reports a single match rate, without naming who it could
not place, is hiding all three.
"""
import erp_system as erp
import web_store as web
from common import log, storage_options, tables_uri
from deltalake import DeltaTable

opts = storage_options()
base = tables_uri()


def read(table):
    return DeltaTable(f"{base}/{table}", storage_options=opts).to_pandas()


pos = read("bronze_customers").drop_duplicates(subset=["customer_id"], keep="last")
web_c = read("bronze_web_customers")
scd2 = read("dim_customer_scd2")
erp_now = scd2[scd2["is_current"]]

# Case-fold before joining: POS stores a share of addresses as the customer
# typed them, and an exact join would treat those people as strangers.
pos = pos.assign(email_key=pos["email"].fillna("").str.strip().str.lower(),
                 phone_key=pos["phone"].astype(str).str.strip())
web_c = web_c.assign(email_key=web_c["email"].str.strip().str.lower())
erp_now = erp_now.assign(phone_key=erp_now["phone"].astype(str).str.strip())

has_email = pos["email_key"] != ""

# --- edge 1: POS <-> Web, on email ------------------------------------------
web_emails = set(web_c["email_key"])
pos_to_web = pos[has_email & pos["email_key"].isin(web_emails)]
assert len(pos_to_web) == web.EXPECTED_SHARED_EMAIL_COUNT, len(pos_to_web)

# --- edge 2: POS <-> ERP, on phone ------------------------------------------
erp_phones = set(erp_now["phone_key"])
pos_to_erp = pos[pos["phone_key"].isin(erp_phones)]

# --- the transitive hop: ERP -> POS -> Web ----------------------------------
bridged = pos[pos["phone_key"].isin(erp_phones)
              & has_email & pos["email_key"].isin(web_emails)]
log(f"edges: POS<->Web {len(pos_to_web):,} on email, "
    f"POS<->ERP {len(pos_to_erp):,} on phone")
log(f"transitive: {len(bridged):,} ERP accounts reach a Web account through POS "
    f"— there is no direct key between those two systems")

# The bridge is a subset of both edges, by construction. Asserting it rather
# than trusting it catches a join that silently fanned out.
assert len(bridged) <= len(pos_to_web) and len(bridged) <= len(pos_to_erp)
assert bridged["customer_id"].is_unique, "the transitive join fanned out"

# --- who cannot be resolved, and why ----------------------------------------
# 1. Known to ERP by phone, but POS holds no email for them: the second hop
#    does not exist. These people are NOT missing from the business — they are
#    missing from the join, which is a different and more dangerous thing.
stranded = pos[pos["phone_key"].isin(erp_phones) & ~has_email]
assert len(stranded) > 0, "the fixture no longer exercises a broken second hop"

# 2. ERP accounts with no POS phone at all — no first hop.
erp_unreachable = erp.EXPECTED_ERP_ONLY_COUNT

# 3. Web-only accounts, seen by nobody else.
web_only = web.EXPECTED_WEB_ONLY_EMAIL_COUNT

resolved = len(bridged)
log(f"unresolved: {len(stranded):,} known to ERP but with no POS email "
    f"(second hop missing), {erp_unreachable:,} ERP-only (no first hop), "
    f"{web_only:,} web-only (seen by nobody else)")

# The number that must never be reported alone.
total_identities = (pos["customer_id"].nunique()
                    + web.EXPECTED_WEB_ONLY_EMAIL_COUNT
                    + erp.EXPECTED_ERP_ONLY_COUNT)
log(f"{total_identities:,} distinct identities across three systems; "
    f"{resolved:,} span all three. A single 'match rate' would hide the rest.")
