#!/usr/bin/env python3
"""Run the Real-Time Intelligence witness: Microsoft's KQL engine (kustainer)
behind the emulator's Eventhouse / KQL Database surface.

Linux/amd64 only — Microsoft documents ARM as unsupported for the engine
container (its native layer needs AVX2, which Apple-silicon emulation does not
expose), so this suite runs on the amd64 CI runners, like the other
container-stack e2es.
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE = ["docker", "compose", "-f", os.path.join(DIR, "docker-compose.yml")]


def compose(*args):
    return subprocess.run(COMPOSE + list(args))


try:
    result = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "witness")
    if result.returncode:
        for service in ("witness", "fabric-emulator", "kustainer"):
            sys.stderr.write(f"\n==== {service} log ====\n")
            compose("logs", service)
    sys.exit(result.returncode)
finally:
    compose("down", "-v")
