#!/usr/bin/env python3
"""Every e2e suite is described in the matrix that claims to list them.

`docs/12-e2e-matrix.md` is the index a reader uses to answer "what is actually
proven end to end, and by which real client". A suite missing from it is worse
than an undocumented one: the table reads as complete, so its absence says the
proof does not exist.

FOUND BY WRITING A SUITE AND FORGETTING TO REGISTER IT. `e2e/notebook-display`
shipped, ran in CI, and was invisible here — noticed only because a neighbouring
suite happened to be compared against it. Nothing was going to catch that.

THE BACKLOG IS RECORDED, NOT WAIVED AWAY. Fifteen suites were already missing
when this was written; listing them as "not a problem" would make this check a
formality on day one. They are named in UNDOCUMENTED with what that costs, and
the check FAILS on any suite that is not either listed or named there — so the
set can only shrink. Removing a name from that list as it gets documented is
the whole point, and `--strict` fails on a name that no longer needs to be
there, for the reason two of `check_notebookutils_surface`'s waivers had gone
stale before anyone reread them.

Usage:
    check_e2e_matrix.py            report
    check_e2e_matrix.py --strict   exit non-zero on any problem
"""
import argparse
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
E2E = ROOT / "e2e"
MATRIX = ROOT / "docs" / "12-e2e-matrix.md"

def is_described(name: str, text: str) -> bool:
    """Does the matrix mention this suite's directory?

    A MENTION OF THE DIRECTORY, not one exact spelling. The first version of
    this demanded a backticked `e2e/<name>/run.py`, which is only the most
    common form: the matrix also writes `e2e/notebook-run/real_fabric.py`,
    `e2e/spark-jvm`, and `e2e/medallion-dbt-fabricspark/`. It reported six
    correctly-described suites as missing, which would have meant editing good
    rows to satisfy a checker rather than the other way round.

    The negative lookahead is what keeps `e2e/medallion` from claiming the row
    that describes `e2e/medallion-advanced`.
    """
    return re.search(rf"e2e/{re.escape(name)}(?![A-Za-z0-9-])", text) is not None


def reference(name: str) -> str:
    """The form to suggest when a row has to be written. One of several the
    matrix accepts; this is the one most of its rows use."""
    return f"`e2e/{name}/run.py`"


# Suites that predate this check. NOT an exemption — a debt, with the cost
# stated. Each is a real suite whose proof the matrix does not mention, so a
# reader planning work cannot see it exists.
UNDOCUMENTED = {
    "agent-contract": "the consumer contract the published agent image owes",
    "databricks-chain": "fabric submitting into databricks-emulator",
    "engine-activities": "pipeline activities that reach an engine",
    "environment": "an Environment item installing a package the image lacks",
    "environment-notebook": "a notebook's own Environment binding",
    "fab-driven": "Microsoft's fab CLI importing definitions and running them",
    "medallion-governance": "the medallion cataloged into OpenMetadata",
    "purview-datamap": "pyapacheatlas against the Purview Data Map",
    "rest-helix": "the BMC Helix REST connector",
    "rest-servicenow": "the ServiceNow Table API connector",
    "sail-session-isolation": "per-session engine isolation on Sail",
    "salesforce": "the Salesforce Bulk API 2.0 round trip",
    "sempy": "semantic-link against the emulator",
    "two-context": "the secured user-context split",
}


class Unreadable(Exception):
    """The tree or the matrix is not the shape this reads."""


def suites() -> list:
    """Every e2e suite: a directory with its own `run.py` entry point."""
    if not E2E.is_dir():
        raise Unreadable(f"{E2E} is missing; this check has nothing to read.")
    found = sorted(p.parent.name for p in E2E.glob("*/run.py"))
    if not found:
        raise Unreadable(
            f"no suite under {E2E} has a run.py — the layout changed and this "
            "check is now vacuous rather than clean.")
    return found


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--strict", action="store_true")
    args = parser.parse_args(argv)

    if not MATRIX.is_file():
        raise Unreadable(f"{MATRIX} is missing; the index this checks is gone.")
    text = MATRIX.read_text(encoding="utf-8")
    all_suites = suites()
    listed = [n for n in all_suites if is_described(n, text)]
    missing = [n for n in all_suites if n not in listed]

    problems = [f"{n} runs in CI and the matrix does not mention it. "
                f"Add a row ending {reference(n)}."
                for n in missing if n not in UNDOCUMENTED]
    stale = [f"UNDOCUMENTED lists {n}, which is now in the matrix. Drop it."
             for n in UNDOCUMENTED if n in listed]
    stale += [f"UNDOCUMENTED lists {n}, which is not a suite any more. Drop it."
              for n in UNDOCUMENTED if n not in all_suites]

    print("e2e matrix coverage:")
    print(f"  suites with a run.py : {len(all_suites)}")
    print(f"  described in the matrix: {len(listed)}")
    print(f"  recorded as undocumented: {len(UNDOCUMENTED)}")
    print(f"  unaccounted for        : {len(problems)}")
    for line in problems + stale:
        print(f"  {line}")
    if not problems and not stale:
        print("  every suite is either described or recorded as owing a description.")
    if args.strict and (problems or stale):
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
