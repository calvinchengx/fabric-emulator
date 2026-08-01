#!/usr/bin/env python3
"""Run the ADVANCED medallion track.

The advanced track continues where the basic one stops, so this runs the whole
of `run_all.py` first and then the numbered steps from 20 up. It reuses that
runner's step list rather than restating it — one copy, so the basic tutorial
cannot drift out from under this one.

Steps 00-11 are unchanged by the advanced track. Read
docs/28-tutorial-end-to-end.md for those; docs/31 picks up at 20.
"""
import pathlib
import subprocess
import sys

import run_all

HERE = pathlib.Path(__file__).resolve().parent

ADVANCED_STEPS = [
    ("20_web_extract.py", "second source: Contoso Web -> Key Vault + Files/landing"),
    ("21_web_bronze.py", "bronze: flatten nested orders, pin the overlap with POS"),
    ("22_erp_extract.py", "third source: Contoso ERP (CDC) + reference data, as Parquet"),
    ("23_erp_bronze.py", "bronze: three Copy activities reading a columnar source"),
    ("24_erp_scd2.py", "SCD2: the change log becomes a dimension with history"),
    ("25_resolve.py", "resolve three customer sets transitively, and name who cannot be"),
    ("26_contract_gates.py", "run the ODCS contracts as gates at every layer"),
    ("27_tmdl_pbip.py", "serialise the model as TMDL; lay out a .pbip project"),
]


def main():
    steps = run_all.STEPS + ADVANCED_STEPS
    basic = len(run_all.STEPS)
    for i, (script, title) in enumerate(steps, 1):
        track = "basic" if i <= basic else "advanced"
        print(f"==> [{i}/{len(steps)}] ({track}) {title}", flush=True)
        rc = subprocess.run([sys.executable, script], cwd=HERE).returncode
        if rc != 0:
            sys.exit(f"FAILED at {script} (exit {rc})")
    print(f"==> advanced medallion complete: {len(steps)}/{len(steps)} steps passed",
          flush=True)


if __name__ == "__main__":
    main()
