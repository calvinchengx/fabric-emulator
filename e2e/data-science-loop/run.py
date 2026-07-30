#!/usr/bin/env python3
"""Run the Spark -> Direct Lake -> MLflow -> dbt composed witness."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]


def compose(*args):
    return subprocess.run(COMPOSE + list(args))


try:
    result = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "witness")
    if result.returncode:
        for service in ("witness", "fabric-emulator", "mlflow", "sail"):
            sys.stderr.write(f"\n==== {service} log ====\n")
            compose("logs", service)
    sys.exit(result.returncode)
finally:
    compose("down", "-v")
