"""Compare the two SILVER builds: imperative PySpark and declarative dbt.

Both examples emit a `silver_summary.json`. This reads them side by side and
asserts the thing that actually matters — **the tool choice does not change the
answer** — then reports the two things that do differ.

The comparison moved here from gold, and the move is the point. Gold is a
Warehouse in both examples, built by dbt-fabric, so there was never a choice to
compare there: dbt-fabricspark materialises into a Lakehouse and cannot write to
a Warehouse. The genuine fork is bronze -> silver, which is Lakehouse-to-
Lakehouse and where a Fabric team really does pick one or the other.

READ THE CAVEAT ON THE TIMINGS. Both run on the emulator's stand-in engine —
Sail, a Rust Spark-Connect server with no JVM — so a ratio measured here says
something about two processes on your laptop and NOTHING about Fabric's managed
Spark pools. Quoting these as engine guidance would misuse them. What they ARE
good for: knowing what each path costs you locally, in a loop you run all day.
"""
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent
PYSPARK_SUMMARY = HERE.parent / "medallion-pyspark" / "silver_summary.json"
DBT_SUMMARY = HERE / "silver_summary.json"


def load(path, how):
    if not path.exists():
        sys.exit(f"missing {path}\n  Run {how} first — the comparison needs both halves.")
    return json.loads(path.read_text())


# The counterpart is produced by a DIFFERENT example, so running this one alone
# cannot produce it. Locally that is a nudge, not a failure — you ran half the
# comparison and the fix is one command. In CI the two examples are separate
# matrix legs on separate runners, so this path is the normal case there and
# failing the leg on it would mean the medallion could never be green.
#
# Skipping is only honest because the assertion still runs SOMEWHERE: the
# `medallion-compare` job in .github/workflows/ci.yml collects both summaries as
# artifacts and runs this file with both present. If that job is ever removed,
# this skip becomes an assertion nobody makes — which is exactly the shape of
# failure this comparison exists to catch, so remove them together or not at all.
if not PYSPARK_SUMMARY.exists():
    print("==> silver comparison SKIPPED — the other half is not here\n")
    print(f"    missing {PYSPARK_SUMMARY}")
    print("    it is written by ../medallion-pyspark, a separate example, so this")
    print("    run could not have produced it. Locally: `uv run python pipeline.py`")
    print("    there, then re-run this step. In CI the medallion-compare job does it.")
    sys.exit(0)

ps = load(PYSPARK_SUMMARY, "`uv run python pipeline.py` in ../medallion-pyspark")
db = load(DBT_SUMMARY, "`uv run python pipeline.py` here")

print("==> silver, two ways\n", flush=True)
print(f"{'':22} {'imperative':>28}  {'declarative':>28}")
print(f"{'tool':22} {ps['engine']:>28}  {db['engine']:>28}")
print(f"{'compute':22} {ps['compute']:>28}  {db['compute']:>28}")
print(f"{'build seconds':22} {ps['build_seconds']:>28}  {db['build_seconds']:>28}")

# The assertion the whole example exists to make. Same source bytes, same rules,
# two tools: if the row counts diverge, one of them is wrong and the comparison
# has found it rather than averaged over it.
assert ps["rows"] == db["rows"], (
    "the two silver builds disagree:\n"
    f"  PySpark:         {ps['rows']}\n"
    f"  dbt-fabricspark: {db['rows']}")
print(f"\n{'rows':22} {ps['rows']!s:>58}")
print("   identical — the tool choice did not change the answer")

# Neither path needs statement rewriting: both speak Spark SQL to the same
# engine. The Warehouse half of BOTH examples does (docs/29-tsql-parity.md).
for name, s in (("PySpark", ps), ("dbt-fabricspark", db)):
    assert s["dialect_adaptations"] == [], (name, s["dialect_adaptations"])
print("   neither path needed a statement rewritten on the wire")
