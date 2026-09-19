#!/usr/bin/env python3
"""Native Livy termination with statements computed by Sail through a
PySpark Connect agent. There is no Apache Livy server or JVM in this stack."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))


# Coverage: layer the overlay so this suite's run contributes counters to
# the merged profile. Only when asked, so the default path is unchanged
# (e2e/docker-compose.coverage.yml, docs/10-testing.md).
def _cov():
    if not os.environ.get("FABRIC_COVERAGE"):
        return []
    return ["-f", "docker-compose.yml",
            "-f", os.path.join("..", "docker-compose.coverage.yml")]


def compose(*args):
    return subprocess.run(["docker", "compose", *_cov(), *args], cwd=DIR).returncode


# THE RECORDING DIRECTORY MUST BE WRITABLE BY A CONTAINER THAT IS NOT ROOT.
# The emulator image is distroless :nonroot (uid 65532) and this stack does not
# override the user, so on Linux -- where a bind mount belongs to the host user
# -- the container cannot create the file. Recording is diagnostic and never
# fatal, so the emulator starts anyway and the suite PASSES HAVING RECORDED
# NOTHING. Measured twice now: once on e2e/medallion, and again here, where the
# artifact simply did not appear and the aggregate gate failed with eight
# routes it had been told to expect. macOS bind mounts are permissive about
# uid, so a local run shows nothing wrong, and git does not carry a directory
# mode, so a chmod on a developer machine does not travel.
#
# Cleared as well, because the recorder APPENDS and the mount outlives the
# stack: a second local run would otherwise validate the first run's traffic.
_recording = os.path.join(DIR, "recording")
os.makedirs(_recording, exist_ok=True)
os.chmod(_recording, 0o777)
_responses = os.path.join(_recording, "responses.jsonl")
if os.path.exists(_responses):
    os.remove(_responses)

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
