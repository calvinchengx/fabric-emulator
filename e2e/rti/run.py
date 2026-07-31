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

UNSUPPORTED_ARCH = """\
The Kusto engine cannot run on this Docker host.

kustainer is x86-64 only: its native layer (libKusto.NativeInfra.so) needs AVX2,
and Rosetta exposes SSE4.2 without it, so the container crashes during boot with
`Crash_FailSlow` instead of failing cleanly. Emulating amd64 on Apple silicon
does not help — this needs a genuine x86-64 kernel.

Docker daemon architecture: {arch}

Two ways forward:
  * CI — the `rti` job on GitHub Actions' amd64 runners is the witness of
    record for this suite; push and read its log.
  * Locally — run a real x86-64 VM and point Docker at it. `--cpu-type max` is
    the part that matters: QEMU's default model omits AVX2 too.
        brew install colima qemu lima-additional-guestagents
        colima start --profile fabric-x86 --arch x86_64 --vm-type qemu \\
            --cpu-type max --memory 8 --cpus 4 --disk 60
        export DOCKER_CONTEXT=colima-fabric-x86
    Expect it to be slow: every instruction is translated. See
    docs/25-rti-kusto.md.

Set RTI_FORCE=1 to run anyway.\
"""


def docker_arch():
    """The architecture of the Docker *daemon*, which is what actually runs the
    engine. Deliberately not platform.machine(): with an x86-64 Colima VM the
    host stays arm64 while the daemon is x86-64, and checking the host would
    refuse a setup that works."""
    try:
        out = subprocess.run(
            ["docker", "version", "--format", "{{.Server.Arch}}"],
            capture_output=True, text=True, timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    return out.stdout.strip() if out.returncode == 0 else ""


def compose(*args):
    return subprocess.run(COMPOSE + list(args))


arch = docker_arch()
if arch and arch not in ("amd64", "x86_64") and not os.environ.get("RTI_FORCE"):
    sys.exit(UNSUPPORTED_ARCH.format(arch=arch))


try:
    result = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "witness")
    if result.returncode:
        for service in ("witness", "fabric-emulator", "kustainer"):
            sys.stderr.write(f"\n==== {service} log ====\n")
            compose("logs", service)
    sys.exit(result.returncode)
finally:
    compose("down", "-v")
