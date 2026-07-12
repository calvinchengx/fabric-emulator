#!/usr/bin/env python3
"""e2e: real notebook cell execution. The emulator parses a Fabric notebook and
real JVM Spark executes its cells against the OneLake plane, reporting the run
back so the RunNotebook job reflects real execution (see runner.py).

Heavier than the pure-wheel e2es (Docker + a JVM Spark image); Linux-only in CI,
alongside the spark-a2 e2e whose image it reuses."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]


def compose(*args):
    return subprocess.run(COMPOSE + list(args))


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "spark").returncode
    if rc != 0:
        sys.stderr.write("\n==== fabric-emulator log ====\n")
        compose("logs", "fabric-emulator")
    sys.exit(rc)
finally:
    compose("down", "-v")
