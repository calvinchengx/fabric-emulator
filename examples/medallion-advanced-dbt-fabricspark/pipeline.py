#!/usr/bin/env python3
"""Run the ADVANCED medallion with silver built by dbt-fabricspark.

Standalone: this folder carries its own copy of the basic steps rather than
importing them from `../medallion-pyspark`. The duplication is deliberate — an
example a reader can copy out whole and run is worth more than one that reaches
across directories — and it is why the *fixtures* are a shared package: the
seeded generators must be one copy, or two examples could pass their own
assertions against different data.

The order lives here and nowhere else. Steps are named, not numbered.

`wrangle` is absent for the same reason as in the simple example: it is the
interactive profiling checkpoint, not a batch step.
"""
import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent

# The single-source pipeline, unchanged from the simple example.
BASIC = [
    ("provision", "workspace, lakehouse, warehouse, workspace identity"),
    ("secret", "source API key into Key Vault + an AKV-reference connection"),
    ("extract_load", "extract from Contoso POS into Files/landing"),
    ("bronze", "a real DataPipeline: a Copy activity plus a Notebook activity"),
    ("engine", "Spark executes the queued notebook run and reports lineage"),
    ("silver", "dbt-fabricspark: bronze -> silver, declaratively, on Sail"),
    ("reflect", "reflect silver into the lakehouse SQL endpoint"),
    ("gold", "dbt-fabric builds the single-source star in the WAREHOUSE"),
    ("dq_gate", "verify the DQ gate rejects bad data"),
    ("semantic_model", "publish + query the semantic model over executeQueries"),
    ("lineage", "assert the lineage graph the emulator recorded"),
]

# What a second and third source system force: conformance that is real,
# identity that is a graph, and a star built by joining across sources.
ADVANCED = [
    ("web_extract", "second source: Contoso Web -> Key Vault + Files/landing"),
    ("web_bronze", "bronze: flatten the nested web export"),
    ("erp_extract", "third source: Contoso ERP (CDC) + reference data, as Parquet"),
    ("erp_bronze", "bronze: three Copy activities reading a columnar source"),
    ("erp_scd2", "SCD2: the change log becomes a dimension with history"),
    ("resolve", "resolve three customer sets transitively; name who cannot be"),
    ("star_silver", "materialise the resolution + the web order-line grain"),
    # A SECOND reflect, and it is not redundant. Reflection happens on a
    # lakehouse LOGIN, so the one in BASIC ran before star_silver existed and
    # could only carry the tables silver.py wrote. gold_star reads
    # silver_customer_conformed and silver_customer_xref from the Warehouse by
    # three-part name, so they have to reach the SQL endpoint too — without
    # this the star fails on `Invalid object name '<lakehouse GUID>...'`, an
    # error that names the database and not the missing step.
    ("reflect", "reflect the resolved tables star_silver just wrote"),
    ("gold_star", "dbt-fabric: the multi-source star, joined in the WAREHOUSE"),
    ("contract_gates", "run the ODCS contracts as gates at every layer"),
    ("tmdl_pbip", "serialise the model as TMDL; lay out a .pbip project"),
    ("govern", "catalog the medallion in OpenMetadata (skipped if not running)"),
    # Last, because it reads what every step above wrote — and it reads the
    # OTHER example's output too, so it skips when run alone. See compare.py.
    ("compare", "compare this build against the PySpark example's"),
]

STEPS = BASIC + ADVANCED


def run(steps, label="advanced medallion"):
    """Execute steps in order, stopping at the first failure."""
    basic = len(BASIC)
    for i, (step, title) in enumerate(steps, 1):
        track = "basic" if i <= basic else "advanced"
        print(f"==> [{i}/{len(steps)}] ({track}) {title}", flush=True)
        rc = subprocess.run([sys.executable, f"{step}.py"], cwd=HERE).returncode
        if rc != 0:
            sys.exit(f"FAILED at {step}.py (exit {rc})")
    print(f"==> {label} complete: {len(steps)}/{len(steps)} steps passed", flush=True)


if __name__ == "__main__":
    run(STEPS)
