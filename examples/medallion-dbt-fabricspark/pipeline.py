#!/usr/bin/env python3
"""Run the medallion with silver built by dbt-fabricspark.

Standalone: this folder carries its own copy of every step. It used to reach
into the sibling example and run its scripts, which made "the two examples agree"
a statement about one codebase rather than two.

Identical to ../medallion-pyspark except for `silver` — that step is the whole
comparison. Gold is a Warehouse in both, built by dbt-fabric, because
dbt-fabricspark materialises into a Lakehouse and cannot write to a Warehouse.

The order lives here and nowhere else. Steps are named, not numbered.
"""
import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent

STEPS = [
    ("provision", "workspace, lakehouse, warehouse, workspace identity"),
    ("secret", "source API key into Key Vault + an AKV-reference connection"),
    ("extract_load", "extract from Contoso POS into Files/landing"),
    ("bronze", "a real DataPipeline: a Copy activity plus a Notebook activity"),
    # The one step that differs from ../medallion-pyspark.
    ("silver", "dbt-fabricspark: bronze -> silver, declaratively, on Sail"),
    ("reflect", "reflect silver into the lakehouse SQL endpoint"),
    ("gold", "dbt-fabric builds the star in the WAREHOUSE, with DQ tests"),
    ("dq_gate", "verify the DQ gate rejects bad data"),
    ("semantic_model", "publish + query the semantic model over executeQueries"),
    ("lineage", "assert the lineage graph the emulator recorded"),
    ("compare", "compare this silver against the PySpark example's"),
]


def run(steps, label="medallion (dbt-fabricspark silver)"):
    for i, (step, title) in enumerate(steps, 1):
        print(f"==> [{i}/{len(steps)}] {title}", flush=True)
        rc = subprocess.run([sys.executable, f"{step}.py"], cwd=HERE).returncode
        if rc != 0:
            sys.exit(f"FAILED at {step}.py (exit {rc})")
    print(f"==> {label} complete: {len(steps)}/{len(steps)} steps passed", flush=True)


if __name__ == "__main__":
    run(STEPS)
