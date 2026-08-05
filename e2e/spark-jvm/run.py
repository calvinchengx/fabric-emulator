#!/usr/bin/env python3
"""Run the optional Apache Spark 3.5 compatibility oracle."""
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
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "spark-jvm").returncode
    if rc:
        compose("logs", "fabric-emulator", "spark-jvm")
    sys.exit(rc)
finally:
    compose("down", "-v")
