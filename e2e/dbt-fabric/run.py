#!/usr/bin/env python3
"""e2e: Microsoft's real dbt-fabric adapter drives a dbt project through the
emulator's TDS warehouse surface via mssql-python, authenticated by
entra-emulator. Brings the stack up and asserts dbt debug -> seed -> run ->
test all pass (--exit-code-from dbt). Linux weight class (SQL Server container)."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
SERVICES_TO_LOG = ["fabric-emulator", "sqlserver", "dbt"]


# Coverage: layer the overlay so this suite's run contributes counters to
# the merged profile. Only when asked, so the default path is unchanged
# (e2e/docker-compose.coverage.yml, docs/10-testing.md).
def _cov():
    if not os.environ.get("FABRIC_COVERAGE"):
        return []
    return ["-f", "docker-compose.yml",
            "-f", os.path.join("..", "docker-compose.coverage.yml")]


def compose(*args):
    return subprocess.run(["docker", "compose", *_cov(), *args], cwd=DIR).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "dbt")
    if rc != 0:
        for svc in SERVICES_TO_LOG:
            print(f"\n==== {svc} logs (tail) ====", file=sys.stderr)
            subprocess.run(["docker", "compose", "logs", "--tail", "60", svc], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
