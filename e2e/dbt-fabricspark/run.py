#!/usr/bin/env python3
"""e2e: Microsoft's real dbt-fabricspark adapter drives a dbt project through
the emulator's Livy High-Concurrency surface onto real Spark, authenticated by
entra-emulator. Brings the stack up and asserts dbt debug -> seed -> run -> test
all pass (--exit-code-from dbt). Heavier weight class (JVM Spark image), like the
other Spark e2es; Linux-friendly."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
SERVICES_TO_LOG = ["fabric-emulator", "spark-agent", "dbt"]


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
