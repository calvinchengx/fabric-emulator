"""Contoso POS — the fictitious SaaS source system, generated at scale.

Every row is generated from a fixed seed, so the export is large but exactly
reproducible: the same seed yields identical bytes on every machine and every
run. That matters because the pipeline asserts against it — a dataset that
drifted between runs could only ever be checked loosely.

Deliberately messy, so silver has real work. The defects are the same ones a
real batch feed carries, but they are injected by RATIO rather than hand-placed:

  * at-least-once redelivery — an order event arrives twice, latest wins
  * duplicated customer rows
  * five spellings across three countries
  * missing emails (nobody can resolve these downstream)
  * malformed orders (negative quantity, no price) — quarantined, not dropped

Injecting by ratio is what makes the defects survive a change of scale. Pinning
them to named rows, as this file used to, meant the teaching cases silently
became a rounding error the moment the row count grew.

The EXPECTED_* values below are computed from the GENERATOR's own decisions —
it knows how many duplicates and malformed rows it planted — not by replaying
silver's transforms. That keeps the downstream asserts honest: silver has to
independently arrive at the number the generator planted, so a regression in the
transforms still fails the pipeline instead of agreeing with itself.

Nothing here is emulator-specific — it is the "vendor API" the pipeline pulls
from, and it refuses to export without the API key held in Key Vault.
"""
import io
import json
import random

API_KEY = "pos-key-8843-dev"

# --- scale -------------------------------------------------------------------
# The old ceiling here was the lakehouse SQL endpoint, which reflected Delta
# into the sidecar as INSERT ... VALUES text and took 600s+ on a table this
# shape. Reflection now uses the TDS bulk-copy protocol, so 100k x 100 lands in
# ~2.4s and the Copy activity ingests it in ~1.9s. What is left to watch is
# MEMORY: the emulator holds OneLake in its SQLite store, in-memory by default
# (FABRIC_DATA_DIR is set empty in docker-compose.yml), so every bronze and
# silver table is resident at once.
SEED = 20260801
N_CUSTOMERS = 100_000
N_ORDERS = 250_000

# --- defect ratios -----------------------------------------------------------
DUPLICATE_CUSTOMER_RATIO = 0.02  # exact-duplicate customer rows in the export
MISSING_EMAIL_RATIO = 0.03  # no email at all -> unresolvable downstream
MIXED_CASE_EMAIL_RATIO = 0.10  # Ben.Okafor@Example.com — same person, other case
COUNTRY_VARIANT_RATIO = 0.20  # spelled some other way than the canonical code
REDELIVERY_RATIO = 0.02  # order event delivered twice (at-least-once)
MALFORMED_RATIO = 0.01  # negative quantity and no price

# Canonical codes silver must conform to, and the spellings this vendor emits.
# Every variant maps back to exactly one canonical code — conformance is the
# point, so an unmappable spelling would be a bug in the generator rather than
# a lesson.
COUNTRY_VARIANTS = {
    "US": ["US", "USA", "us", "U.S.", "United States"],
    "GB": ["GB", "U.K.", "uk", "GBR", "United Kingdom"],
    "SG": ["SG", "sg", "SGP", "Singapore", "singapore"],
}
CANONICAL_COUNTRIES = sorted(COUNTRY_VARIANTS)

PRODUCTS = [
    ("P-100", "Widget", "Hardware", 24.50),
    ("P-200", "Gadget Pro", "Hardware", 129.00),
    ("P-300", "Sticker Pack", "Accessories", 9.90),
    ("P-400", "Cable", "Accessories", 4.20),
    ("P-500", "Workstation", "Hardware", 349.00),
    ("P-600", "Dock", "Hardware", 189.00),
    ("P-700", "Sleeve", "Accessories", 19.90),
    ("P-800", "Keyboard", "Hardware", 79.00),
]

FIRST_NAMES = ["Ava", "Ben", "Carla", "Dev", "Emi", "Farid", "Grace", "Hana", "Ivan",
               "Jun", "Kira", "Liam", "Mia", "Noor", "Omar", "Priya", "Quinn", "Rosa",
               "Sam", "Tara", "Umar", "Vera", "Wei", "Xena", "Yuki", "Zane"]
LAST_NAMES = ["Chen", "Okafor", "Diaz", "Patel", "Sato", "Rahman", "Lim", "Kim",
              "Petrov", "Nakamura", "Silva", "Haddad", "Novak", "Muller", "Rossi",
              "Dubois", "Andersen", "Kowalski", "Fernandez", "Adeyemi"]

# --- the wide customer record ------------------------------------------------
# A customer-360 table, which is where width in a real POS export comes from:
# identity, address, loyalty, preferences, demographics, lifecycle, support, and
# the scored/affinity families a marketing system bolts on. The names are
# meaningful because a hundred columns called col_001 would exercise the same
# code paths while teaching nothing about what a wide table is actually like.
LOYALTY_TIERS = ["bronze", "silver", "gold", "platinum"]
CHANNELS = ["store", "web", "app", "phone", "partner"]
SEGMENTS = ["value", "mainstream", "premium", "lapsed", "new"]
AFFINITY_CATEGORIES = ["hardware", "accessories", "software", "services", "bundles",
                       "refurb", "warranty", "training", "support", "trade_in"]
FLAG_NAMES = ["is_employee", "is_reseller", "is_vip", "has_open_dispute",
              "is_tax_exempt", "accepts_backorder", "is_test_account",
              "has_stored_card", "is_gdpr_subject", "is_sanctions_screened"]


def _customer_columns():
    """The wide schema, as (name, kind) pairs. Kind drives value generation."""
    cols = [
        ("customer_id", "id"), ("first_name", "first"), ("last_name", "last"),
        ("name", "full"), ("email", "email"), ("phone", "phone"),
        ("country", "country"), ("signup_date", "date"),
        ("address_line1", "street"), ("address_line2", "street2"), ("city", "city"),
        ("state_province", "region"), ("postal_code", "postcode"),
        ("latitude", "lat"), ("longitude", "lon"), ("address_type", "addrtype"),
        ("loyalty_tier", "tier"), ("loyalty_points", "int_0_50000"),
        ("loyalty_since", "date"), ("lifetime_value", "money"),
        ("last_purchase_date", "date"), ("purchase_count", "int_0_500"),
        ("avg_basket_value", "money"),
        ("pref_language", "lang"), ("pref_currency", "ccy"),
        ("opt_in_email", "bool"), ("opt_in_sms", "bool"), ("opt_in_push", "bool"),
        ("marketing_segment", "segment"), ("preferred_channel", "channel"),
        ("preferred_store", "store"),
        ("birth_year", "birthyear"), ("household_size", "int_1_7"),
        ("income_band", "band"), ("occupation_code", "occ"),
        ("education_level", "edu"),
        ("account_status", "status"), ("created_ts", "ts"), ("updated_ts", "ts"),
        ("last_login_ts", "ts"), ("churn_risk_score", "score"), ("nps_score", "nps"),
        ("support_tickets_open", "int_0_5"), ("support_tickets_total", "int_0_50"),
        ("last_ticket_date", "date"), ("satisfaction_score", "score"),
        ("escalation_flag", "bool"),
    ]
    cols += [(f"affinity_{c}", "score") for c in AFFINITY_CATEGORIES]
    cols += [(f"segment_score_{i:02d}", "score") for i in range(1, 21)]
    cols += [(f"flag_{n}", "bool") for n in FLAG_NAMES]
    cols += [(f"attr_{i:02d}", "attr") for i in range(1, 15)]
    return cols


CUSTOMER_COLUMNS = _customer_columns()

# Order events are narrower than customers, as a fact feed is: the width lives
# on the dimension. These are the columns bronze_orders carries.
ORDER_FIELDS = ["order_id", "customer_id", "product_id", "order_date", "channel",
                "store_id", "currency", "discount_pct", "tax_rate", "shipping_fee",
                "payment_method", "is_gift", "promo_code", "quantity", "unit_price",
                "status", "event_seq"]


def _value(kind, rnd, i):
    """One synthetic cell. Everything here is CSV-safe: no commas, no quotes."""
    if kind == "id":
        return f"C-{i:07d}"
    if kind == "first":
        return FIRST_NAMES[i % len(FIRST_NAMES)]
    if kind == "last":
        return LAST_NAMES[(i // len(FIRST_NAMES)) % len(LAST_NAMES)]
    if kind == "full":
        return (f"{FIRST_NAMES[i % len(FIRST_NAMES)]} "
                f"{LAST_NAMES[(i // len(FIRST_NAMES)) % len(LAST_NAMES)]}")
    if kind == "email":
        return ""  # filled in by the caller, which knows the name it must match
    if kind == "country":
        return ""  # ditto: the caller picks canonical vs variant spelling
    if kind == "phone":
        return f"+{rnd.randint(1, 99)}{rnd.randint(1000000, 9999999)}"
    if kind == "date":
        return f"202{rnd.randint(3, 6)}-{rnd.randint(1, 12):02d}-{rnd.randint(1, 28):02d}"
    if kind == "ts":
        return (f"202{rnd.randint(3, 6)}-{rnd.randint(1, 12):02d}-"
                f"{rnd.randint(1, 28):02d}T{rnd.randint(0, 23):02d}:"
                f"{rnd.randint(0, 59):02d}:00Z")
    if kind == "street":
        return f"{rnd.randint(1, 999)} {rnd.choice(['Oak', 'Main', 'Cedar', 'Elm', 'Pine'])} St"
    if kind == "street2":
        return rnd.choice(["", f"Unit {rnd.randint(1, 40)}", f"Level {rnd.randint(1, 20)}"])
    if kind == "city":
        return rnd.choice(["Springfield", "Riverton", "Lakeside", "Fairview", "Georgetown"])
    if kind == "region":
        return rnd.choice(["CA", "NY", "TX", "WA", "IL", "MA"])
    if kind == "postcode":
        return str(rnd.randint(10000, 99999))
    if kind == "lat":
        return f"{rnd.uniform(-90, 90):.5f}"
    if kind == "lon":
        return f"{rnd.uniform(-180, 180):.5f}"
    if kind == "addrtype":
        return rnd.choice(["billing", "shipping", "both"])
    if kind == "tier":
        return rnd.choice(LOYALTY_TIERS)
    if kind == "money":
        return f"{rnd.uniform(0, 9999):.2f}"
    if kind == "score":
        return f"{rnd.random():.4f}"
    if kind == "nps":
        return str(rnd.randint(0, 10))
    if kind == "lang":
        return rnd.choice(["en", "fr", "de", "ja", "zh"])
    if kind == "ccy":
        return rnd.choice(["USD", "GBP", "SGD", "EUR"])
    if kind == "bool":
        return rnd.choice(["true", "false"])
    if kind == "segment":
        return rnd.choice(SEGMENTS)
    if kind == "channel":
        return rnd.choice(CHANNELS)
    if kind == "store":
        return f"S-{rnd.randint(1, 400):04d}"
    if kind == "birthyear":
        return str(rnd.randint(1945, 2006))
    if kind == "band":
        return rnd.choice(["<25k", "25-50k", "50-75k", "75-100k", "100k+"])
    if kind == "occ":
        return f"OCC-{rnd.randint(1, 99):02d}"
    if kind == "edu":
        return rnd.choice(["hs", "college", "bachelor", "master", "doctorate"])
    if kind == "status":
        return rnd.choice(["active", "dormant", "closed"])
    if kind == "attr":
        return f"v{rnd.randint(1, 9999)}"
    if kind.startswith("int_"):
        _, lo, hi = kind.split("_")
        return str(rnd.randint(int(lo), int(hi)))
    raise ValueError(f"unknown kind {kind!r}")


def _build():
    """Generate the whole export plus its expectations, in one seeded pass.

    Returns (customers_csv_bytes, orders_jsonl_bytes, expectations_dict).
    """
    rnd = random.Random(SEED)
    names = [c for c, _ in CUSTOMER_COLUMNS]
    kinds = [k for _, k in CUSTOMER_COLUMNS]
    i_email, i_country = names.index("email"), names.index("country")
    i_first, i_last = names.index("first_name"), names.index("last_name")

    # --- customers ----------------------------------------------------------
    rows = []
    emails = []  # the lowercase (canonical) address, "" where there is none
    phones = []  # positionally aligned with emails; the key Contoso ERP uses
    n_missing_email = 0
    n_mixed_case = 0
    i_phone = names.index("phone")
    for i in range(1, N_CUSTOMERS + 1):
        row = [_value(k, rnd, i) for k in kinds]
        phones.append(row[i_phone])
        canonical_email = f"{row[i_first]}.{row[i_last]}{i}@example.com".lower()
        row[i_email] = canonical_email

        canonical = CANONICAL_COUNTRIES[i % len(CANONICAL_COUNTRIES)]
        variants = COUNTRY_VARIANTS[canonical]
        # variants[0] is the canonical spelling; anything else is the messy case.
        row[i_country] = (rnd.choice(variants[1:])
                          if rnd.random() < COUNTRY_VARIANT_RATIO else variants[0])

        # Some addresses arrive capitalised the way the customer typed them.
        # Case-folding is therefore not cosmetic: without it these are silently
        # two different people once a second system joins on email.
        if rnd.random() < MIXED_CASE_EMAIL_RATIO:
            row[i_email] = f"{row[i_first]}.{row[i_last]}{i}@Example.com"
            n_mixed_case += 1

        if rnd.random() < MISSING_EMAIL_RATIO:
            row[i_email] = ""
            canonical_email = ""
            n_missing_email += 1
        emails.append(canonical_email)
        rows.append(row)

    # Exact-duplicate customer rows, appended so bronze carries both copies.
    n_dup_customers = int(N_CUSTOMERS * DUPLICATE_CUSTOMER_RATIO)
    for idx in rnd.sample(range(len(rows)), n_dup_customers):
        rows.append(list(rows[idx]))

    buf = io.StringIO()
    buf.write(",".join(names) + "\n")
    for row in rows:
        buf.write(",".join(row) + "\n")
    customers_csv = buf.getvalue().encode()

    # --- orders -------------------------------------------------------------
    # The malformed and redelivered sets are disjoint by construction: a row
    # that is both would make "how many should be quarantined" depend on which
    # rule the transform happens to apply first, which is not a lesson but an
    # ambiguity.
    n_malformed = int(N_ORDERS * MALFORMED_RATIO)
    n_redelivered = int(N_ORDERS * REDELIVERY_RATIO)
    picked = rnd.sample(range(N_ORDERS), n_malformed + n_redelivered)
    malformed = set(picked[:n_malformed])
    redelivered = set(picked[n_malformed:])

    customer_ids = [r[0] for r in rows[:N_CUSTOMERS]]
    events = []
    revenue = 0.0
    for i in range(N_ORDERS):
        pid, _name, _cat, price = PRODUCTS[rnd.randrange(len(PRODUCTS))]
        qty = rnd.randint(1, 5)
        base = {
            "order_id": f"O-{i:08d}",
            "customer_id": customer_ids[rnd.randrange(len(customer_ids))],
            "product_id": pid,
            "order_date": f"2026-07-{rnd.randint(1, 28):02d}",
            "channel": rnd.choice(CHANNELS),
            "store_id": f"S-{rnd.randint(1, 400):04d}",
            "currency": "USD",
            "discount_pct": round(rnd.uniform(0, 0.3), 3),
            "tax_rate": 0.07,
            "shipping_fee": round(rnd.uniform(0, 15), 2),
            "payment_method": rnd.choice(["card", "wallet", "invoice", "cash"]),
            "is_gift": rnd.choice([True, False]),
            "promo_code": rnd.choice(["", "SAVE10", "FREESHIP", "VIP5"]),
        }
        if i in malformed:
            # No price at all, and a nonsensical quantity. Silver quarantines
            # rather than dropping, so the row stays auditable.
            events.append({**base, "quantity": -1, "unit_price": None,
                           "status": "error", "event_seq": len(events)})
            continue
        events.append({**base, "quantity": qty, "unit_price": price,
                       "status": "pending", "event_seq": len(events)})
        if i in redelivered:
            # The same order delivered again with a later status. Quantity and
            # price are unchanged, so revenue does not depend on which copy
            # wins — only the dedupe is under test here.
            events.append({**base, "quantity": qty, "unit_price": price,
                           "status": "shipped", "event_seq": len(events)})
        revenue += qty * price

    orders_jsonl = "\n".join(json.dumps(e) for e in events).encode()

    exp = {
        "EXPECTED_BRONZE_CUSTOMERS": len(rows),
        "EXPECTED_BRONZE_ORDERS": len(events),
        "EXPECTED_SILVER_CUSTOMERS": N_CUSTOMERS,
        "EXPECTED_SILVER_ORDERS": N_ORDERS - n_malformed,
        "EXPECTED_QUARANTINED": n_malformed,
        "EXPECTED_COUNTRIES": set(CANONICAL_COUNTRIES),
        "EXPECTED_REVENUE": round(revenue, 2),
        "EXPECTED_CUSTOMER_COLUMNS": len(names),
        "EXPECTED_MISSING_EMAILS": n_missing_email,
        "EXPECTED_MIXED_CASE_EMAILS": n_mixed_case,
        "EXPECTED_DUPLICATE_CUSTOMER_ROWS": n_dup_customers,
        "EXPECTED_REDELIVERED_ORDERS": n_redelivered,
    }
    return customers_csv, orders_jsonl, exp, emails, phones


# Built once per process, on first use. Every step imports this module for the
# EXPECTED_* values, and regenerating tens of thousands of rows on each import
# would be pure waste.
_CACHE = None


def _built():
    global _CACHE
    if _CACHE is None:
        _CACHE = _build()
    return _CACHE


def __getattr__(name):
    """Serve the EXPECTED_* values without generating at import time."""
    if name.startswith("EXPECTED_"):
        exp = _built()[2]
        if name in exp:
            return exp[name]
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


def customer_emails():
    """The customers' canonical (lowercased) addresses, "" where there is none.

    Contoso Web knows nobody by customer_id, so email is the only key the two
    systems share — web_store.py builds its designed overlap out of this list
    rather than hard-coding addresses that would drift the moment the seed or
    the scale changed.
    """
    return _built()[3]


def customer_phones():
    """The customers' phone numbers, positionally aligned with customer_emails().

    Contoso ERP predates email and knows people by phone, so phone is the only
    key IT shares with POS — and it shares none at all with Contoso Web. Any
    ERP-to-Web match therefore has to travel through POS, which is the point of
    25_resolve.py.
    """
    return _built()[4]


def export(api_key):
    """The vendor's export endpoint. Wrong key → refused, as the real one would."""
    if api_key != API_KEY:
        raise PermissionError("Contoso POS: invalid API key")
    customers_csv, orders_jsonl = _built()[0], _built()[1]
    return {"customers.csv": customers_csv, "orders.jsonl": orders_jsonl}
