#!/usr/bin/env python3
"""Run a Fabric notebook with no runner in the stack — only published services."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]


def compose(*args):
    return subprocess.run(COMPOSE + list(args))


try:
    rc = compose("up", "--build", "--abort-on-container-exit",
                 "--exit-code-from", "client").returncode
    if rc != 0:
        sys.stderr.write("\n==== fabric-emulator log ====\n")
        compose("logs", "fabric-emulator")
        sys.stderr.write("\n==== spark-agent log ====\n")
        compose("logs", "spark-agent")
    sys.exit(rc)
finally:
    compose("down", "-v")
