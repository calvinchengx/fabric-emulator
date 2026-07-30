#!/usr/bin/env python3
"""Run the representative notebook on Apache Spark 3.5 / Delta 3.2."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.jvm.yml")]


def compose(*args):
    return subprocess.run(COMPOSE + list(args))


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "spark-jvm").returncode
    if rc:
        compose("logs", "fabric-emulator", "spark-jvm")
    sys.exit(rc)
finally:
    compose("down", "-v")
