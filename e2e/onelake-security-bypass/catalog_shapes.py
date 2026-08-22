#!/usr/bin/env python3
"""What Sail actually reports about schemas, temp views and re-registration.

The livy e2e failed on the SECOND statement against a secured table -- "Table
not found: sales" -- which means the sweep removed something the rebuild needed
and `restore()` did not put it back. Three things could produce that and they
want different fixes, so this asks which:

  * does `SHOW TABLES` mark a temp view as temporary (the sweep skips those --
    if the flag is missing the sweep deletes the enforcement itself)?
  * does a temp view appear under a SCHEMA, or only unqualified?
  * does `SHOW DATABASES` even list the schema a table was registered into?
"""
import os

from pyspark.sql import SparkSession

REMOTE = os.environ.get("SPARK_REMOTE", "sc://sail:50051")
DATA = "/tmp/spike-catalog-shapes"


def show(spark, sql):
    try:
        rows = spark.sql(sql).collect()
        print(f"  {sql}\n      -> {[tuple(r) for r in rows]}", flush=True)
        return rows
    except Exception as exc:  # noqa: BLE001 - the error is the answer
        print(f"  {sql}\n      -> RAISED {type(exc).__name__}: "
              f"{str(exc).strip().splitlines()[0][:100]}", flush=True)
        return []


def main():
    spark = SparkSession.builder.remote(REMOTE).create()
    spark.createDataFrame([(1, 10), (2, 20)], ["region_id", "amount"]) \
         .write.format("delta").mode("overwrite").save(f"{DATA}/sales")

    print("after registering into a SCHEMA, as the agent does:", flush=True)
    show(spark, "CREATE SCHEMA IF NOT EXISTS lake")
    show(spark, f"CREATE TABLE IF NOT EXISTS lake.sales USING delta LOCATION '{DATA}/sales'")
    show(spark, f"CREATE TABLE IF NOT EXISTS default.sales USING delta LOCATION '{DATA}/sales'")
    show(spark, "SHOW DATABASES")
    show(spark, "SHOW TABLES IN `lake`")
    show(spark, "SHOW TABLES IN `default`")

    print("\nafter installing the filter as a temp view:", flush=True)
    show(spark, "CREATE OR REPLACE TEMP VIEW sales AS "
                "SELECT region_id FROM (SELECT * FROM sales WHERE region_id = 1)")
    show(spark, "SHOW TABLES IN `lake`")
    show(spark, "SHOW TABLES IN `default`")
    show(spark, "SHOW TABLES")

    print("\nafter the sweep drops the qualified registrations:", flush=True)
    show(spark, "DROP TABLE IF EXISTS `lake`.`sales`")
    show(spark, "DROP TABLE IF EXISTS `default`.`sales`")
    show(spark, "SHOW TABLES")
    show(spark, "SELECT count(*) FROM sales")

    print("\nthe SECOND application: can the table be restored under the view?",
          flush=True)
    show(spark, "DROP VIEW IF EXISTS sales")
    show(spark, f"CREATE TABLE IF NOT EXISTS `default`.`sales` USING delta "
                f"LOCATION '{DATA}/sales'")
    show(spark, "SELECT count(*) FROM sales")
    show(spark, "CREATE OR REPLACE TEMP VIEW sales AS "
                "SELECT region_id FROM (SELECT * FROM sales WHERE region_id = 1)")
    show(spark, "SELECT count(*) FROM sales")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
