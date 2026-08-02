#!/usr/bin/env python3
"""Each medallion PAIR must differ only in how silver is built.

There are two pairs, and both make the same promise:

    medallion-pyspark           vs  medallion-dbt-fabricspark
    medallion-advanced-pyspark  vs  medallion-advanced-dbt-fabricspark

Within a pair, exactly one step changes — bronze to silver, imperative PySpark
against declarative dbt-fabricspark — and that single difference is the entire
point of shipping two examples instead of one. Gold is a Warehouse in all four,
built by dbt-fabric over TDS, because dbt-fabricspark materialises into a
Lakehouse and has no path to a Warehouse.

Everything else being byte-identical is what makes the comparison meaningful.
"Both silver builds agree, row for row" is a statement about two ENGINES only if
every other step is the same code; the moment a second file diverges, the
comparison is measuring that too and cannot say which caused what.

WHY THIS EXISTS. The advanced pair silently diverged: a lineage change landed in
the pyspark copies of star_silver.py and semantic_model.py and not in the
dbt-fabricspark ones. Both CI legs stayed green — the dbt leg simply reported
eleven fewer lineage edges, and nothing asserted it should report any. Then a
README landed in one advanced example and not the other, and that was green too.
Neither is visible to any test that runs ONE example: two files drifting apart
cannot be seen from inside either one.

The simple pair was unguarded for the same reasons and by the same accident —
the advanced pair got a check first only because that is where the drift was
noticed.

THE ALLOWLISTS ARE THE CONTRACT. Every entry below is a difference that is
DELIBERATE, and each says why. Adding an entry is how you declare "these two
examples are now allowed to differ here" — a real decision about what the pair
claims, to be argued for in the commit that makes it rather than slipped in to
make a red build green.

There are TWO lists per pair, because "the content may differ" and "the file may
be absent" are different permissions and one list cannot express both. A single
list did conflate them, and the README failure showed why: exempting README.md
for its content would ALSO have permitted it being missing, greening the check
on the exact defect it had just caught.
"""
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
EXAMPLES = ROOT / "examples"

# Present in BOTH halves of a pair; content may differ. Shared by both pairs
# because both make the same promise for the same reasons.
CONTENT = {
    "silver.py": "the step under comparison: PySpark vs dbt-fabricspark",
    # One docstring line and one step LABEL name the engine. The step SEQUENCE
    # is asserted separately below, so this cannot hide a dropped step.
    "pipeline.py": "docstring + the silver step's human-readable label",
    "pyproject.toml": "dbt-fabricspark and its transitive deps",
    "uv.lock": "resolved from the pyproject above",
    # Each example is standalone and a reader may copy out either one, so both
    # need a README — describing their own engine, hence different text.
    "README.md": "each describes its own silver engine",
}

# May exist in ONE half only. Shared entries here, per-pair extras below.
ONLY_IN = {
    "livy_query.py": "dbt-fabricspark only: the Livy HC client silver.py drives",
    "silver_dbt": "dbt-fabricspark only: the dbt project that builds silver",
}

PAIRS = [
    {
        "label": "simple medallion",
        "a": "medallion-pyspark",
        "b": "medallion-dbt-fabricspark",
        "only_in": {
            # The comparison lives with the dbt half and reads the pyspark
            # half's summary; only one copy can own it.
            "compare.py": "dbt-fabricspark only: reads both silver summaries",
        },
        # `compare` is a real extra step, not a drifted one. Named here so the
        # sequence check still compares everything else in order.
        "extra_steps_in_b": ["compare"],
    },
    {
        "label": "advanced medallion",
        "a": "medallion-advanced-pyspark",
        "b": "medallion-advanced-dbt-fabricspark",
        "only_in": {
            "check_wh.py": "PySpark only: warehouse probe",
            "compare.py": "dbt-fabricspark only: reads both halves' summaries",
        },
        "extra_steps_in_b": ["compare"],
    },
]


def tracked(root):
    """Every TRACKED file under `root`, relative to it.

    Asked of git rather than the filesystem, deliberately. Running an example
    leaves artifacts behind — state.json, gold_summary.json, a whole pbip/
    project, dbt's target/ and logs/ — and only one half of a pair is ever run
    at a time, so a filesystem walk reports a dozen phantom divergences that say
    nothing about the source. Tracked files are also exactly what this check is
    about: examples that must be the same IN THE REPOSITORY.
    """
    out = subprocess.run(["git", "ls-files", "-z", "--", str(root)],
                         cwd=ROOT, capture_output=True, text=True, check=True)
    rel = root.relative_to(ROOT).as_posix() + "/"
    return {p[len(rel):] for p in out.stdout.split("\0") if p.startswith(rel)}


def listed(rel, table):
    """True if `rel` is in `table`, by exact name or as a child of a dir."""
    return any(rel == a or rel.startswith(a + "/") for a in table)


def steps(pipeline):
    """The step names, in order, as pipeline.py declares them."""
    return [ln.split('("', 1)[1].split('"', 1)[0]
            for ln in pipeline.read_text().splitlines()
            if ln.strip().startswith('("')]


def check(pair):
    """Return a list of divergences for one pair."""
    a, b = EXAMPLES / pair["a"], EXAMPLES / pair["b"]
    for d in (a, b):
        if not d.is_dir():
            return [f"missing example directory: {d}"]

    only_in = {**ONLY_IN, **pair["only_in"]}
    fa, fb = tracked(a), tracked(b)
    problems = []

    def absence(rel, have, lack):
        why = (" — exempt for content, which does not excuse it being missing"
               if listed(rel, CONTENT) else "")
        # The file list comes from git, so a file written but never `git add`ed
        # is invisible here. Without this line the author of the fix re-runs the
        # check, sees the identical failure, and looks for the wrong bug.
        if (lack / rel).exists():
            why += (f"\n      (it EXISTS at {lack.name}/{rel} but is untracked — "
                    f"this check reads `git ls-files`; `git add` it)")
        return f"{rel}: present in {have.name}, absent from {lack.name}{why}"

    # Only ONLY_IN excuses an absence. A file exempted for its CONTENT is still
    # required in both — that is the whole distinction.
    for rel in sorted(fa - fb):
        if not listed(rel, only_in):
            problems.append(absence(rel, a, b))
    for rel in sorted(fb - fa):
        if not listed(rel, only_in):
            problems.append(absence(rel, b, a))
    for rel in sorted(fa & fb):
        if listed(rel, CONTENT) or listed(rel, only_in):
            continue
        if (a / rel).read_bytes() != (b / rel).read_bytes():
            problems.append(f"{rel}: differs between the two examples")

    # The step SEQUENCE must match even though pipeline.py is exempt, or the
    # exemption granted for a docstring would also cover a dropped step.
    sa, sb = steps(a / "pipeline.py"), steps(b / "pipeline.py")
    extra = pair["extra_steps_in_b"]
    # A stale exemption is its own bug: if the extra step is gone, the allowance
    # silently starts covering a step that drifted out of the OTHER pipeline.
    for name in extra:
        if name not in sb:
            problems.append(
                f"pipeline.py: '{name}' is exempted as an extra step in "
                f"{b.name} but is not in its pipeline — remove the exemption")
    trimmed = [s for s in sb if s not in extra]
    if trimmed != sa:
        problems.append(
            f"pipeline.py step ORDER differs, which the exemption does not "
            f"cover:\n      {a.name}: {sa}\n      {b.name} (less {extra}): {trimmed}")

    return problems


def main():
    failed = False
    for pair in PAIRS:
        problems = check(pair)
        if problems:
            failed = True
            print(f"\n{pair['label']}: diverged in {len(problems)} place(s):",
                  file=sys.stderr)
            for p in problems:
                print(f"  - {p}", file=sys.stderr)
        else:
            a, b = EXAMPLES / pair["a"], EXAMPLES / pair["b"]
            shared = tracked(a) & tracked(b)
            only_in = {**ONLY_IN, **pair["only_in"]}
            n = sum(1 for r in shared
                    if not (listed(r, CONTENT) or listed(r, only_in)))
            print(f"{pair['label']}: {n} files byte-identical, "
                  f"{len(CONTENT)} may differ in content, "
                  f"{len(only_in)} may exist in one half, step order matches")

    if failed:
        print("\nEach pair must differ ONLY in how silver is built — that is what lets\n"
              "the comparison attribute a difference to the ENGINE rather than to the\n"
              "code around it. Mirror the change into both halves, or add an entry to\n"
              "CONTENT (must exist in both) or the pair's only_in (may exist in one),\n"
              "SAYING WHY the difference is deliberate.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
