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

THE ALLOWLISTS ARE THE CONTRACT. Every entry below is a difference that is
DELIBERATE, and each says why. Adding an entry is how you declare "these two
examples are now allowed to differ here" — a real decision about what the pair
claims, to be argued for in the commit that makes it rather than slipped in to
make a red build green.

There are TWO lists, because "the content may differ" and "the file may be
absent" are different permissions and one list cannot express both.
"""
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
A = ROOT / "examples" / "medallion-advanced-pyspark"
B = ROOT / "examples" / "medallion-advanced-dbt-fabricspark"

# Two different exemptions, because "these may differ" and "this may be missing"
# are different claims and conflating them hides the second.
#
# A single ALLOWED set did conflate them, and the first real failure showed why:
# a README landed in one example and not the other. README content MUST differ —
# each describes its own silver engine — so the obvious fix was to exempt it,
# and under one combined set that exemption would ALSO have permitted the
# missing file, silently blessing a standalone example with no README. The
# check would have gone green on the exact defect it had just caught.

# Present in BOTH, content may differ.
ALLOWED_CONTENT = {
    # The one real difference, and the reason both examples exist.
    "silver.py": "the step under comparison: PySpark vs dbt-fabricspark",
    # One docstring line and one step LABEL name the engine. The step list, the
    # order, and the runner are identical — the step SEQUENCE is asserted
    # separately below, so this exemption cannot hide a dropped step.
    "pipeline.py": "docstring + the silver step's human-readable label",
    # Different silver engines pull different dependency trees.
    "pyproject.toml": "dbt-fabricspark and its transitive deps",
    "uv.lock": "resolved from the pyproject above",
    # Each example is standalone and a reader may copy out either one, so both
    # need a README — describing their own engine, hence different text.
    "README.md": "each describes its own silver engine",
}

# May exist in ONE example only.
ONLY_IN = {
    # dbt-fabricspark needs a dbt project and a Livy client to drive it; the
    # PySpark example needs neither.
    "livy_query.py": "dbt-fabricspark only: the Livy HC client silver.py drives",
    "silver_dbt": "dbt-fabricspark only: the dbt project that builds silver",
    # The PySpark example carries a warehouse probe the dbt one has no use for.
    "check_wh.py": "PySpark only: warehouse probe",
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


def _in(rel, table):
    """True if `rel` is listed in `table`, by exact name or as a child of a dir."""
    return any(rel == a or rel.startswith(a + "/") for a in table)


def main():
    if not A.is_dir() or not B.is_dir():
        sys.exit(f"missing example directory: {A if not A.is_dir() else B}")

    fa, fb = files(A), files(B)
    problems = []

    # Only ONLY_IN excuses an absence. A file exempted for its CONTENT is still
    # required in both — that is the whole distinction.
    def absence(rel, have, lack):
        why = (" — exempt for content, which does not excuse it being missing"
               if _in(rel, ALLOWED_CONTENT) else "")
        # The file list comes from git, so a file written but never `git add`ed
        # is invisible here. Without this line the author of the fix re-runs the
        # check, sees the identical failure, and goes looking for the wrong bug.
        if (lack / rel).exists():
            why += (f"\n      (it EXISTS at {lack.name}/{rel} but is untracked — "
                    f"this check reads `git ls-files`; `git add` it)")
        return f"{rel}: present in {have.name}, absent from {lack.name}{why}"

    for rel in sorted(fa - fb):
        if not _in(rel, ONLY_IN):
            problems.append(absence(rel, A, B))
    for rel in sorted(fb - fa):
        if not _in(rel, ONLY_IN):
            problems.append(absence(rel, B, A))
    for rel in sorted(fa & fb):
        if _in(rel, ALLOWED_CONTENT) or _in(rel, ONLY_IN):
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
              "copies, or add an entry to ALLOWED_CONTENT (must exist in both) or\n"
              "ONLY_IN (may exist in one) in this script, SAYING WHY the\n"
              "difference is deliberate.", file=sys.stderr)
        return 1

    n = len(fa & fb) - sum(1 for r in fa & fb
                           if _in(r, ALLOWED_CONTENT) or _in(r, ONLY_IN))
    print(f"advanced medallion pair: {n} files byte-identical, "
          f"{len(ALLOWED_CONTENT)} may differ in content, "
          f"{len(ONLY_IN)} may exist in one only, step order matches")
    return 0


if __name__ == "__main__":
    sys.exit(main())
