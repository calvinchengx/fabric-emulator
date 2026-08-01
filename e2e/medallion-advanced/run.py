#!/usr/bin/env python3
"""e2e: the ADVANCED medallion track, executed.

Runs `examples/medallion/run_advanced.py` — the whole basic pipeline, then the
steps from 20 up — on the same stack the basic harness brings up. This directory
holds no pipeline code and no stack definition: the example is the single copy
of one, and e2e/medallion/docker-compose.yml is the single copy of the other.

  python3 e2e/medallion-advanced/run.py

Linux weight class (SQL Server container), same as the basic harness.
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
BASE = os.path.join(DIR, "..", "medallion")
SERVICES_TO_LOG = ["pipeline", "fabric-emulator", "sqlserver", "keyvault-emulator"]


def compose(*args):
    # cwd is the BASE directory because the base file's build contexts are
    # relative to it. A distinct project name keeps this run from colliding with
    # the basic harness's containers and volumes.
    return subprocess.run([
        "docker", "compose",
        "-f", os.path.join(BASE, "docker-compose.yml"),
        "-f", os.path.join(DIR, "docker-compose.advanced.yml"),
        "-p", "medallion-advanced", *args], cwd=BASE).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "pipeline")
    if rc != 0:
        for svc in SERVICES_TO_LOG:
            print(f"\n==== {svc} logs (tail) ====", file=sys.stderr)
            compose("logs", "--tail", "80", svc)
    sys.exit(rc)
finally:
    compose("down", "-v", "--remove-orphans")
