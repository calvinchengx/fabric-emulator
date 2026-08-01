"""Contoso Web — the second source system: the e-commerce channel.

Contoso POS is a flat batch export (CSV + JSON Lines, one row per record, a
`customer_id` on every row). This one is deliberately nothing like it:

  * **Nested JSON.** An order carries its line items inline, so bronze has to
    flatten before anything downstream can read it.
  * **No customer id at all.** The web store knows a customer only by email.
    Two systems that already share a key can be joined; two that don't must be
    RESOLVED, and that is the work silver has never had to do here.
  * **A third spelling convention for country** — "United States", not `US` or
    `USA` or `us`. Conformance is not a two-value mapping.
  * **Timestamps carrying an offset.** `W-5002` is placed at 02:41 on the 30th
    in +08:00, which is 18:41 on the **29th** in UTC. Whichever silver picks,
    it has to pick deliberately: the order lands in a different day's revenue
    either way.

The overlap with POS is designed, not incidental — see EXPECTED_SHARED_EMAILS
below. Four people exist in both systems with conflicting attributes, two exist
only here, and one POS customer (Farid Rahman) can never be matched at all
because POS holds no email for him. A resolution step that reports 100% match
is a resolution step that is lying.
"""
import json

API_KEY = "web-key-4417-dev"

# Country as this system spells it — full names, overlapping with neither of
# POS's conventions.
CUSTOMERS = [
    {"email": "ava.chen@example.com", "full_name": "Ava M. Chen",
     "country": "United States", "signup_ts": "2025-03-04T09:12:00+00:00"},
    {"email": "ben.okafor@example.com", "full_name": "Benjamin Okafor",
     "country": "United States", "signup_ts": "2025-06-19T17:45:00+00:00"},
    {"email": "carla@example.com", "full_name": "Carla Díaz",
     "country": "United States", "signup_ts": "2025-09-02T11:30:00+00:00"},
    {"email": "dev.patel@example.com", "full_name": "Dev Patel",
     "country": "United Kingdom", "signup_ts": "2026-01-15T08:05:00+00:00"},
    {"email": "hana.kim@example.com", "full_name": "Hana Kim",
     "country": "Singapore", "signup_ts": "2026-02-27T03:20:00+00:00"},
    {"email": "ivan.petrov@example.com", "full_name": "Ivan Petrov",
     "country": "United Kingdom", "signup_ts": "2026-05-08T13:55:00+00:00"},
]

# A real catalog, which POS does not have — this is where dim_product comes
# from. List price is the catalogue price; a line carries the price actually
# charged, and the two are allowed to differ.
PRODUCTS = [
    {"product_id": "P-100", "name": "Widget", "category": "Hardware", "list_price": 24.50},
    {"product_id": "P-200", "name": "Gadget Pro", "category": "Hardware", "list_price": 129.00},
    {"product_id": "P-300", "name": "Sticker Pack", "category": "Accessories", "list_price": 9.90},
    {"product_id": "P-400", "name": "Cable", "category": "Accessories", "list_price": 4.20},
    {"product_id": "P-500", "name": "Workstation", "category": "Hardware", "list_price": 349.00},
]

# Nested: lines live inside their order. W-5003 is cancelled (must not reach
# revenue) and W-5004 line 2 references P-999, which is not in the catalog —
# a referential break the pipeline has to catch rather than join away.
ORDERS = [
    {"web_order_id": "W-5001", "email": "ava.chen@example.com",
     "placed_at": "2026-07-29T14:03:11+00:00", "status": "fulfilled", "lines": [
         {"line_no": 1, "product_id": "P-100", "quantity": 1, "unit_price": 24.50},
         {"line_no": 2, "product_id": "P-300", "quantity": 4, "unit_price": 9.90}]},
    {"web_order_id": "W-5002", "email": "hana.kim@example.com",
     "placed_at": "2026-07-30T02:41:57+08:00", "status": "fulfilled", "lines": [
         {"line_no": 1, "product_id": "P-200", "quantity": 1, "unit_price": 129.00}]},
    {"web_order_id": "W-5003", "email": "ivan.petrov@example.com",
     "placed_at": "2026-07-30T18:22:04+01:00", "status": "cancelled", "lines": [
         {"line_no": 1, "product_id": "P-500", "quantity": 1, "unit_price": 349.00}]},
    {"web_order_id": "W-5004", "email": "carla@example.com",
     "placed_at": "2026-07-31T11:09:45+00:00", "status": "fulfilled", "lines": [
         {"line_no": 1, "product_id": "P-400", "quantity": 3, "unit_price": 4.20},
         {"line_no": 2, "product_id": "P-999", "quantity": 1, "unit_price": 12.00}]},
]

# --- the oracle, in the style of source_system.py ----------------------------
EXPECTED_WEB_CUSTOMERS = 6
EXPECTED_WEB_PRODUCTS = 5
EXPECTED_WEB_ORDERS = 4
EXPECTED_WEB_LINES = 6              # flattened out of the four orders
EXPECTED_WEB_CANCELLED_ORDERS = 1   # W-5003
EXPECTED_WEB_UNKNOWN_PRODUCT_LINES = 1  # W-5004 line 2 -> P-999
EXPECTED_WEB_CLEAN_LINES = 4        # 6 lines, less 1 cancelled, less 1 unknown product
EXPECTED_WEB_REVENUE = 205.70       # 24.50 + 39.60 + 129.00 + 12.60

# --- the designed overlap with Contoso POS -----------------------------------
# Asserted at A1 so that A2's resolution has a stated target rather than
# whatever the join happens to produce.
EXPECTED_SHARED_EMAILS = {
    "ava.chen@example.com",    # POS C-001: "Ava Chen" vs "Ava M. Chen"
    "ben.okafor@example.com",  # POS C-002 spells it "Ben.Okafor@Example.com" — case-fold to match
    "carla@example.com",       # POS C-003: "Carla Diaz" vs "Carla Díaz"
    "dev.patel@example.com",   # POS C-004: agrees once country is conformed
}
EXPECTED_WEB_ONLY_EMAILS = {"hana.kim@example.com", "ivan.petrov@example.com"}
# emi.sato and grace.lim are POS-only; farid has NO email in POS, so he is not
# merely unmatched — he is unmatchable on this key.
EXPECTED_POS_ONLY_PEOPLE = 3
EXPECTED_UNMATCHABLE_POS_PEOPLE = 1
EXPECTED_DISTINCT_PEOPLE = 9        # 7 distinct in POS + 2 web-only

# The one line whose calendar day depends on the timezone decision.
EXPECTED_TZ_SENSITIVE_ORDER = "W-5002"  # 2026-07-30 local (+08:00) / 2026-07-29 UTC


def export(api_key):
    """The web store's export endpoint. Wrong key -> refused, as POS's is."""
    if api_key != API_KEY:
        raise PermissionError("Contoso Web: invalid API key")
    return {
        "customers.json": json.dumps(CUSTOMERS).encode(),
        "products.json": json.dumps(PRODUCTS).encode(),
        "orders.json": json.dumps(ORDERS).encode(),
    }
