#!/usr/bin/env python3
"""e2e: native Livy termination + real Spark. Brings up entra + fabric + a Spark
statement-executor agent + a client, and asserts the client gets real
Spark-computed results through the emulator's own Livy layer (no Apache Livy
server). Linux-friendly; the JVM Spark image is a heavier weight class, like the
other Spark e2e."""
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
