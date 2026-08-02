#!/usr/bin/env python3
"""The two advanced medallion examples must differ ONLY in how silver is built.

`examples/medallion-advanced-pyspark` and `examples/medallion-advanced-dbt-fabricspark`
run the same twenty-two steps over the same three source systems. Exactly one of
those steps differs — bronze to silver, imperative PySpark against declarative
dbt-fabricspark — and that single difference is the entire point of shipping two
examples instead of one.

Everything else being byte-identical is what makes the `medallion-compare` job's
claim meaningful. "Both silver builds agree, row for row" is only a statement
about the two ENGINES if every other step is the same code; the moment a second
file diverges, the comparison is measuring that too and cannot say which caused
what.

WHY THIS EXISTS. The pair silently diverged: a lineage change landed in the
pyspark copies of star_silver.py and semantic_model.py and not in the
dbt-fabricspark ones. Both legs stayed green — the dbt leg simply reported less
lineage, and nothing asserted that it should report any. That is the failure
mode this repo keeps meeting: an absence that every available check reports as a
presence. Two files drifting apart cannot be seen by any test that runs one of
them.

THE ALLOWLIST IS THE CONTRACT. Every entry below is a file whose difference is
DELIBERATE, and each says why. Adding an entry is how you declare "these two
examples are now allowed to differ here" — which is a real decision about what
the pair claims, and should be argued for in the commit that makes it, not
slipped in to make a red build green.
"""
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
A = ROOT / "examples" / "medallion-advanced-pyspark"
B = ROOT / "examples" / "medallion-advanced-dbt-fabricspark"

# Paths relative to each example root that are ALLOWED to differ, and why.
ALLOWED = {
    # The one real difference, and the reason both examples exist.
    "silver.py": "the step under comparison: PySpark vs dbt-fabricspark",
    # dbt-fabricspark needs a dbt project and a Livy client to drive it; the
    # PySpark example needs neither.
    "livy_query.py": "dbt-fabricspark only: the Livy HC client silver.py drives",
    "silver_dbt": "dbt-fabricspark only: the dbt project that builds silver",
    # The PySpark example carries a warehouse probe the dbt one has no use for.
    "check_wh.py": "PySpark only: warehouse probe",
    # One docstring line and one step LABEL name the engine. The step list, the
    # order, and the runner are identical — parity of the pipeline is asserted
    # separately below, so this exemption cannot hide a missing step.
    "pipeline.py": "docstring + the silver step's human-readable label",
    # Different silver engines pull different dependency trees.
    "pyproject.toml": "dbt-fabricspark and its transitive deps",
    "uv.lock": "resolved from the pyproject above",
}

def files(root):
    """Every TRACKED file under `root`, relative to it.

    Asked of git rather than the filesystem, deliberately. Running either
    example leaves artifacts behind — state.json, gold_summary.json, a whole
    pbip/ project, dbt's target/ and logs/ — and only one of the pair gets run
    at a time, so a filesystem walk reports a dozen phantom divergences that say
    nothing about the source. Tracked files are also exactly what this check is
    about: two examples that must be the same IN THE REPOSITORY.
    """
    out = subprocess.run(["git", "ls-files", "-z", "--", str(root)],
                         cwd=ROOT, capture_output=True, text=True, check=True)
    rel = pathlib.Path(root).relative_to(ROOT).as_posix() + "/"
    return {p[len(rel):] for p in out.stdout.split("\0") if p.startswith(rel)}


def allowed(rel):
    """True if `rel` is exempt — by exact name or as a child of an exempt dir."""
    return any(rel == a or rel.startswith(a + "/") for a in ALLOWED)


def main():
    if not A.is_dir() or not B.is_dir():
        sys.exit(f"missing example directory: {A if not A.is_dir() else B}")

    fa, fb = files(A), files(B)
    problems = []

    for rel in sorted(fa - fb):
        if not allowed(rel):
            problems.append(f"{rel}: present in {A.name}, absent from {B.name}")
    for rel in sorted(fb - fa):
        if not allowed(rel):
            problems.append(f"{rel}: present in {B.name}, absent from {A.name}")
    for rel in sorted(fa & fb):
        if allowed(rel):
            continue
        if (A / rel).read_bytes() != (B / rel).read_bytes():
            problems.append(f"{rel}: differs between the two examples")

    # The step SEQUENCE must match even though pipeline.py is exempt, or the
    # exemption granted for a docstring would also cover a dropped step.
    steps = {}
    for root in (A, B):
        src = (root / "pipeline.py").read_text()
        steps[root.name] = [ln.split('("', 1)[1].split('"', 1)[0]
                            for ln in src.splitlines()
                            if ln.strip().startswith('("')]
    if steps[A.name] != steps[B.name]:
        problems.append(
            f"pipeline.py step ORDER differs, which the exemption does not "
            f"cover:\n      {A.name}: {steps[A.name]}\n      {B.name}: {steps[B.name]}")

    if problems:
        print(f"The advanced medallion pair has diverged in "
              f"{len(problems)} place(s):\n", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        print("\nThese two examples must differ ONLY in how silver is built — that is\n"
              "what lets the medallion-compare job attribute a difference to the\n"
              "ENGINE rather than to the code around it. Mirror the change into both\n"
              "copies, or add an entry to ALLOWED in this script SAYING WHY the\n"
              "difference is deliberate.", file=sys.stderr)
        return 1

    n = len(fa & fb) - sum(1 for r in fa & fb if allowed(r))
    print(f"advanced medallion pair: {n} files byte-identical, "
          f"{len(ALLOWED)} deliberate exemptions, step order matches")
    return 0


if __name__ == "__main__":
    sys.exit(main())
