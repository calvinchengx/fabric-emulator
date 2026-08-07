#!/usr/bin/env python3
import os
import subprocess

HERE = os.path.dirname(os.path.abspath(__file__))
cmd = ["docker", "compose", "-p", "fabric-salesforce-e2e", "-f", os.path.join(HERE, "docker-compose.yml")]
# Coverage: layer the overlay so this suite's run contributes counters
# to the merged profile (e2e/docker-compose.coverage.yml, docs/10-testing.md).
if os.environ.get("FABRIC_COVERAGE"):
    cmd += ["-f", os.path.join(os.path.dirname(HERE), "docker-compose.coverage.yml")]
try:
    subprocess.run(cmd + ["up", "--build", "--abort-on-container-exit", "--exit-code-from", "test"], check=True)
finally:
    subprocess.run(cmd + ["down", "-v", "--remove-orphans"], check=False)
