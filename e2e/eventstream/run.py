#!/usr/bin/env python3
"""Run the Eventstream Kafka witness on JVM Spark, Sail, or both.

EVENTSTREAM_ENGINE=jvm|sail|both  (default: jvm, matching the weekly spark-jvm job)
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]
if os.environ.get("FABRIC_COVERAGE"):
    COMPOSE += ["-f", os.path.join(os.path.dirname(DIR), "docker-compose.coverage.yml")]


def compose(*args):
    return subprocess.run(COMPOSE + list(args))


def run_engine(profile, exit_from):
    rc = compose("--profile", profile, "up", "--build",
                 "--abort-on-container-exit", "--exit-code-from", exit_from).returncode
    if rc:
        compose("logs", "fabric-emulator", "kafka", exit_from)
    return rc


engine = os.environ.get("EVENTSTREAM_ENGINE", "jvm").strip().lower()
if engine not in ("jvm", "sail", "both"):
    print(f"EVENTSTREAM_ENGINE={engine!r} (want jvm|sail|both)", file=sys.stderr)
    sys.exit(2)

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
    rc = 0
    if engine in ("jvm", "both"):
        rc = run_engine("jvm", "spark-jvm")
        if rc:
            sys.exit(rc)
        compose("down", "-v")
    if engine in ("sail", "both"):
        rc = run_engine("sail", "spark")
    sys.exit(rc)
finally:
    compose("down", "-v")
