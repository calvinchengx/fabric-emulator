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
print(f"\n{'rows':22} {str(ps['rows']):>58}")
print("   identical — the tool choice did not change the answer")

# Neither path needs statement rewriting: both speak Spark SQL to the same
# engine. The Warehouse half of BOTH examples does (docs/29-tsql-parity.md).
for name, s in (("PySpark", ps), ("dbt-fabricspark", db)):
    assert s["dialect_adaptations"] == [], (name, s["dialect_adaptations"])
print("   neither path needed a statement rewritten on the wire")
