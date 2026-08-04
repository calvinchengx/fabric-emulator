#!/usr/bin/env python3
"""Run every step of the medallion example, in order.

The order lives HERE and nowhere else. Steps are named for what they do rather
than numbered, so inserting one is an edit to this list instead of a rename of
every file after it — and a reader looking for the silver transform opens
`silver.py` rather than counting.

Each step is an ordinary script you can also run by hand, which is how the
tutorial walks through them. This runner executes them as separate processes,
exactly as a reader would, so nothing passes here that would fail when typed one
line at a time.

`wrangle` is deliberately absent: it is the interactive profiling checkpoint,
meant for the VS Code Interactive Window rather than a batch run.
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
    ("engine", "Spark executes the queued notebook run and reports lineage"),
    ("silver", "PySpark: dedupe, conform, quarantine"),
    ("reflect", "reflect silver into the lakehouse SQL endpoint"),
    ("gold", "dbt-fabric builds the star in the WAREHOUSE, with DQ tests"),
    ("dq_gate", "verify the DQ gate rejects bad data"),
    ("semantic_model", "publish + query the semantic model over executeQueries"),
    ("lineage", "assert the lineage graph the emulator recorded"),
]


def run(steps, label="medallion"):
    """Execute steps in order, stopping at the first failure."""
    for i, (step, title) in enumerate(steps, 1):
        print(f"==> [{i}/{len(steps)}] {title}", flush=True)
        rc = subprocess.run([sys.executable, f"{step}.py"], cwd=HERE).returncode
        if rc != 0:
            sys.exit(f"FAILED at {step}.py (exit {rc})")
    print(f"==> {label} complete: {len(steps)}/{len(steps)} steps passed", flush=True)


if __name__ == "__main__":
    run(STEPS)
