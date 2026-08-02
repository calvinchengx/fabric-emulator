#!/usr/bin/env python3
"""e2e: the advanced medallion, catalogued.

The same 23 steps as `e2e/medallion-advanced`, run against the stack the ROOT
Makefile starts — so the `govern` step finds OpenMetadata and the catalog it
builds is asserted rather than skipped.

  python3 e2e/medallion-governance/run.py

WHY THIS ONE USES `make` AND THE OTHER HARNESSES DO NOT.

The sibling harnesses each carry a compose file describing the stack their
example runs against. That is a deliberate pattern — a suite that owns its
stack cannot be broken by a change to someone else's. It is also duplication,
and duplication has a cost that shows up exactly here: OpenMetadata is four
services plus two volumes, and copying them into a harness file makes a second
place where the catalog's definition can drift from the one a reader starts.

The root Makefile already solves this. `make up` starts the whole family
*including* OpenMetadata — the governance profile is on by default precisely so
that the documented command matches the quickstart (see PROFILE in ../../Makefile).
So this harness starts nothing itself. It asks for the same stack a reader
asks for, by the same words, and then runs the example against it.

That is also the honest test of the example: a reader who types `make up` and
runs `pipeline.py` gets a catalogued medallion, and this proves it — where a
bespoke harness stack could pass while the documented path was broken.

Requirements: the advanced medallion's (Microsoft ODBC Driver 18 on the host),
plus the memory OpenMetadata's Postgres + OpenSearch need.
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))
EXAMPLE = os.path.join(REPO, "examples", "medallion-advanced-pyspark")


def make(target):
    """Run a root Makefile target. The Makefile is the contract a reader uses;
    reaching past it to `docker compose` here would test a path nobody runs."""
    print(f"==> make {target}", flush=True)
    return subprocess.run(["make", target], cwd=REPO).returncode


def main():
    if make("up") != 0:
        sys.exit("make up failed")
    try:
        # `make status` is the real verdict — `up` only means the containers
        # exist. A pipeline started against a half-ready stack fails somewhere
        # far from the cause.
        if make("status") != 0:
            sys.exit("stack came up but is not usable (make status)")
        # `uv run --project`, matching the sibling harness and the example's
        # README: the example owns its locked dependencies, and the system
        # Python has none of them.
        rc = subprocess.run(["uv", "run", "--project", EXAMPLE, "python", "pipeline.py"],
                            cwd=EXAMPLE).returncode
        if rc != 0:
            sys.exit(f"advanced medallion failed (exit {rc})")
        print("MEDALLION + CATALOG E2E: PASS", flush=True)
    finally:
        make("down")


if __name__ == "__main__":
    main()
