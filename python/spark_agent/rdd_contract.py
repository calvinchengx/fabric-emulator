"""The `sc` idioms the facade implements, and the answers real Spark gives.

ONE definition, asserted twice: by the facade's unit tests (fast, every case)
and by `e2e/spark-jvm/job.py` against a real Apache Spark 3.5 JVM (slow, CI).
That second assertion is what makes these expectations an ORACLE rather than
my opinion — a test whose expected values came from the same head that wrote
the implementation cannot fail, which is the defect this repo has hit three
times (see docs/50 and the notes on shared oracles).

So: if the facade drifts, the unit tests fail. If these expectations are wrong
ABOUT SPARK, the JVM job fails. Neither alone is sufficient and the pair is.

Each case is `(label, source_snippet, expected)`. The snippet is evaluated with
`sc` bound to whichever SparkContext is under test, so the two suites run
identical text rather than two transcriptions of one idea.
"""

# Every case here is drawn from measured usage (docs/50-rdd-usage-capture.md):
# Microsoft's diagnostic-emitter pages use the parallelize smoke chain, and
# `fabric-samples` uses setLogLevel. Nothing is here because the RDD API has it.
CASES = [
    ("parallelize.sum", "sc.parallelize([1, 2, 3]).sum()", 6),
    ("parallelize.map.sum", "sc.parallelize([1, 2, 3, 4]).map(lambda x: x * 2).sum()", 20),
    ("parallelize.count", "sc.parallelize([1, 2, 3, 4]).count()", 4),
    ("parallelize.collect", "sc.parallelize([3, 1, 2]).collect()", [3, 1, 2]),
    ("parallelize.filter.collect", "sc.parallelize([1, 2, 3, 4]).filter(lambda x: x % 2 == 0).collect()", [2, 4]),
    ("parallelize.map.collect", "sc.parallelize([1, 2]).map(lambda x: x + 10).collect()", [11, 12]),
    # The exact shape of Microsoft's diagnostic-emitter smoke idiom:
    # `sc.parallelize(Seq(1,2,3,4)).toDF().count()`.
    ("parallelize.toDF.count", "sc.parallelize([1, 2, 3, 4]).toDF().count()", 4),
]

# Returns None on both a real SparkContext and the facade; asserted separately
# because `None` as an expected value reads like a missing case.
VOID_CASES = [
    ("setLogLevel", "sc.setLogLevel('WARN')"),
]
