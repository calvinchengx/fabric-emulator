#!/usr/bin/env python3
"""Hit the DAX pump in front of a live Desktop msmdsrv, and diff the rows.

This is the pump witness (docs/52 Phase 1-2). e2e/pbix-desktop already proves
ADOMD against Desktop; this file proves the HTTP front the emulator actually
calls. It does not start fabric-emulator - the Go relay is unit-tested against
a mock pump. What only Windows can show is that /v1/dax returns Desktop's
numbers.

    STAGE health :: OK | <reason>
    STAGE query  :: OK | <reason>
    PUMP AGREES WITH executeQueries: True|False
"""
from __future__ import annotations

import argparse
import json
import pathlib
import sys
import time
import urllib.error
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
DESKTOP = HERE.parent / "pbix-desktop"
sys.path.insert(0, str(DESKTOP))
from verify import compare  # noqa: E402

DAX = ("EVALUATE SUMMARIZECOLUMNS(Customer[Country], "
       '"Revenue", [Total Revenue], "PerUnit", [Revenue per Unit])')
FIELDS = {"Total Revenue": "[Revenue]", "Revenue per Unit": "[PerUnit]"}
KEY = "Customer[Country]"


def stage(name: str, outcome: str) -> None:
    print(f"STAGE {name} :: {outcome}", flush=True)


def http_json(method: str, url: str, body: dict | None = None, timeout: float = 60) -> tuple[int, dict]:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            parsed = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            parsed = {"error": {"message": raw.decode(errors="replace")}}
        return e.code, parsed


def wait_health(base: str, deadline: float) -> None:
    url = base.rstrip("/") + "/health"
    last = "no response"
    while time.time() < deadline:
        try:
            code, body = http_json("GET", url, timeout=5)
            if code == 200 and body.get("ok") is True:
                stage("health", f"OK port={body.get('port', '')}")
                return
            last = f"HTTP {code} {body}"
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as e:
            last = f"{type(e).__name__}: {e}"
        time.sleep(1)
    stage("health", last)
    raise SystemExit(1)


def json_rows(payload: dict) -> list[dict[str, str]]:
    rows = []
    for row in payload.get("rows") or []:
        rows.append({k: "" if v is None else str(v) for k, v in row.items()})
    return rows


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--pump", default="http://127.0.0.1:8080")
    ap.add_argument("--wait-sec", type=int, default=60)
    args = ap.parse_args()

    wait_health(args.pump, time.time() + args.wait_sec)

    code, body = http_json("POST", args.pump.rstrip("/") + "/v1/dax", {"query": DAX})
    if code != 200:
        msg = (body.get("error") or {}).get("message") or json.dumps(body)
        stage("query", f"HTTP {code} :: {msg}")
        return 1
    stage("query", "OK")

    golden = json.loads((DESKTOP / "fixture" / "expected.json").read_text())
    ok, lines = compare(golden["rows"], json_rows(body), KEY, FIELDS)
    for ln in lines:
        print(ln)
    print("PUMP AGREES WITH executeQueries:", ok)
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
