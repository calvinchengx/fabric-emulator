#!/usr/bin/env python3
"""e2e S0: LakeSail's Sail (Rust Spark-Connect server) replaces JVM Spark —
a real PySpark client writes/reads Delta through fabric-emulator's OneLake
plane with entra client-credentials auth. No JVM in any container.
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]
# Coverage: layer the overlay so this suite's run contributes counters
# to the merged profile (e2e/docker-compose.coverage.yml, docs/10-testing.md).
if os.environ.get("FABRIC_COVERAGE"):
    COMPOSE += ["-f", os.path.join(os.path.dirname(DIR), "docker-compose.coverage.yml")]


def compose(*args, check=False):
    return subprocess.run(COMPOSE + list(args), check=check)


try:
    rc = compose("up", "--build", "--abort-on-container-exit",
                 "--exit-code-from", "driver").returncode
    if rc != 0:
        for svc in ("sail", "fabric-emulator"):
            sys.stderr.write(f"\n==== {svc} log ====\n")
            compose("logs", svc)
    sys.exit(rc)
finally:
    compose("down", "-v")
