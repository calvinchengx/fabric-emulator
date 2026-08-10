"""Assert the lineage graph the emulator recorded while the pipeline ran.

Two mechanisms produce these edges, and the graph keeps them distinguishable:

  * `Copy` — the emulator moved the data itself, so it knows the source and
    sink exactly.
  * `Notebook` — the engine that ran the cell reported what it read and wrote.

Neither is inferred from user code. That is the whole point: an edge here is
either something the emulator did, or something an engine witnessed doing.
"""
from common import lineage_edges, load, log, skip_on_real

# The flow graph is the EMULATOR's, and deliberately so: it records only what it
# did itself or what an engine witnessed doing, never an inference from user code.
# Real Fabric publishes no such API — its lineage story is Purview, a different
# model reached a different way — so there is nothing here to point at a tenant.
skip_on_real(
    "the lineage assertion",
    "the flow graph is the emulator's own record of what moved; real Fabric has "
    "no equivalent endpoint, and Purview is a different integration")

st = load()
edges = lineage_edges()
assert edges, "no lineage recorded — the pipeline should have produced edges"

by_producer = {}
for e in edges:
    by_producer.setdefault(e.get("producer", "Copy"), []).append(e)

# The Copy activity's edge: landing file → bronze table.
copies = by_producer.get("Copy", [])
assert any(e["targetPath"] == "Tables/bronze_customers" for e in copies), \
    f"Copy edge into bronze_customers missing: {copies}"

# The notebook's edge, reported by the engine that executed the cell.
notebook = by_producer.get("Notebook", []) + by_producer.get("NotebookObserved", [])
assert any(e["targetPath"] == "Tables/bronze_orders" for e in notebook), \
    f"Notebook edge into bronze_orders missing: {notebook}"

for producer, group in sorted(by_producer.items()):
    for e in group:
        log(f"{producer:16} {e['sourcePath']} -> {e['targetPath']}  ({e['activityName']})")
log(f"lineage: {len(edges)} edge(s) across {len(by_producer)} producer(s)")
