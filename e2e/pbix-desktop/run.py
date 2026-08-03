#!/usr/bin/env python3
"""Query the model Power BI Desktop loaded, and diff it against our golden.

Reads the port Desktop wrote, drives Microsoft's ADOMD.NET client against it,
and compares. Every stage is reported; the exit code is a summary, never the
finding.
"""
import argparse
import json
import pathlib
import re
import subprocess
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from verify import ProbeError, compare, parse_port_file, parse_rows, stage_results  # noqa: E402

HERE = pathlib.Path(__file__).resolve().parent
DAX = ('EVALUATE SUMMARIZECOLUMNS(Customer[Country], '
       '"Revenue", [Total Revenue], "PerUnit", [Revenue per Unit])')
FIELDS = {"Total Revenue": "[Revenue]", "Revenue per Unit": "[PerUnit]"}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--desktop-log", required=True,
                    help="output of desktop.ps1, which names the port file")
    args = ap.parse_args()

    log = pathlib.Path(args.desktop_log).read_text(errors="replace")
    stages = stage_results(log)
    for name, outcome in stages.items():
        print(f"desktop STAGE {name} :: {outcome}")
    bad = {k: v for k, v in stages.items() if not v.startswith("OK")}
    if bad:
        # Named, not summarised: "never hosted Analysis Services" and "the query
        # failed" are opposite findings about whether Desktop can run here.
        print(f"FAILED before the query: {bad}")
        return 1

    m = re.search(r"^PORTFILE (.+)$", log, re.M)
    if not m:
        print("desktop.ps1 reported OK but named no port file")
        return 1
    try:
        port = parse_port_file(pathlib.Path(m.group(1).strip()).read_bytes())
    except ProbeError as e:
        print(f"port file unreadable: {e}")
        return 1
    print(f"analysis services port: {port}")

    probe = HERE / "probe"
    r = subprocess.run(["dotnet", "run", "--project", str(probe), "-c", "Release"],
                       capture_output=True, text=True,
                       env={**__import__("os").environ,
                            "PBI_PORT": str(port), "PBI_DAX": DAX})
    out = r.stdout + r.stderr
    print(out)
    pstages = stage_results(out)
    if pstages.get("connect") != "OK":
        print(f"FAILED at connect :: {pstages.get('connect', '(no stage line)')}")
        return 1
    if pstages.get("query") != "OK":
        print(f"FAILED at query :: {pstages.get('query', '(no stage line)')}")
        return 1

    golden = json.loads((HERE / "fixture" / "expected.json").read_text())
    ok, lines = compare(golden["rows"], parse_rows(out), "Customer[Country]", FIELDS)
    for ln in lines:
        print(ln)
    print("DESKTOP AGREES WITH executeQueries:", ok)
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
