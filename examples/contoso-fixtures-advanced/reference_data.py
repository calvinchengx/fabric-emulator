"""Reference data — the fourth source, and the only one that is not a system.

POS, Web and ERP are all *operational* systems: they record what happened. This
is master/reference data, published by the finance and merchandising teams, and
it exists for a different reason entirely — it is what makes numbers from the
other three **comparable**.

  * **FX rates**, one row per (date, currency). Contoso Web already charges in
    whatever currency the customer used, and POS is USD-only, so "total revenue"
    across the two is a meaningless number until every amount is converted at
    the rate that applied ON THE ORDER'S DATE. Converting at today's rate, or at
    a single average, is the classic way to produce a revenue figure that
    nobody can reconcile.
  * **A product hierarchy**, one row per product. The operational systems know a
    `product_id` and at best a flat category; roll-ups by department or segment
    are only possible against a hierarchy somebody owns deliberately.

Both land as **Parquet**, like the ERP feed — reference data is published, not
scraped, and a columnar file is what a publisher hands you.

A reference feed has no "defects" to inject, and inventing some would be
dishonest: the lesson here is conformance, not cleansing. What it does carry is
the one property real rate tables always have — **gaps**. Rates are published on
business days only, so a weekend order has no same-day rate and the pipeline
must decide, deliberately, to carry the last published rate forward.
"""
import datetime
import io
import random

import pyarrow as pa
import pyarrow.parquet as pq
import web_store as web

API_KEY = "ref-key-7734-dev"

SEED = 20260804

# The window the order feeds cover, with a margin either side so a lookup near
# the edges still finds a rate to carry forward.
START = datetime.date(2026, 6, 20)
END = datetime.date(2026, 8, 5)

# USD is the reporting currency, so its rate is 1.0 by definition and it is
# published like any other — a consumer should not have to special-case it.
CURRENCIES = {"USD": 1.0, "GBP": 1.27, "SGD": 0.74, "EUR": 1.09}

# Departments above the flat `category` the web catalogue carries.
DEPARTMENTS = {"Hardware": "Devices", "Accessories": "Attach"}
SEGMENTS = {"Devices": "Core", "Attach": "Peripheral"}


def _build():
    rnd = random.Random(SEED)

    # --- FX: business days only ---------------------------------------------
    fx = []
    published_days = 0
    day = START
    while day <= END:
        if day.weekday() < 5:  # Mon-Fri; weekends are simply absent
            published_days += 1
            for ccy, base in CURRENCIES.items():
                rate = 1.0 if ccy == "USD" else round(base * rnd.uniform(0.98, 1.02), 6)
                fx.append({"rate_date": day.isoformat(), "currency": ccy,
                           "rate_to_usd": rate})
        day += datetime.timedelta(days=1)

    # --- product hierarchy ---------------------------------------------------
    # Built from the web catalogue, which is the system that actually owns a
    # product list — the hierarchy adds the levels above it rather than
    # inventing a parallel set of products that would then disagree.
    hierarchy = []
    for p in web.PRODUCTS:
        dept = DEPARTMENTS[p["category"]]
        hierarchy.append({
            "product_id": p["product_id"],
            "product_name": p["name"],
            "category": p["category"],
            "department": dept,
            "segment": SEGMENTS[dept],
            "list_price_usd": p["list_price"],
        })

    total_days = (END - START).days + 1
    exp = {
        "EXPECTED_FX_ROWS": len(fx),
        "EXPECTED_FX_CURRENCIES": len(CURRENCIES),
        "EXPECTED_FX_PUBLISHED_DAYS": published_days,
        # The gap is the point: a consumer that joins orders to rates on an
        # equality of dates loses every weekend order.
        "EXPECTED_FX_MISSING_DAYS": total_days - published_days,
        "EXPECTED_PRODUCTS": len(hierarchy),
        "EXPECTED_DEPARTMENTS": len(set(DEPARTMENTS.values())),
    }
    return fx, hierarchy, exp


_CACHE = None


def _built():
    global _CACHE
    if _CACHE is None:
        _CACHE = _build()
    return _CACHE


def __getattr__(name):
    if name.startswith("EXPECTED_"):
        exp = _built()[2]
        if name in exp:
            return exp[name]
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


def _parquet(rows):
    cols = list(rows[0])
    table = pa.table({c: pa.array([r[c] for r in rows]) for c in cols})
    buf = io.BytesIO()
    pq.write_table(table, buf)
    return buf.getvalue()


def export(api_key):
    """The reference publisher's feed. Same key gate as the operational
    systems — reference data is not automatically public."""
    if api_key != API_KEY:
        raise PermissionError("Contoso Reference: invalid API key")
    fx, hierarchy, _ = _built()
    return {"fx_rates.parquet": _parquet(fx),
            "product_hierarchy.parquet": _parquet(hierarchy)}
