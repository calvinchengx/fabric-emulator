#!/usr/bin/env python3
"""Run the Spark -> Direct Lake -> MLflow -> dbt composed witness."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]
# Coverage: layer the overlay so this suite's run contributes counters
# to the merged profile (e2e/docker-compose.coverage.yml, docs/10-testing.md).
if os.environ.get("FABRIC_COVERAGE"):
    COMPOSE += ["-f", os.path.join(os.path.dirname(DIR), "docker-compose.coverage.yml")]


def compose(*args):
    return subprocess.run(COMPOSE + list(args))


# THE RECORDING DIRECTORY MUST BE WRITABLE BY A CONTAINER THAT IS NOT ROOT.
# The emulator image is distroless :nonroot (uid 65532), so on Linux -- where a
# bind mount belongs to the host user -- the container cannot create the file,
# recording is never fatal by design, and the suite PASSES HAVING RECORDED
# NOTHING. Measured on medallion, livy and sail before this was added here;
# macOS bind mounts are permissive about uid, so a local run shows nothing
# wrong. Cleared as well: the recorder APPENDS and the mount outlives the stack.
_recording = os.path.join(DIR, "recording")
os.makedirs(_recording, exist_ok=True)
os.chmod(_recording, 0o777)
_responses = os.path.join(_recording, "responses.jsonl")
if os.path.exists(_responses):
    os.remove(_responses)


try:
    result = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "witness")
    if result.returncode:
        for service in ("witness", "fabric-emulator", "mlflow", "sail"):
            sys.stderr.write(f"\n==== {service} log ====\n")
            compose("logs", service)
    sys.exit(result.returncode)
finally:
    compose("down", "-v")
