"""Silver: deploy the transform as a NOTEBOOK, submit it, verify what landed.

The transform itself is `definitions/silver.Notebook/notebook-content.py`. This
step deploys that definition and submits a `RunNotebook` job — which is the whole
reason it is shaped this way.

WHAT CHANGED AND WHY IT MATTERS. This step used to build the DataFrames here and
dial Spark Connect directly. That works against the emulator, which attaches an
engine (Sail) on a port, and it can never work against real Fabric, which exposes
no Spark endpoint to dial: on Fabric, Spark runs INSIDE the service and the unit
of work is a submitted job. So the step was portable in every respect except the
one that mattered, and it refused under FABRIC_TARGET=real rather than pretend.
Moving the code into a notebook definition removes the refusal: the emulator's
Spark agent executes the cells locally, real Fabric executes the same cells on
the workspace's starter pool (or a custom pool via an Environment), and neither
this file nor the notebook changes between them.

WHY THE ASSERTIONS LIVE HERE AND THE TRANSFORM DOES NOT. A notebook runs on the
pool, where the example's fixture package does not exist — so the numbers the
seeded generator implies cannot be checked in there. They are checked here, by
reading the Delta tables the run produced. That is the stronger check: it
verifies the ARTIFACT rather than an in-process DataFrame, so a transform that
computed the right answer and wrote it to the wrong place still fails.

The same transform written as **dbt-fabricspark models** is in
`../medallion-advanced-dbt-fabricspark`, which builds the identical silver
declaratively and then compares the two. Imperative Spark or declarative dbt is a
real choice a Fabric team makes; neither example claims it is the only one.
"""
import json
import pathlib
import time

import source_system as src
from common import (
    create_item_from_definition,
    load,
    log,
    report_lineage,
    run_job,
    storage_options,
    tables_uri,
)
from deltalake import DeltaTable

st = load()

nb = create_item_from_definition(
    "silver.Notebook", WORKSPACE_ID=st["workspace"], LAKEHOUSE_ID=st["lakehouse"])

# The build clock covers the submit and the run, which is what a reader comparing
# engines cares about: the wall time from "go" to "silver exists".
t0 = time.time()
jid, status = run_job(nb, "RunNotebook")
assert status == "Completed", f"silver notebook run {jid}: {status}"
build_secs = time.time() - t0

# --- verify what landed -------------------------------------------------------
# delta-rs reads OneLake directly with a storage-audience token, independent of
# whatever engine wrote the tables. Deliberately a different reader from the
# writer: if both halves were Spark, a Spark-shaped misunderstanding of the Delta
# log would agree with itself.
opts = storage_options()


def table(name):
    return DeltaTable(f"{tables_uri()}/{name}", storage_options=opts).to_pandas()


customers = table("silver_customers")
orders = table("silver_orders")
quarantine = table("silver_quarantine_orders")

n_customers, n_orders, n_quarantine = len(customers), len(orders), len(quarantine)
countries = set(customers["country"].unique())

assert n_customers == src.EXPECTED_SILVER_CUSTOMERS, n_customers
assert n_orders == src.EXPECTED_SILVER_ORDERS, n_orders
assert n_quarantine == src.EXPECTED_QUARANTINED, n_quarantine
assert countries == src.EXPECTED_COUNTRIES, countries
assert len(customers.columns) == src.EXPECTED_CUSTOMER_COLUMNS, list(customers.columns)
assert orders["order_id"].nunique() == n_orders, "silver_orders still has duplicate order ids"
# The unresolvable people are still here, not quietly dropped — the resolution
# step in the advanced example depends on them existing to prove that a claim of
# 100% is a lie. The notebook asserts this too; both are cheap and the failure
# reads differently from each side.
assert (customers["email"] == "").sum() > 0, "the missing-email cohort vanished"

log(f"silver (notebook on the target's own Spark): {n_customers:,} customers x "
    f"{len(customers.columns)} cols, {n_orders:,} orders, {n_quarantine:,} quarantined")

# A machine-readable summary of what this tool produced, so
# ../medallion-dbt-fabricspark can compare its declarative build against this
# imperative one without either example importing the other's code.
pathlib.Path(__file__).resolve().parent.joinpath("silver_summary.json").write_text(
    json.dumps({
        "engine": "PySpark (Notebook job)",
        "target": "Lakehouse (Delta in OneLake)",
        "compute": "the target's own Spark: agent-driven Sail locally, a Fabric pool on real",
        "build_seconds": round(build_secs, 2),
        "rows": {"silver_customers": n_customers, "silver_orders": n_orders,
                 "silver_quarantine_orders": n_quarantine},
        # Empty, and that is the finding rather than an omission: Spark SQL
        # needs no statement rewriting on the wire. The Warehouse half of this
        # example does (docs/29-tsql-parity.md, T6 and T8).
        "dialect_adaptations": [],
    }, indent=2))

# The flow graph's bronze -> silver hop. The notebook writes Delta straight to
# OneLake, so the emulator sees every byte and every table version — but not
# which input produced which output. This step is the only thing that knows.
#
# Two movements, not one cross product: the conformed customers come from the
# customer export alone, and both order tables — clean and quarantined — are the
# two halves of the same order export. Reporting one reads/writes pair would
# claim bronze_customers produced the quarantine, which it did not.
_lake = st["lakehouse"]
report_lineage("silver", [
    ([(_lake, "Tables/bronze_customers")], [(_lake, "Tables/silver_customers")]),
    ([(_lake, "Tables/bronze_orders")],
     [(_lake, "Tables/silver_orders"), (_lake, "Tables/silver_quarantine_orders")]),
])
