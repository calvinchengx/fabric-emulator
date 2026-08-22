#!/usr/bin/env python3
"""Two suspected holes in the stage-5 OneLake security enforcement, measured.

`onelake_security.apply()` secures a session by reshaping its CATALOG: a table
the caller may see in part becomes a temp view holding the filter, and a table
they may not see is dropped. Both moves lean on assumptions about catalog
semantics that were never measured, and each has a plausible failure:

  Q1  A temp view shadows an UNQUALIFIED name. If `spark_catalog.default.sales`
      still resolves to the real table, the viewer reads around the filter with
      a longer name, and the SQL path we already call green is not secure.

  Q2  Sessions are isolated (that is what the sail-session-isolation spike
      settled) but the METASTORE behind them is one. If `DROP TABLE` in the
      viewer's session unregisters the table for everyone, then applying a
      viewer's policy DAMAGES the owner — enforcement with a side effect on
      other people's sessions is not enforcement, it is an outage.

Neither question is about OneLake, tokens or the emulator, so none of those are
in the stack: this is Sail and a Delta table on disk. A probe that needed the
whole platform could not tell a catalog answer from a plumbing one.

EXIT CODE IS 0 EVEN WHEN THE ANSWER IS BAD. A spike reports; it does not gate.
A non-zero exit here would read as "the probe broke".
"""
import os
import sys
import traceback

from pyspark.sql import SparkSession

REMOTE = os.environ.get("SPARK_REMOTE", "sc://sail:50051")
DATA = "/tmp/spike-onelake-bypass"


def session():
    """A session of its own — `create()`, per the sail-session-isolation spike.

    `getOrCreate()` would hand back the same session for owner and viewer, and
    every answer below would be about that instead of about the catalog.
    """
    return SparkSession.builder.remote(REMOTE).create()


def scalar(spark, sql):
    return spark.sql(sql).collect()[0][0]


def attempt(label, fn):
    """Run a probe step, reporting an exception as the ANSWER rather than a crash.

    A blocked read raises, and "it raised" is frequently the result we want:
    printing the exception and carrying on keeps one refused query from hiding
    the questions after it.
    """
    try:
        value = fn()
        print(f"    {label}: {value}", flush=True)
        return value, None
    except Exception as exc:  # noqa: BLE001 - the exception IS a result here
        line = str(exc).strip().splitlines()[0][:120]
        print(f"    {label}: RAISED {type(exc).__name__}: {line}", flush=True)
        return None, exc


def main():
    owner = session()
    print("owner session up", flush=True)

    # Two Delta tables: one the viewer sees in part, one they may not see.
    for name, rows in (("sales", [(1, 10), (1, 20), (2, 30)]),
                       ("secret", [(9, 99)])):
        df = owner.createDataFrame(rows, ["region_id", "amount"])
        df.write.format("delta").mode("overwrite").save(f"{DATA}/{name}")
        owner.sql(f"CREATE TABLE IF NOT EXISTS {name} USING delta "
                  f"LOCATION '{DATA}/{name}'")
    print(f"owner: sales={scalar(owner, 'SELECT count(*) FROM sales')} rows, "
          f"secret={scalar(owner, 'SELECT count(*) FROM secret')} rows", flush=True)

    viewer = session()
    print("viewer session up (separate)", flush=True)

    # MEASURED FIRST TIME ROUND, and it changes the setup: Sail gives each
    # session its OWN catalog, so the viewer opened above could not see `sales`
    # at all ("Table not found"). That is not how a Livy session looks, because
    # `catalog.register()` re-registers the lakehouse's tables into whichever
    # session is serving — including into `default`, so unqualified names
    # resolve. So the probe does what the agent does, or it would be measuring
    # an empty catalog rather than the enforcement.
    for name in ("sales", "secret"):
        viewer.sql(f"CREATE TABLE IF NOT EXISTS {name} USING delta "
                   f"LOCATION '{DATA}/{name}'")
    print(f"viewer catalog bootstrapped: "
          f"sales={scalar(viewer, 'SELECT count(*) FROM sales')} rows unfiltered",
          flush=True)

    # Exactly what onelake_security.apply() does for a role with RLS + CLS.
    viewer.sql("CREATE OR REPLACE TEMP VIEW sales AS "
               "SELECT region_id FROM (SELECT * FROM sales WHERE region_id = 1)")

    print("\nQ1: does a fully-qualified name resolve past the temp view?", flush=True)
    unqualified, _ = attempt("SELECT count(*) FROM sales (the view)",
                             lambda: scalar(viewer, "SELECT count(*) FROM sales"))
    qualified, _ = attempt("SELECT count(*) FROM spark_catalog.default.sales",
                           lambda: scalar(viewer, "SELECT count(*) FROM spark_catalog.default.sales"))
    two_part, _ = attempt("SELECT count(*) FROM default.sales",
                          lambda: scalar(viewer, "SELECT count(*) FROM default.sales"))
    cols, _ = attempt("columns of the unqualified read",
                      lambda: ",".join(viewer.sql("SELECT * FROM sales").columns))
    qcols, _ = attempt("columns of the qualified read",
                       lambda: ",".join(viewer.sql("SELECT * FROM spark_catalog.default.sales").columns))
    twocols, _ = attempt("columns of the TWO-PART read",
                         lambda: ",".join(viewer.sql("SELECT * FROM default.sales").columns))
    api, _ = attempt('spark.read.table("sales")',
                     lambda: viewer.read.table("sales").count())
    api2, _ = attempt('spark.read.table("default.sales")',
                      lambda: viewer.read.table("default.sales").count())

    # Can the qualified name be secured the same way? A temp view is unqualified
    # by construction, so the only lever is removing the qualified REGISTRATION.
    # If that works, the stage-5 approach is repairable in place; if it does not,
    # catalog reshaping cannot secure this path at all and the two-context model
    # is the only route.
    print("\nQ1b: can dropping the qualified registration close it?", flush=True)
    attempt("viewer: DROP TABLE IF EXISTS default.sales",
            lambda: (viewer.sql("DROP TABLE IF EXISTS default.sales"), "ok")[1])
    after_q, _ = attempt("viewer: SELECT count(*) FROM default.sales",
                         lambda: scalar(viewer, "SELECT count(*) FROM default.sales"))
    after_u, _ = attempt("viewer: SELECT count(*) FROM sales (the view)",
                         lambda: scalar(viewer, "SELECT count(*) FROM sales"))
    owner_q, _ = attempt("OWNER: SELECT count(*) FROM default.sales",
                         lambda: scalar(owner, "SELECT count(*) FROM default.sales"))
    path, _ = attempt("spark.read.format(delta).load(path)",
                      lambda: viewer.read.format("delta").load(f"{DATA}/sales").count())

    # Exactly what onelake_security._drop() does for a denied table.
    print("\nQ2: does the viewer's DROP hit the shared metastore?", flush=True)
    for stmt in ("DROP VIEW IF EXISTS secret", "DROP TABLE IF EXISTS secret"):
        attempt(f"viewer: {stmt}", lambda s=stmt: (viewer.sql(s), "ok")[1])
    owner_after, _ = attempt("OWNER, after the viewer dropped it: "
                             "SELECT count(*) FROM secret",
                             lambda: scalar(owner, "SELECT count(*) FROM secret"))
    listed, _ = attempt("OWNER: is `secret` still in SHOW TABLES",
                        lambda: any(r[1] == "secret"
                                    for r in owner.sql("SHOW TABLES").collect()))
    files, _ = attempt("do the Delta FILES survive the drop",
                       lambda: owner.read.format("delta").load(f"{DATA}/secret").count())

    print("\n--- findings", flush=True)
    # EVERY qualified spelling counts, not just the three-part one. The first
    # cut of this line asked only about `spark_catalog.default.sales`, which
    # Sail does not support at all — so it printed "no" while `default.sales`
    # was returning the unfiltered table two lines above. A summary that checks
    # one spelling of the thing it is looking for is a false all-clear.
    worse = [f"{name}={got}" for name, got in
             (("spark_catalog.default.sales", qualified), ("default.sales", two_part))
             if got is not None and unqualified is not None and got > unqualified]
    print(f"Q1 qualified-name bypass  : "
          f"{'BYPASS via ' + ', '.join(worse) if worse else 'no'} "
          f"(the view returns {unqualified})", flush=True)
    print(f"Q1 CLS via qualified name : view cols={cols!r} three-part={qcols!r} "
          f"two-part={twocols!r}", flush=True)
    print(f"Q1b drop the registration : viewer qualified after={after_q} "
          f"view still={after_u}, OWNER qualified after={owner_q}", flush=True)
    print(f"Q1 read.table             : unqualified={api}, two-part={api2}", flush=True)
    print(f"Q1 DataFrame reader       : read.table={api}, read.load(path)={path}", flush=True)
    print(f"Q2 owner lost the table   : "
          f"{'YES — the viewer damaged the owner' if listed is False else 'no'} "
          f"(owner count after={owner_after}, still listed={listed}, files={files})", flush=True)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:  # noqa: BLE001 - a spike reports, it does not gate
        traceback.print_exc()
        sys.exit(0)
