"""Compare the two ADVANCED builds: imperative PySpark and declarative dbt.

`../medallion-dbt-fabricspark/compare.py` makes this claim for the simple pair —
two silver engines, one answer. This one makes the harder version of it.

The advanced pair does not stop at silver. On top of it sits an identity
resolution across three source systems that share no common key, and a star
joined from two of them. So the question here is not "do the two silver builds
agree" but **"does the silver engine choice perturb anything downstream of
it"** — and that is quieter to get wrong. The cohorts can shift between each
other while every total holds: move a hundred people from `erp_bridged` to
`erp_only` and the row counts do not move at all.

Everything between silver and the star is byte-identical in the two examples;
`scripts/check_example_parity.py` fails CI if that stops being true. That is what
makes a difference here ATTRIBUTABLE — silver is the only thing that differs, so
silver is what caused it.

WHAT THIS DOES NOT CLAIM. Both halves run against the same Sail container, so
the build seconds compare two client libraries driving one engine on one laptop.
They say nothing about Fabric's managed Spark pools, and quoting them as engine
guidance would misuse them.
"""
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent
OTHER = HERE.parent / "medallion-advanced-pyspark"

# (filename, what it proves) — read in pipeline order, so the first disagreement
# reported is the earliest one, which is the one that caused the rest.
STAGES = [
    ("silver_summary.json", "silver"),
    ("star_silver_summary.json", "the identity resolution"),
    ("gold_star_summary.json", "the multi-source star"),
]


def load(path, how):
    if not path.exists():
        sys.exit(f"missing {path}\n  Run {how} first — the comparison needs both halves.")
    return json.loads(path.read_text())


# The counterpart is produced by a DIFFERENT example, so running this one alone
# cannot produce it. Locally that is a nudge, not a failure — you ran half the
# comparison and the fix is one command. In CI the two examples are separate
# matrix legs on separate runners, so this path is the normal case there and
# failing the leg on it would mean the advanced medallion could never be green.
#
# Skipping is only honest because the assertion still runs SOMEWHERE: the
# `medallion-advanced-compare` job in .github/workflows/ci.yml collects both
# examples' summaries as artifacts and runs this file with all of them present.
# If that job is ever removed, this skip becomes an assertion nobody makes —
# which is the shape of failure this comparison exists to catch, so remove them
# together or not at all.
missing = [f for f, _ in STAGES if not (OTHER / f).exists()]
if missing:
    print("==> advanced comparison SKIPPED — the other half is not here\n")
    print(f"    missing from {OTHER.name}: {', '.join(missing)}")
    print("    they are written by ../medallion-advanced-pyspark, a separate")
    print("    example, so this run could not have produced them. Locally:")
    print("    `uv run python pipeline.py` there, then re-run this step.")
    print("    In CI the medallion-advanced-compare job does it.")
    sys.exit(0)

print("==> the advanced medallion, two ways\n", flush=True)

failures = []
for filename, what in STAGES:
    ps = load(OTHER / filename, f"`uv run python pipeline.py` in {OTHER.name}")
    db = load(HERE / filename, "`uv run python pipeline.py` here")

    # Compare every key EXCEPT the ones that are allowed to differ, rather than
    # naming the keys to check. A comparison that lists what to look at goes
    # quietly blind the day a summary gains a field — and the new field is
    # exactly where a new bug would be.
    skip = {"example", "engine", "compute", "target", "build_seconds",
            "dialect_adaptations"}
    keys = (set(ps) | set(db)) - skip
    for k in sorted(keys):
        a, b = ps.get(k, "<absent>"), db.get(k, "<absent>")
        if a != b:
            failures.append(f"{what} ({filename}): '{k}' differs\n"
                            f"      PySpark:         {a}\n"
                            f"      dbt-fabricspark: {b}")
    if not any(f.startswith(what) for f in failures):
        print(f"    {what:26} agrees on {len(keys)} measure(s)")

if failures:
    print()
    sys.exit("the two advanced builds disagree:\n  - " + "\n  - ".join(failures))

# Reported, not asserted — see the caveat in the module docstring.
sil_ps = load(OTHER / "silver_summary.json", "")
sil_db = load(HERE / "silver_summary.json", "")
print(f"\n    {'silver build seconds':26} "
      f"PySpark {sil_ps['build_seconds']:>7}   dbt {sil_db['build_seconds']:>7}")
print("\n    identical through silver, the resolution and the star —")
print("    the silver engine choice did not change the answer anywhere downstream")
