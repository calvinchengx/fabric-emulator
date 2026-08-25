#!/usr/bin/env python3
"""The emulator leg of the tenant engine probes — see tenant.py for what it proves."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", "docker-compose.tenant.yml"]

try:
    rc = subprocess.run([*COMPOSE, "up", "--build", "--abort-on-container-exit",
                         "--exit-code-from", "client"], cwd=DIR).returncode
    if rc != 0:
        print("\n==== fabric-emulator logs ====", file=sys.stderr)
        subprocess.run([*COMPOSE, "logs", "fabric-emulator"], cwd=DIR)
        print("\n==== spark-agent logs (tail) ====", file=sys.stderr)
        subprocess.run([*COMPOSE, "logs", "--tail", "40", "spark-agent"], cwd=DIR)
    sys.exit(rc)
finally:
    subprocess.run([*COMPOSE, "down", "-v"], cwd=DIR)
