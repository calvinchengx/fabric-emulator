#!/usr/bin/env python3
"""In-container driver: the whole medallion pipeline, landing → bronze → silver
→ gold → semantic model, against the emulator family.

This is the executable form of docs/28-tutorial-end-to-end.md. Every step
asserts its own outcome; a non-zero exit fails the e2e.
"""
import os
import sys

from common import log
from steps import (bronze, extract_to_landing, gold, gold_tests_catch_bad_data, provision,
                   reflect, semantic_model, silver, store_secret)

GOLD_PROJECT = os.environ.get("GOLD_PROJECT", "/pipeline/gold")

STEPS = [
    ("provision workspace, lakehouse, warehouse, identity", provision),
    ("store the source API key in Key Vault + bind an AKV reference", store_secret),
    ("extract from Contoso POS into Files/landing", extract_to_landing),
    ("bronze: append landing verbatim into Delta", bronze),
    ("silver: dedupe, conform, quarantine", silver),
    ("reflect silver into the lakehouse SQL endpoint", reflect),
    ("gold: dbt build in the warehouse (models + DQ tests)", lambda: gold(GOLD_PROJECT)),
    ("verify the DQ gate rejects bad data", lambda: gold_tests_catch_bad_data(GOLD_PROJECT)),
    ("publish + query the semantic model over executeQueries", semantic_model),
]


def main():
    for i, (name, fn) in enumerate(STEPS, 1):
        log(f"[{i}/{len(STEPS)}] {name}")
        fn()
    log(f"medallion pipeline complete: {len(STEPS)}/{len(STEPS)} steps passed")


if __name__ == "__main__":
    try:
        main()
    except AssertionError as e:
        print(f"FAILED: {e}", file=sys.stderr)
        sys.exit(1)
