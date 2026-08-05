import collections
import sys

sys.path.insert(0, ".")
from common import FABRIC, S, fabric_headers, load

st = load()
edges = S.get(f"{FABRIC}/v1/workspaces/{st['workspace']}/lineage", headers=fabric_headers()).json()["value"]
print(collections.Counter(e["producer"] for e in edges))
for e in edges:
    if e["producer"] == "Warehouse":
        print("  WH:", e["sourcePath"], "->", e["targetPath"])
