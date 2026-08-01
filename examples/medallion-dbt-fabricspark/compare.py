"""Compare the two gold builds: the Warehouse star and the Lakehouse star.

Both examples emit a `gold_summary.json`. This reads them side by side and
asserts the thing that actually matters — **the adapter choice does not change
the answer** — then reports the two things that do differ.

READ THE CAVEAT ON THE TIMINGS. They are the emulator's stand-in engines:

    dbt-fabric      -> a vanilla SQL Server sidecar
    dbt-fabricspark -> Sail, a Rust Spark-Connect engine with no JVM

Neither is the Fabric engine it stands for. Fabric Warehouse is a distributed
MPP engine and Fabric Spark is a managed pool; a ratio measured here says
something about two containers on your laptop and NOTHING about which is faster
on Fabric. Quoting these numbers as engine guidance would be a misuse of them.

What the timings ARE good for: knowing what each path costs you locally, in a
loop you run all day.
"""
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent
WAREHOUSE_SUMMARY = HERE.parent / "medallion" / "gold_summary.json"
SPARK_SUMMARY = HERE / "gold_summary.json"


def load(path, how):
    if not path.exists():
        sys.exit(f"missing {path}\n  Run {how} first — the comparison needs both halves.")
    return json.loads(path.read_text())


wh = load(WAREHOUSE_SUMMARY, "`uv run python pipeline.py` in ../medallion")
sp = load(SPARK_SUMMARY, "`uv run python pipeline.py` here")

print("==> gold, two ways\n", flush=True)
print(f"{'':22} {'Warehouse':>28}  {'Lakehouse':>28}")
print(f"{'adapter':22} {wh['engine']:>28}  {sp['engine']:>28}")
print(f"{'target':22} {wh['target']:>28}  {sp['target']:>28}")
print(f"{'compute':22} {wh['compute']:>28}  {sp['compute']:>28}")
print()

# --- 1. equivalence: the claim worth making ----------------------------------
mismatch = []
for table in sorted(set(wh["rows"]) | set(sp["rows"])):
    a, b = wh["rows"].get(table), sp["rows"].get(table)
    flag = "" if a == b else "   <-- MISMATCH"
    if a != b:
        mismatch.append(f"{table}: warehouse={a} spark={b}")
    print(f"{table:22} {a if a is not None else '-':>28}  {b if b is not None else '-':>28}{flag}")

rev_delta = abs(wh["revenue"] - sp["revenue"])
print(f"{'revenue':22} {wh['revenue']:>28,.2f}  {sp['revenue']:>28,.2f}")
if rev_delta >= 0.01:
    mismatch.append(f"revenue differs by {rev_delta:.2f}")

if mismatch:
    sys.exit("\nFAIL: the two engines produced different stars:\n  - "
             + "\n  - ".join(mismatch))
print("\n==> equivalent: both adapters built the same star, to the cent")

# --- 2. dialect divergence: the real portability cost -------------------------
print("\n==> dialect adaptations the emulator had to make on the wire")
for label, s in (("Warehouse", wh), ("Lakehouse", sp)):
    if not s["dialect_adaptations"]:
        print(f"  {label}: none — the SQL dbt emitted ran unmodified")
        continue
    for note in s["dialect_adaptations"]:
        print(f"  {label}: {note}")
print("  (this is the portability cost of each path, and it is not symmetric)")

# --- 3. wall clock, with the caveat attached to it ---------------------------
print(f"\n==> dbt build wall-clock — EMULATOR SIDECARS, NOT FABRIC ENGINES")
print(f"  {wh['engine']:16} {wh['build_seconds']:>7.1f}s   ({wh['compute']})")
print(f"  {sp['engine']:16} {sp['build_seconds']:>7.1f}s   ({sp['compute']})")
print("  These compare a SQL Server container against a Sail container on this")
print("  machine. Fabric Warehouse is an MPP engine and Fabric Spark is a managed")
print("  pool; neither is represented here. Do not read this as engine guidance.")
