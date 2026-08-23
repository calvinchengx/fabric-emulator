#!/usr/bin/env python3
"""The two-context split, witnessed: statements run in a process that holds no
service credential, and the privileged half supplies the filtered rows."""
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
BASE = os.path.join(HERE, "..", "livy")
FILES = ["-f", "docker-compose.yml", "-f", os.path.join("..", "two-context", "two-context.yml")]


def compose(*args):
    return subprocess.run(["docker", "compose", *FILES, *args], cwd=BASE).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "client")
    if rc != 0:
        print("\n==== spark-agent logs (tail) ====", file=sys.stderr)
        subprocess.run(["docker", "compose", *FILES, "logs", "--tail", "60", "spark-agent"],
                       cwd=BASE)
    sys.exit(rc)
finally:
    compose("down", "-v")
