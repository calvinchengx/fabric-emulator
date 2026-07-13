#!/usr/bin/env python3
"""e2e: Microsoft's real dbt-fabric adapter drives a dbt project through the
emulator's TDS warehouse surface via the Microsoft ODBC Driver 18, authenticated
by entra-emulator. Brings the stack up and asserts dbt debug -> seed -> run ->
test all pass (--exit-code-from dbt). Linux weight class (SQL Server container)."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
SERVICES_TO_LOG = ["fabric-emulator", "sqlserver", "dbt"]


def compose(*args):
    return subprocess.run(["docker", "compose", *args], cwd=DIR).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "dbt")
    if rc != 0:
        for svc in SERVICES_TO_LOG:
            print(f"\n==== {svc} logs (tail) ====", file=sys.stderr)
            subprocess.run(["docker", "compose", "logs", "--tail", "60", svc], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
