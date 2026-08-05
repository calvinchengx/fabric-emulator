#!/usr/bin/env python3
"""Fabric Data Engineering VS Code extension 1.18.1 contract witness."""
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent


def compose(*args: str) -> int:
    return subprocess.run(["docker", "compose", *args], cwd=HERE).returncode


try:
    result = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "client")
    if result:
        for service in ("client", "fabric-emulator", "entra-emulator"):
            print(f"\n==== {service} logs ====", file=sys.stderr)
            subprocess.run(["docker", "compose", "logs", "--tail", "100", service], cwd=HERE)
    raise SystemExit(result)
finally:
    compose("down", "-v", "--remove-orphans")
