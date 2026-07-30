#!/usr/bin/env python3
"""Run the common Spark API / Delta workload on the default Sail engine."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]


def compose(*args, check=False):
    return subprocess.run(COMPOSE + list(args), check=check)


try:
    # Build fabric (from source) + the Spark image, bring the stack up, and run
    # the job. --exit-code-from spark surfaces the job's pass/fail; the job
    # asserts internally (write, read-back, append).
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "spark").returncode
    if rc != 0:
        sys.stderr.write("\n==== fabric-emulator log ====\n")
        compose("logs", "fabric-emulator")
    sys.exit(rc)
finally:
    compose("down", "-v")
