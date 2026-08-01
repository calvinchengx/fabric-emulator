#!/usr/bin/env python3
"""Run the LAKEHOUSE medallion: same data, gold built by Spark instead of T-SQL.

Steps 00-05 are the Warehouse example's, run unmodified from
`../medallion` — provisioning, the Key Vault secret, landing, bronze, the Spark
notebook run, and silver are identical for both paths, and copying them here
would mean two versions of the same eight files drifting apart. What this
example OWNS is where the two paths diverge, which is everything from gold on:

    ../medallion            here
    reflect.py           (nothing — Spark reads the Delta directly)
    gold.py    dbt-fabric      gold_spark.py   dbt-fabricspark
    …                             compare.py

State is separate (`PIPELINE_STATE` points here), so this example can be run
against its own workspace without disturbing the Warehouse one — and both must
have run before `compare.py` has two halves to compare.

Runs on the standard stack (`docker compose up -d`) with no overlay. That is
only true since the Livy surface started forwarding a statement's `kind`: the
shipped agent was Python-only, so a SQL client had nothing to talk to and the
dbt e2e had to carry its own SQL-only agent. The agent now dispatches — `sql`
statements to Spark SQL with a structured result set, everything else to the
Python REPL — so one agent serves both this example and engine.py's notebook.
"""
import os
import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
WAREHOUSE = HERE.parent / "medallion-pyspark"
STATE = HERE / "state.json"

# Shared with the Warehouse example: identical steps, run in its directory so
# its imports resolve, but writing OUR state file.
SHARED = [
    ("provision.py", "provision workspace + lakehouse"),
    ("secret.py", "store the source API key in Key Vault + AKV reference"),
    ("extract_load.py", "extract from Contoso POS into Files/landing"),
    ("bronze.py", "bronze: a pipeline — Copy activity + Notebook activity"),
    ("engine.py", "Spark executes the queued notebook run"),
    ("silver.py", "silver: dedupe, conform, quarantine"),
]

OWN = [
    ("gold_spark.py", "gold: dbt-fabricspark builds the star with Spark SQL"),
    ("compare.py", "compare both gold builds — equivalence, dialect, wall-clock"),
]


def main():
    # Its own workspace: display names are unique per emulator, so the two
    # examples must not both ask for "contoso-analytics" on one stack.
    env = {**os.environ,
           "PIPELINE_STATE": str(STATE),
           "WORKSPACE_NAME": os.environ.get("WORKSPACE_NAME", "contoso-analytics-spark")}
    steps = [(WAREHOUSE, s, t) for s, t in SHARED] + [(HERE, s, t) for s, t in OWN]
    for i, (cwd, script, title) in enumerate(steps, 1):
        where = "shared" if cwd == WAREHOUSE else "lakehouse"
        print(f"==> [{i}/{len(steps)}] ({where}) {title}", flush=True)
        rc = subprocess.run([sys.executable, script], cwd=cwd, env=env).returncode
        if rc != 0:
            sys.exit(f"FAILED at {script} (exit {rc})")
    print(f"==> lakehouse medallion complete: {len(steps)}/{len(steps)} steps passed",
          flush=True)


if __name__ == "__main__":
    main()
