"""Contoso ERP — the third source system: the finance/back-office master.

POS sends a full snapshot every day. Contoso Web sends the current state of its
own world. This one sends neither: it emits an append-only **change log**, and
that single difference is the reason it exists.

  * **Change data capture, not a snapshot.** Each row is an event — `I` insert,
    `U` update, `D` delete — carrying the sequence it was captured at and the
    business time it took effect. Reconstructing "what did this customer look
    like on 2026-03-01" is only possible from a feed shaped like this, which is
    what makes **SCD2** and **incremental loading** real rather than aspirational.
  * **Soft deletes.** A `D` event does not remove history; it closes it. A
    pipeline that treats `D` as "drop the row" silently destroys the past.
  * **Out-of-order and late-arriving events.** Capture order and effective order
    disagree for a share of rows, exactly as a real CDC feed does when a
    connector retries or backfills. Ordering by the wrong column gives the wrong
    current state, and the fixture is built so that mistake shows up.
  * **Keyed by PHONE.** This system predates email; it has never held one. Phone
    is therefore the only key it shares with POS, and it shares NO key at all
    with Contoso Web. An ERP-to-Web match has to travel through POS — see
    resolve.py.
  * **Parquet on the wire**, not CSV or JSON, so the Copy activity ingests it
    through its ParquetSource path.

The overlap with POS is built from POS's own generated phone numbers
(source_system.customer_phones()), for the same reason web_store.py builds its
overlap from POS's emails: a hard-coded list would drift the moment the seed or
the scale changed.
"""
import collections
import io
import random

import pyarrow as pa
import pyarrow.parquet as pq
import source_system as pos

API_KEY = "erp-key-2291-dev"

# --- scale -------------------------------------------------------------------
SEED = 20260803  # neither POS's nor Web's
N_ERP_CUSTOMERS = 30_000
UPDATES_PER_CUSTOMER = 2.0  # mean number of U events after the initial I

# --- shape ratios ------------------------------------------------------------
OVERLAP_RATIO = 0.60  # ERP customers who also exist in POS, by phone
DELETED_RATIO = 0.04  # customers whose last event is a soft delete
# A backfill: captured LAST (highest capture_seq) but effective in the middle of
# that customer's own history. This is the hazard that makes capture order and
# business order genuinely different — a consumer that orders by capture_seq and
# takes the final row gets a version that was superseded months earlier.
LATE_ARRIVING_RATIO = 0.10

TIERS = ["standard", "preferred", "strategic", "key-account"]
SEGMENTS = ["smb", "mid-market", "enterprise", "public-sector"]
STATUSES = ["active", "on-hold", "closed"]
CREDIT_BANDS = ["A", "B", "C", "D"]

# The attributes an SCD2 dimension actually tracks. Kept deliberately small:
# a change feed's lesson is the SHAPE of the history, and a hundred columns of
# generated noise would bury it rather than enrich it.
TRACKED = ["legal_name", "account_tier", "segment", "credit_band",
           "account_status", "payment_terms_days", "country"]

# ERP spells country as ISO-3 — a fourth convention, agreeing with neither POS's
# free text nor Web's full names.
ERP_COUNTRIES = ["USA", "GBR", "SGP"]


def _attrs(rnd, i):
    return {
        "legal_name": f"Contoso Account {i:07d} Ltd",
        "account_tier": rnd.choice(TIERS),
        "segment": rnd.choice(SEGMENTS),
        "credit_band": rnd.choice(CREDIT_BANDS),
        "account_status": "active",
        "payment_terms_days": rnd.choice([14, 30, 45, 60, 90]),
        "country": ERP_COUNTRIES[i % len(ERP_COUNTRIES)],
    }


def _build():
    """Generate the change log plus its expectations, in one seeded pass."""
    rnd = random.Random(SEED)

    # --- who this system knows about ----------------------------------------
    # Phones POS actually issued, so the join has something to find. POS's
    # missing-email cohort is irrelevant here: ERP joins on phone, and that is
    # precisely why it can reach people Web never can.
    pos_phones = [p for p in pos.customer_phones() if p]
    # POS phones are not unique — the generator collides on a handful out of
    # 100,000, which is realistic. Which ones matters to the expectations below,
    # because an ambiguous key cannot be matched on.
    _pos_phone_counts = collections.Counter(pos_phones)
    n_shared = min(int(N_ERP_CUSTOMERS * OVERLAP_RATIO), len(pos_phones))
    shared = rnd.sample(pos_phones, n_shared)
    erp_only = [f"+00{i:07d}" for i in range(N_ERP_CUSTOMERS - n_shared)]
    phones = shared + erp_only

    deleted = set(rnd.sample(range(len(phones)), int(len(phones) * DELETED_RATIO)))
    late = set(rnd.sample(range(len(phones)), int(len(phones) * LATE_ARRIVING_RATIO)))

    rows = []
    seq = 0
    n_updates = n_deletes = n_late = 0

    for i, phone in enumerate(phones):
        erp_id = f"E-{i:07d}"
        attrs = _attrs(rnd, i)

        # The insert: day one of this account's history.
        first_day = day = rnd.randint(1, 120)
        seq += 1
        rows.append({"op": "I", "capture_seq": seq, "erp_customer_id": erp_id,
                     "phone": phone, "effective_date": _day(day), **attrs})

        # Updates: each one changes exactly one tracked attribute, so an SCD2
        # build that misses a column is visible as a missing version rather than
        # a subtly wrong one.
        for _ in range(rnd.randint(0, int(UPDATES_PER_CUSTOMER * 2))):
            day += rnd.randint(5, 60)
            field = rnd.choice([f for f in TRACKED if f != "country"])
            if field == "account_tier":
                attrs["account_tier"] = rnd.choice(TIERS)
            elif field == "segment":
                attrs["segment"] = rnd.choice(SEGMENTS)
            elif field == "credit_band":
                attrs["credit_band"] = rnd.choice(CREDIT_BANDS)
            elif field == "account_status":
                attrs["account_status"] = rnd.choice(STATUSES)
            elif field == "payment_terms_days":
                attrs["payment_terms_days"] = rnd.choice([14, 30, 45, 60, 90])
            else:
                attrs["legal_name"] = attrs["legal_name"].replace(" Ltd", " PLC")
            seq += 1
            n_updates += 1
            rows.append({"op": "U", "capture_seq": seq, "erp_customer_id": erp_id,
                         "phone": phone, "effective_date": _day(day), **attrs})

        if i in deleted:
            day += rnd.randint(5, 60)
            seq += 1
            n_deletes += 1
            rows.append({"op": "D", "capture_seq": seq, "erp_customer_id": erp_id,
                         "phone": phone, "effective_date": _day(day),
                         **{**attrs, "account_status": "closed"}})

        # The backfill. Captured after everything above (highest capture_seq for
        # this customer) but effective back near the start of its history, so
        # capture order and business order genuinely disagree WITHIN one
        # customer. Deleted accounts are left out of this: "a correction
        # arriving after the close" is a different problem, and mixing the two
        # would make the expected version count ambiguous.
        if i in late and i not in deleted and day > first_day + 2:
            seq += 1
            n_late += 1
            rows.append({"op": "U", "capture_seq": seq, "erp_customer_id": erp_id,
                         "phone": phone,
                         "effective_date": _day(rnd.randint(first_day + 1, day - 1)),
                         **{**attrs, "credit_band": "D"}})

    exp = {
        "EXPECTED_ERP_CUSTOMERS": len(phones),
        "EXPECTED_ERP_CHANGE_EVENTS": len(rows),
        "EXPECTED_ERP_INSERTS": len(phones),
        "EXPECTED_ERP_UPDATES": n_updates + n_late,
        "EXPECTED_ERP_DELETES": n_deletes,
        # An SCD2 dimension has one row per version, which is exactly one row per
        # change event — the delete closes a version rather than adding one.
        "EXPECTED_SCD2_VERSIONS": len(rows) - n_deletes,
        "EXPECTED_SCD2_CURRENT": len(phones) - n_deletes,
        "EXPECTED_ERP_SHARED_PHONE_COUNT": n_shared,
        "EXPECTED_ERP_ONLY_COUNT": len(phones) - n_shared,
        # The same cohort as the star sees it, which is NOT the line above.
        #
        # EXPECTED_ERP_ONLY_COUNT is this generator's intent: accounts we did
        # not give a POS phone to. Two things move the number the star actually
        # counts, and both are real rather than bookkeeping:
        #
        #   - a soft-deleted account is not is_current, so it is absent from the
        #     SCD2 dimension the star reads (~476 of the 12,000 here);
        #   - an account whose phone is AMBIGUOUS in POS cannot be matched — a
        #     phone shared by two customers identifies nobody — so it falls back
        #     to its own identity and joins this cohort (2 here).
        #
        # star_silver.py asserting the intent against the observation was worth
        # 11,526 vs 12,000, and the gap decomposed exactly into those two terms.
        #
        # `!= 1` rather than `> 1` on purpose: it is the same test the star
        # applies (a key must identify EXACTLY one POS customer), so this stays
        # coupled to the resolution rule instead of drifting the next time it
        # changes. It is the coupling that makes the assertion worth having —
        # invariants alone hold even if accounts move between cohorts.
        "EXPECTED_ERP_ONLY_CURRENT": sum(
            1 for i, p in enumerate(phones)
            if i not in deleted and (i >= n_shared or _pos_phone_counts[p] != 1)
        ),
        # Customers whose final row by capture_seq is NOT their final row by
        # effective_date. 24_erp_scd2.py asserts the two orderings disagree by
        # exactly this many, so the hazard is measured rather than asserted.
        "EXPECTED_LATE_ARRIVING": n_late,
    }
    return rows, exp


def _day(offset):
    """A date `offset` days after 2026-01-01, as an ISO string."""
    import datetime
    return (datetime.date(2026, 1, 1) + datetime.timedelta(days=offset)).isoformat()


_CACHE = None


def _built():
    global _CACHE
    if _CACHE is None:
        _CACHE = _build()
    return _CACHE


def __getattr__(name):
    """Serve the EXPECTED_* values without generating at import time."""
    if name.startswith("EXPECTED_"):
        exp = _built()[1]
        if name in exp:
            return exp[name]
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


def export(api_key):
    """The ERP's CDC extract. Parquet, because that is what a CDC connector
    lands — and because it makes the Copy activity read a columnar source."""
    if api_key != API_KEY:
        raise PermissionError("Contoso ERP: invalid API key")
    rows, _ = _built()
    cols = ["op", "capture_seq", "erp_customer_id", "phone", "effective_date"] + TRACKED
    table = pa.table({c: pa.array([r[c] for r in rows]) for c in cols})
    buf = io.BytesIO()
    pq.write_table(table, buf)
    return {"changes.parquet": buf.getvalue()}
