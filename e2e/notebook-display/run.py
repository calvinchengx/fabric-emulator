#!/usr/bin/env python3
"""`display()` under a real Jupyter kernel — see client.py for what it proves."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))


def compose(*args):
    return subprocess.run(["docker", "compose", *args], cwd=DIR).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "client")
    if rc != 0:
        print("\n==== fabric-emulator logs ====", file=sys.stderr)
        subprocess.run(["docker", "compose", "logs", "fabric-emulator"], cwd=DIR)
        print("\n==== sail logs (tail) ====", file=sys.stderr)
        subprocess.run(["docker", "compose", "logs", "--tail", "40", "sail"], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
