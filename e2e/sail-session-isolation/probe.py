#!/usr/bin/env python3
"""Can two Spark Connect sessions against Sail be isolated from each other?

WHY IT MATTERS. The Livy agent holds ONE Spark session for every Livy session it
serves. That is fine until two callers need DIFFERENT answers to the same name —
which is exactly what OneLake security row filtering needs, because a secured
table becomes a per-user view. Measured (docs/54, stage 5): the filter installed
for one user narrowed another user's session.

`catalog.isolate()` already tries `newSession()` and falls back to the shared
session when an engine lacks it. This asks whether ANY route gives real
isolation on Sail, because the answer decides an architecture:

  isolated  -> per-Livy-session engine sessions; RLS is deliverable on Sail
  not       -> engine-side RLS is JVM-overlay only, whatever else we build

THE TEST IS A LEAK TEST. Each strategy creates a temp view in session A and asks
session B for it. Seeing it is a leak; not seeing it is isolation. A strategy
that fails to build a second session at all is reported as such rather than
counted as isolation — "no session" and "isolated session" are different answers
and only one of them is useful.
"""
import os
import sys
import traceback

from pyspark.sql import SparkSession

REMOTE = os.environ.get("SPARK_REMOTE", "sc://sail:50051")
VIEW = "spike_isolation_probe"


def root_session():
    return SparkSession.builder.remote(REMOTE).getOrCreate()


def strategy_new_session(root):
    """What catalog.isolate() uses today."""
    return root.newSession()


def strategy_builder_create(_root):
    """A second client asking for a NEW session rather than the current one.

    `create()` is Spark Connect's "do not reuse" builder call; `getOrCreate()`
    would hand back the same session and prove nothing.
    """
    return SparkSession.builder.remote(REMOTE).create()


def strategy_builder_get_or_create(_root):
    """The naive route, included because it is what a reader would try first.

    Expected to LEAK. It is here so the result table shows the difference rather
    than asserting it.
    """
    return SparkSession.builder.remote(REMOTE).getOrCreate()


def leaks(a, b):
    """True when a temp view made in `a` is visible from `b`."""
    a.sql(f"CREATE OR REPLACE TEMP VIEW {VIEW} AS SELECT 1 AS x")
    try:
        b.sql(f"SELECT * FROM {VIEW}").collect()
        return True
    except Exception:
        return False
    finally:
        for s in (a, b):
            try:
                s.sql(f"DROP VIEW IF EXISTS {VIEW}")
            except Exception:  # noqa: BLE001 - already gone in an isolated session
                pass


def main():
    root = root_session()
    print(f"sail reachable at {REMOTE}", flush=True)

    results = []
    for name, make in (
        ("newSession()", strategy_new_session),
        ("builder.create()", strategy_builder_create),
        ("builder.getOrCreate()", strategy_builder_get_or_create),
    ):
        try:
            other = make(root)
        except Exception as exc:  # noqa: BLE001
            results.append((name, "unsupported", f"{type(exc).__name__}: {exc}"))
            continue
        if other is None:
            results.append((name, "unsupported", "returned None"))
            continue
        try:
            results.append((name, "LEAKS" if leaks(root, other) else "isolated", ""))
        except Exception:  # noqa: BLE001
            results.append((name, "error", traceback.format_exc(limit=1).strip()))

    print("\nstrategy                 result       detail")
    print("-" * 72)
    for name, result, detail in results:
        print(f"{name:24} {result:12} {detail}")

    isolating = [n for n, r, _ in results if r == "isolated"]
    print("\nVERDICT:", flush=True)
    if isolating:
        print(f"  Sail CAN isolate sessions, via: {', '.join(isolating)}")
        print("  => per-Livy-session engine sessions are possible; stage 5 is")
        print("     deliverable on the default engine.")
    else:
        print("  No strategy isolated. Every route shares one Sail session, so a")
        print("  per-user view is a process-wide view.")
        print("  => engine-side RLS cannot be per-user on Sail, whatever else we")
        print("     build; it is JVM-overlay only until Sail grows sessions.")
    # A spike REPORTS. Exiting non-zero on a negative answer would make a real
    # measurement look like a broken test.
    return 0


if __name__ == "__main__":
    sys.exit(main())
