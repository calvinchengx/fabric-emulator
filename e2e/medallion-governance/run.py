#!/usr/bin/env python3
"""e2e: the advanced medallion, catalogued.

The same 23 steps as `e2e/medallion-advanced`, with OpenMetadata attached — so
the `govern` step is not skipped and the catalog it builds is asserted rather
than assumed. Everything the assertion needs already lives in the example:
`govern.py` reads back the domain, the glossary, the metrics, the contracted
ODCS rules, and gold's upstream lineage from OM's own graph API, and fails the
run if any of it is absent.

This harness therefore holds no assertions of its own. The example is the test;
this only guarantees the catalog is RUNNING, which is the one thing the example
cannot arrange for itself — without OpenMetadata `govern.py` skips by design,
and a skipped step passing is exactly the shape that hides a broken feature.

  python3 e2e/medallion-governance/run.py

Requirements: the advanced medallion's (Microsoft ODBC Driver 18 on the host),
plus the memory OpenMetadata's Postgres + OpenSearch need — this is the
heaviest suite in the repo.
"""
import os
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))
sys.path.insert(0, os.path.join(REPO, "e2e", "medallion"))

import run as basic  # noqa: E402 — after the path insert above

EXAMPLE = os.path.join(REPO, "examples", "medallion-advanced-pyspark")

if __name__ == "__main__":
    sys.exit(basic.run(EXAMPLE, label="advanced medallion + catalog",
                       profiles=("governance",)))
