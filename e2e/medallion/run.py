#!/usr/bin/env python3
"""e2e: the medallion tutorial, executed.

Brings up the family (entra + keyvault + fabric + SQL Server) and runs the full
pipeline — Key Vault secret → landing → bronze → silver → gold (dbt) → semantic
model — asserting every hop. The runnable witness for
docs/28-tutorial-end-to-end.md. Linux weight class (SQL Server container).
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
SERVICES_TO_LOG = ["pipeline", "fabric-emulator", "sqlserver", "keyvault-emulator"]


def compose(*args):
    return subprocess.run(["docker", "compose", *args], cwd=DIR).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "pipeline")
    if rc != 0:
        for svc in SERVICES_TO_LOG:
            print(f"\n==== {svc} logs (tail) ====", file=sys.stderr)
            subprocess.run(["docker", "compose", "logs", "--tail", "80", svc], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v", "--remove-orphans")
