#!/usr/bin/env python3
"""Build the .pbix Power BI Desktop will be asked to open.

Built here rather than committed: a 2 MB binary in git that nobody can diff is
a liability, and building it in the job proves the WRITE path works on the same
runner that then tests the read path.

pbix-mcp is installed into a throwaway location, never into uv.lock — see the
non-goal in docs/33-pbix-tooling.md. It is verification tooling; the emulator
must not acquire a dependency on it.
"""
import json
import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
PIN = "pbix-mcp==0.9.78"  # pinned: the thing under observation is somebody else's release

subprocess.run([sys.executable, "-m", "pip", "install", "--quiet", "--disable-pip-version-check", PIN],
               check=True)

from pbix_mcp.builder import PBIXBuilder  # noqa: E402  (installed just above)

model = json.loads((HERE / "fixture" / "model.bim").read_text())
rows = json.loads((HERE / "fixture" / "rows.json").read_text())

b = PBIXBuilder(model["name"])
for t in model["model"]["tables"]:
    cols = [{"name": c["name"], "dataType": c["dataType"]} for c in t["columns"]]
    data = [{c["name"]: r.get(c["name"]) for c in t["columns"]} for r in rows.get(t["name"], [])]
    b.add_table(t["name"], cols, rows=data)
for t in model["model"]["tables"]:
    for m in t.get("measures", []):
        b.add_measure(t["name"], m["name"], m["expression"])
for r in model["model"].get("relationships", []):
    b.add_relationship(r["fromTable"], r["fromColumn"], r["toTable"], r["toColumn"])

out = HERE / "model.pbix"
b.save(str(out))
print(f"BUILT {out} {out.stat().st_size} bytes")
