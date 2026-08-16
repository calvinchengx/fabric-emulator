"""Make a byte-size Connect conf readable by a client that calls int() on it.

`pyspark`'s Connect client reads `spark.sql.session.localRelationSizeLimit` and
calls `int()` on whatever the server returns, so every `createDataFrame` fails
outright if the engine spells that config as a JVM byte string:

    ValueError: invalid literal for int() with base 10: '3GB'

**Which engine you are pointed at decides whether that happens**, and this agent
is pointed at more than one. Sail 0.7.0, which fabric-emulator pins, serves
`'3221225472'` and needs nothing. Older Sail — including the build
databricks-emulator still pins — serves `'3GB'` and breaks every local relation.
The agent image is shared by both repos, so an unconditional answer is wrong in
one of them whichever answer it is:

- Always preset it (what the agent did until #277): pins the limit to whatever
  constant the preset names, capping local relations at 64MiB on an engine
  offering 3GiB — a 48x reduction applied to user code for a bug it doesn't have.
- Never preset it (#277, measured correctly against Sail 0.7.0): every
  `createDataFrame` raises on the engine databricks-emulator pins. That is not
  hypothetical — it is what its delta witness hit, in `delta_ops`' own
  `spark.createDataFrame([(message,)], ["result"])` on the way back from an
  intercepted MERGE.

So ask the engine and act on the answer. Rewrite only when the served value does
not parse, and rewrite it to **the same size in a spelling the client accepts** —
never to a constant, so no engine's real limit is reduced.
"""
import sys

CONF = "spark.sql.session.localRelationSizeLimit"

_UNITS = {"": 1, "b": 1, "k": 1 << 10, "kb": 1 << 10, "m": 1 << 20, "mb": 1 << 20,
          "g": 1 << 30, "gb": 1 << 30, "t": 1 << 40, "tb": 1 << 40,
          "p": 1 << 50, "pb": 1 << 50}


def byte_size(value):
    """`'3GB'` -> 3221225472. None when `value` is not a JVM byte string.

    Spark's own spelling, which is what an engine copying its config vocabulary
    emits: a number, optional whitespace, an optional unit, case-insensitive,
    with or without the trailing B. A bare number is already what the client
    wants and returns None — there is nothing to rewrite.
    """
    if not isinstance(value, str):
        return None
    text = value.strip().lower()
    digits = 0
    while digits < len(text) and (text[digits].isdigit() or text[digits] == "."):
        digits += 1
    number, unit = text[:digits].strip(), text[digits:].strip()
    if not number or unit not in _UNITS or unit in ("", "b"):
        # No unit means it is already a plain byte count (or not a size at all);
        # either way rewriting it would change a value the client can read.
        return None
    try:
        return int(float(number) * _UNITS[unit])
    except ValueError:
        return None


def apply(spark, conf=CONF):
    """Normalise `conf` on this session if the engine spells it unreadably.

    Returns one of "ok" (the engine's value already parses), "rewritten",
    "unreadable" (it does not parse and is not a size we recognise), or
    "unavailable" (the engine does not serve it, or is not reachable yet).

    Per session, not once at import: the conf lives on the SERVER, so if the
    engine restarts while this agent keeps running, the client reconnects to a
    fresh engine and any earlier normalisation is gone — while `spark.range`
    keeps working, which makes the next `createDataFrame` failure look like user
    error rather than a lost setting.
    """
    try:
        served = spark.conf.get(conf)
    except Exception as err:  # noqa: BLE001 — engine not reachable yet; retried next session
        print(f"agent: could not read {conf} ({err}); leaving it alone",
              file=sys.stderr, flush=True)
        return "unavailable"
    if served is None:
        return "unavailable"
    try:
        int(served)
        return "ok"
    except (TypeError, ValueError):
        pass
    size = byte_size(served)
    if size is None:
        print(f"agent: {conf} is {served!r}, which this client cannot parse and "
              f"this agent does not recognise as a byte size; createDataFrame "
              f"may fail", file=sys.stderr, flush=True)
        return "unreadable"
    try:
        spark.conf.set(conf, str(size))
    except Exception as err:  # noqa: BLE001 — same reason as the read above
        print(f"agent: could not rewrite {conf} ({err})", file=sys.stderr, flush=True)
        return "unavailable"
    print(f"agent: {conf} served as {served!r}, which pyspark's int() rejects; "
          f"rewrote it to {size} — the same limit, not a smaller one",
          file=sys.stderr, flush=True)
    return "rewritten"
