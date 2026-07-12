#!/usr/bin/env python3
"""e2e A2: real JVM Spark (delta-spark) writes and reads a Delta table through
fabric-emulator's OneLake plane via the Hadoop ABFS driver, authenticated to
entra-emulator.

The container network is the crux — the ABFS driver takes no endpoint
override, so onelake.dfs.fabric.microsoft.com resolves (via a compose network
alias) to fabric-emulator. fabric-emulator is built from this working tree, so
the e2e tests the current code. Docker + a JVM Spark image make this a heavier
job than the pure-wheel e2es; it runs Linux-only in CI.
"""
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
