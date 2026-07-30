#!/usr/bin/env python3
"""Native Livy termination with statements computed by Sail through a
PySpark Connect agent. There is no Apache Livy server or JVM in this stack."""
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
        print("\n==== spark-agent logs (tail) ====", file=sys.stderr)
        subprocess.run(["docker", "compose", "logs", "--tail", "40", "spark-agent"], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
