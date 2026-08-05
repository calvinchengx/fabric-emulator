#!/usr/bin/env python3
"""Fabric Data Engineering VS Code extension 1.18.1 contract witness."""
import os
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent


# Coverage: layer the overlay so this suite's run contributes counters to
# the merged profile. Only when asked, so the default path is unchanged
# (e2e/docker-compose.coverage.yml, docs/10-testing.md).
def _cov():
    if not os.environ.get("FABRIC_COVERAGE"):
        return []
    return ["-f", "docker-compose.yml",
            "-f", os.path.join("..", "docker-compose.coverage.yml")]


def compose(*args: str) -> int:
    return subprocess.run(["docker", "compose", *_cov(), *args], cwd=HERE).returncode


try:
    result = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "client")
    if result:
        for service in ("client", "fabric-emulator", "entra-emulator"):
            print(f"\n==== {service} logs ====", file=sys.stderr)
            subprocess.run(["docker", "compose", "logs", "--tail", "100", service], cwd=HERE)
    raise SystemExit(result)
finally:
    compose("down", "-v", "--remove-orphans")
