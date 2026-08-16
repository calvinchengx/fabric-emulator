#!/usr/bin/env python3
"""e2e: Microsoft's Azure CLI drives Data Factory pipeline ACTIVITIES.

Delete, GetMetadata, Lookup and Validation, each asserted by what it did to the
data rather than by the status it reported. A separate job from e2e/az-rest so
one container cannot take two dozen parity rows red at once; it reuses that
suite's login by importing it, so the `az login` machinery has one home.
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))


def compose(*a):
    return subprocess.run(["docker", "compose", *a], cwd=DIR).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "client")
    if rc != 0:
        for svc in ("client", "fabric", "entra"):
            sys.stderr.write(f"\n==== {svc} logs ====\n")
            subprocess.run(["docker", "compose", "logs", "--tail", "60", svc], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
