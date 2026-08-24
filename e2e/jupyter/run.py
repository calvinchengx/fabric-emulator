#!/usr/bin/env python3
"""Execute the shipped JupyterLab image: one notebook, kernel and agent, compared."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]
# Coverage: layer the overlay so this suite's run contributes counters
# to the merged profile (e2e/docker-compose.coverage.yml, docs/10-testing.md).
if os.environ.get("FABRIC_COVERAGE"):
    COMPOSE += ["-f", os.path.join(os.path.dirname(DIR), "docker-compose.coverage.yml")]


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
