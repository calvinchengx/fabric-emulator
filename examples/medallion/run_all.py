#!/usr/bin/env python3
"""Run every step of the medallion example in order.

Each step is an ordinary script you can also run by hand — that is how the
tutorial walks through them. This runner executes them as separate processes,
exactly as a reader would, so nothing passes here that would fail when typed
one line at a time.

05_wrangle.py is skipped: it is the interactive Data Wrangler checkpoint, meant
for the VS Code Interactive Window rather than a batch run.
"""
import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent

STEPS = [
    ("00_provision.py", "provision workspace, lakehouse, warehouse, identity"),
    ("01_secret.py", "store the source API key in Key Vault + bind an AKV reference"),
    ("02_extract_load.py", "extract from Contoso POS into Files/landing"),
    ("03_bronze.py", "bronze: append landing verbatim into Delta"),
    ("04_silver.py", "silver: dedupe, conform, quarantine"),
    ("06_reflect.py", "reflect silver into the lakehouse SQL endpoint"),
    ("07_gold.py", "gold: dbt build in the warehouse (models + DQ tests)"),
    ("08_dq_gate.py", "verify the DQ gate rejects bad data"),
    ("09_semantic_model.py", "publish + query the semantic model over executeQueries"),
]


def main():
    for i, (script, title) in enumerate(STEPS, 1):
        print(f"==> [{i}/{len(STEPS)}] {title}", flush=True)
        rc = subprocess.run([sys.executable, script], cwd=HERE).returncode
        if rc != 0:
            sys.exit(f"FAILED at {script} (exit {rc})")
    print(f"==> medallion pipeline complete: {len(STEPS)}/{len(STEPS)} steps passed", flush=True)


if __name__ == "__main__":
    main()
