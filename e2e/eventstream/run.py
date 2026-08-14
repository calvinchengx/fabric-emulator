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
