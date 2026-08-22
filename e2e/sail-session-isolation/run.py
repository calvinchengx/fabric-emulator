#!/usr/bin/env python3
"""Run the Sail session-isolation spike (docs/54, stage 5)."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))


def compose(*args):
    return subprocess.run(["docker", "compose", *args], cwd=DIR).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "probe")
    if rc != 0:
        print("\n==== sail logs ====", file=sys.stderr)
        subprocess.run(["docker", "compose", "logs", "--tail", "40", "sail"], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
