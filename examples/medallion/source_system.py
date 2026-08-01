"""Contoso POS — the fictitious SaaS source system.

Deliberately messy, so silver has real work: an at-least-once duplicate order
event, five spellings of three countries, a duplicated customer row, a missing
email, and one malformed order (negative quantity, no price).

Nothing here is emulator-specific — it is the "vendor API" the pipeline pulls
from, and it refuses to export without the API key held in Key Vault.
"""
import json

API_KEY = "pos-key-8843-dev"

CUSTOMERS_CSV = """customer_id,name,email,country
C-001,Ava Chen,ava.chen@example.com,US
C-002,Ben Okafor,Ben.Okafor@Example.com,USA
C-003,Carla Diaz,carla@example.com,us
C-004,Dev Patel,dev.patel@example.com,GB
C-005,Emi Sato,emi.sato@example.com,U.K.
C-006,Farid Rahman,,SG
C-007,Grace Lim,grace.lim@example.com,SG
C-007,Grace Lim,grace.lim@example.com,SG
"""

# (order_id, customer_id, order_date, quantity, unit_price, status)
ORDER_EVENTS = [
    ("O-1001", "C-001", "2026-07-28", 2, 24.50, "shipped"),
    ("O-1002", "C-002", "2026-07-28", 1, 129.00, "shipped"),
    ("O-1003", "C-003", "2026-07-29", 3, 9.90, "pending"),
    ("O-1003", "C-003", "2026-07-29", 3, 9.90, "shipped"),  # at-least-once redelivery
    ("O-1004", "C-004", "2026-07-30", 5, 4.20, "shipped"),
    ("O-1005", "C-005", "2026-07-30", 1, 349.00, "shipped"),
    ("O-1006", "C-007", "2026-07-31", 2, 62.00, "shipped"),
    ("O-1007", "C-006", "2026-07-31", -1, None, "error"),  # malformed
]

# What silver must produce from the above — the pipeline asserts against these
# so a regression in the transforms fails the e2e rather than sliding through.
EXPECTED_BRONZE_CUSTOMERS = 8
EXPECTED_BRONZE_ORDERS = 8
EXPECTED_SILVER_CUSTOMERS = 7  # the duplicated C-007 row collapses
EXPECTED_SILVER_ORDERS = 6  # 8 events - 1 duplicate - 1 quarantined
EXPECTED_QUARANTINED = 1
EXPECTED_COUNTRIES = {"US", "GB", "SG"}
EXPECTED_REVENUE = 701.70  # sum of the 6 clean orders


def export(api_key):
    """The vendor's export endpoint. Wrong key → refused, as the real one would."""
    if api_key != API_KEY:
        raise PermissionError("Contoso POS: invalid API key")
    orders = "\n".join(
        json.dumps({"order_id": o, "customer_id": c, "order_date": d,
                    "quantity": q, "unit_price": p, "status": s, "event_seq": i})
        for i, (o, c, d, q, p, s) in enumerate(ORDER_EVENTS))
    return {"customers.csv": CUSTOMERS_CSV.encode(), "orders.jsonl": orders.encode()}
