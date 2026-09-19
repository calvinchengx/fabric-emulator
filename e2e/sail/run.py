#!/usr/bin/env python3
"""e2e S0: LakeSail's Sail (Rust Spark-Connect server) replaces JVM Spark —
a real PySpark client writes/reads Delta through fabric-emulator's OneLake
plane with entra client-credentials auth. No JVM in any container.
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]
# Coverage: layer the overlay so this suite's run contributes counters
# to the merged profile (e2e/docker-compose.coverage.yml, docs/10-testing.md).
if os.environ.get("FABRIC_COVERAGE"):
    COMPOSE += ["-f", os.path.join(os.path.dirname(DIR), "docker-compose.coverage.yml")]


# The CI job that runs this has a 25-minute budget, and a run that eats all of
# it is reported as CANCELLED — which reads as infrastructure noise and gets
# rerun rather than investigated. So the suite must always end on its own terms
# first. driver.py's per-step watchdog is the primary guard and names what it
# was waiting on; this is the backstop for a stack that hangs before the driver
# can say anything at all — a build that stalls, or sail never listening.
#
# THE BUDGETS MUST ADD UP, and the first draft of this did not: 900 up + 120 ps
# + 3x120 logs + 300 down is 28 minutes, so the guard could itself be cancelled
# at 25 and prove nothing. Every path below has a ceiling and they sum on
# purpose — 600 + 60 + 3x60 + 180 = 17 minutes, against a job budget of 25 and a
# normal run of about one. Change one number and re-add them.
BUDGET = float(os.environ.get("SAIL_E2E_BUDGET", "600"))
DIAGNOSTIC = 60.0
TEARDOWN = 180.0


def compose(*args, check=False, timeout=None):
    return subprocess.run(COMPOSE + list(args), check=check, timeout=timeout)


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
    try:
        rc = compose("up", "--build", "--abort-on-container-exit",
                     "--exit-code-from", "driver", timeout=BUDGET).returncode
    except subprocess.TimeoutExpired:
        sys.stderr.write(f"\n==== TIMEOUT: the stack did not finish in {BUDGET:.0f}s ====\n")
        compose("ps", timeout=DIAGNOSTIC)
        rc = 1
    if rc != 0:
        # driver first: its own output is the only thing that says how far the
        # run got, and on the timeout path `up` was killed mid-stream.
        for svc in ("driver", "sail", "fabric-emulator"):
            sys.stderr.write(f"\n==== {svc} log ====\n")
            compose("logs", svc, timeout=DIAGNOSTIC)
    sys.exit(rc)
finally:
    # Timed as well: the whole point is that nothing here can consume the job's
    # budget, and a diagnostic or a teardown hangs as readily as the run does.
    compose("down", "-v", timeout=TEARDOWN)
