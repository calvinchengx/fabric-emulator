"""Contoso Web — the second source system: the e-commerce channel, generated.

Contoso POS is a flat batch export (CSV + JSON Lines, one row per record, a
`customer_id` on every row). This one is deliberately nothing like it:

  * **Nested JSON.** An order carries its line items inline, so bronze has to
    flatten before anything downstream can read it.
  * **No customer id at all.** The web store knows a customer only by email.
    Two systems that already share a key can be joined; two that don't must be
    RESOLVED, and that is the work silver has never had to do here.
  * **A third spelling convention for country** — "United States", not `US` or
    `USA` or `us`. Conformance is not a two-value mapping.
  * **Timestamps carrying an offset.** A share of orders are placed in +08:00 or
    +01:00, where the local calendar day and the UTC day differ. Whichever
    silver picks, it has to pick deliberately: the order lands in a different
    day's revenue either way.

The overlap with POS is DESIGNED, and built from POS's own generated addresses
(source_system.customer_emails()) rather than hard-coded — at this scale a
literal list would drift the moment the seed or the row count changed. A share
of web customers also exist in POS with conflicting attributes, the rest exist
only here, and POS's missing-email cohort can never be matched at all. A
resolution step that reports 100% match is a resolution step that is lying.
"""
import json
import random

import source_system as pos

API_KEY = "web-key-4417-dev"

# --- scale -------------------------------------------------------------------
SEED = 20260802  # deliberately not POS's seed
N_WEB_CUSTOMERS = 40_000
N_WEB_ORDERS = 90_000

# --- defect / shape ratios ---------------------------------------------------
OVERLAP_RATIO = 0.55  # web customers who also exist in POS, by email
CANCELLED_RATIO = 0.05  # cancelled orders must not reach revenue
UNKNOWN_PRODUCT_RATIO = 0.02  # a line referencing a product not in the catalog
OFFSET_RATIO = 0.15  # placed_at carries a non-UTC offset
MAX_LINES_PER_ORDER = 4

# Country as this system spells it — full names, overlapping with neither of
# POS's conventions.
WEB_COUNTRIES = ["United States", "United Kingdom", "Singapore"]
OFFSETS = ["+08:00", "+01:00", "-05:00"]

# A real catalog, which POS does not have — this is where dim_product comes
# from. List price is the catalogue price; a line carries the price actually
# charged, and the two are allowed to differ.
PRODUCTS = [
    {"product_id": "P-100", "name": "Widget", "category": "Hardware", "list_price": 24.50},
    {"product_id": "P-200", "name": "Gadget Pro", "category": "Hardware", "list_price": 129.00},
    {"product_id": "P-300", "name": "Sticker Pack", "category": "Accessories", "list_price": 9.90},
    {"product_id": "P-400", "name": "Cable", "category": "Accessories", "list_price": 4.20},
    {"product_id": "P-500", "name": "Workstation", "category": "Hardware", "list_price": 349.00},
    {"product_id": "P-600", "name": "Dock", "category": "Hardware", "list_price": 189.00},
    {"product_id": "P-700", "name": "Sleeve", "category": "Accessories", "list_price": 19.90},
    {"product_id": "P-800", "name": "Keyboard", "category": "Hardware", "list_price": 79.00},
]
# Not in the catalog: a referential break the pipeline has to catch rather than
# join away.
UNKNOWN_PRODUCT_IDS = ["P-999", "P-998", "P-997"]


def _build():
    """Generate the export plus its expectations, in one seeded pass."""
    rnd = random.Random(SEED)

    # --- customers ----------------------------------------------------------
    # The overlap is drawn from POS's real addresses, skipping the cohort POS
    # has no email for — those people are unmatchable on this key by
    # construction, which is the point A2 has to account for.
    pos_emails = [e for e in pos.customer_emails() if e]
    n_shared = int(N_WEB_CUSTOMERS * OVERLAP_RATIO)
    n_shared = min(n_shared, len(pos_emails))
    shared = rnd.sample(pos_emails, n_shared)

    customers = []
    for i, email in enumerate(shared):
        # Same person, deliberately conflicting attributes: the web store holds
        # its own spelling of the name and its own country convention.
        local = email.split("@")[0]
        customers.append({
            "email": email,
            "full_name": local.replace(".", " ").title() + " (web)",
            "country": WEB_COUNTRIES[i % len(WEB_COUNTRIES)],
            "signup_ts": f"202{rnd.randint(3, 6)}-{rnd.randint(1, 12):02d}-"
                         f"{rnd.randint(1, 28):02d}T{rnd.randint(0, 23):02d}:"
                         f"{rnd.randint(0, 59):02d}:00+00:00",
        })

    for i in range(N_WEB_CUSTOMERS - n_shared):
        customers.append({
            "email": f"webonly.{i:06d}@example.net",  # .net: cannot collide with POS
            "full_name": f"Web Only {i:06d}",
            "country": WEB_COUNTRIES[i % len(WEB_COUNTRIES)],
            "signup_ts": f"202{rnd.randint(3, 6)}-{rnd.randint(1, 12):02d}-"
                         f"{rnd.randint(1, 28):02d}T{rnd.randint(0, 23):02d}:"
                         f"{rnd.randint(0, 59):02d}:00+00:00",
        })

    web_emails = [c["email"] for c in customers]
    known_products = [p["product_id"] for p in PRODUCTS]
    price_of = {p["product_id"]: p["list_price"] for p in PRODUCTS}

    # --- orders (nested) ----------------------------------------------------
    picked = rnd.sample(range(N_WEB_ORDERS),
                        int(N_WEB_ORDERS * CANCELLED_RATIO)
                        + int(N_WEB_ORDERS * UNKNOWN_PRODUCT_RATIO))
    n_cancelled = int(N_WEB_ORDERS * CANCELLED_RATIO)
    cancelled = set(picked[:n_cancelled])
    has_unknown = set(picked[n_cancelled:])

    orders = []
    n_lines = n_unknown_lines = 0
    revenue = 0.0
    tz_sensitive = 0
    for i in range(N_WEB_ORDERS):
        offset = rnd.choice(OFFSETS) if rnd.random() < OFFSET_RATIO else "+00:00"
        if offset != "+00:00":
            tz_sensitive += 1
        status = "cancelled" if i in cancelled else "fulfilled"
        lines = []
        for ln in range(1, rnd.randint(1, MAX_LINES_PER_ORDER) + 1):
            pid = known_products[rnd.randrange(len(known_products))]
            qty = rnd.randint(1, 4)
            # The price charged may differ from the catalogue price.
            price = round(price_of[pid] * rnd.uniform(0.9, 1.1), 2)
            lines.append({"line_no": ln, "product_id": pid,
                          "quantity": qty, "unit_price": price})
            n_lines += 1
            if status != "cancelled":
                revenue += qty * price
        if i in has_unknown:
            pid = UNKNOWN_PRODUCT_IDS[i % len(UNKNOWN_PRODUCT_IDS)]
            qty = rnd.randint(1, 4)
            price = round(rnd.uniform(5, 50), 2)
            lines.append({"line_no": len(lines) + 1, "product_id": pid,
                          "quantity": qty, "unit_price": price})
            n_lines += 1
            n_unknown_lines += 1
            # An unknown product contributes no revenue: it cannot be joined to
            # the catalog, so silver quarantines the line rather than pricing it.

        orders.append({
            "web_order_id": f"W-{i:08d}",
            "email": web_emails[rnd.randrange(len(web_emails))],
            "placed_at": f"2026-07-{rnd.randint(1, 28):02d}T"
                         f"{rnd.randint(0, 23):02d}:{rnd.randint(0, 59):02d}:00{offset}",
            "status": status,
            "lines": lines,
        })

    # Lines that survive both rules — the only ones that may reach revenue.
    cancelled_lines = sum(len(o["lines"]) for o in orders if o["status"] == "cancelled")
    clean_lines = n_lines - cancelled_lines - n_unknown_lines
    # revenue accumulated above already excludes cancelled orders and never
    # counted an unknown-product line.

    exp = {
        "EXPECTED_WEB_CUSTOMERS": len(customers),
        "EXPECTED_WEB_PRODUCTS": len(PRODUCTS),
        "EXPECTED_WEB_ORDERS": len(orders),
        "EXPECTED_WEB_LINES": n_lines,
        "EXPECTED_WEB_CANCELLED_ORDERS": n_cancelled,
        "EXPECTED_WEB_UNKNOWN_PRODUCT_LINES": n_unknown_lines,
        "EXPECTED_WEB_CLEAN_LINES": clean_lines,
        "EXPECTED_WEB_REVENUE": round(revenue, 2),
        "EXPECTED_TZ_SENSITIVE_ORDERS": tz_sensitive,
        # --- the designed overlap with Contoso POS --------------------------
        # Counts, not literal address sets: at 40,000 web customers a set
        # would be unreadable, and the property under test is the SIZE of each
        # cohort, not which names happen to be in it.
        "EXPECTED_SHARED_EMAIL_COUNT": n_shared,
        "EXPECTED_WEB_ONLY_EMAIL_COUNT": N_WEB_CUSTOMERS - n_shared,
    }
    return customers, orders, exp


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


def export(api_key):
    """The web store's export endpoint. Wrong key -> refused, as POS's is."""
    if api_key != API_KEY:
        raise PermissionError("Contoso Web: invalid API key")
    customers, orders, _ = _built()
    return {
        "customers.json": json.dumps(customers).encode(),
        "products.json": json.dumps(PRODUCTS).encode(),
        "orders.json": json.dumps(orders).encode(),
    }
