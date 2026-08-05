#!/usr/bin/env python3
"""Run the Spark -> Direct Lake -> MLflow -> dbt composed witness."""
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
    result = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "witness")
    if result.returncode:
        for service in ("witness", "fabric-emulator", "mlflow", "sail"):
            sys.stderr.write(f"\n==== {service} log ====\n")
            compose("logs", service)
    sys.exit(result.returncode)
finally:
    compose("down", "-v")
