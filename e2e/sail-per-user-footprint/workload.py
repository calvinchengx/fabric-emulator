#!/usr/bin/env python3
"""Give one Sail a realistic user's work, so its footprint is not an idle one.

A session, a Delta write to OneLake, a read back, and an aggregation -- enough
to allocate the object-store client, the query engine's buffers and the session
state that a real notebook would.
"""
import os
import sys

from pyspark.sql import SparkSession

remote = sys.argv[1]
path = sys.argv[2]
spark = SparkSession.builder.remote(remote).create()
rows = [(i % 7, i, f"row-{i}") for i in range(20000)]
df = spark.createDataFrame(rows, ["region_id", "n", "label"])
df.write.format("delta").mode("overwrite").save(path)
back = spark.read.format("delta").load(path)
total = back.groupBy("region_id").count().collect()
print(f"{remote}: wrote {df.count()} rows, {len(total)} groups", flush=True)
os._exit(0)
