#!/usr/bin/env python3
"""e2e: Microsoft's Fabric CLI (fab) provisions, uploads, imports, RUNS, and
reads back — the whole of examples/fab-driven, unmodified.

WHY THIS IS NOT e2e/fabric-cli. That suite drives fab over EMPTY items: mkdir a
Notebook with no cells, a DataPipeline with no activities, then ls/get/rm.
Nothing it creates can be executed. This one uploads real definitions with
`fab import`, moves 71 MB into OneLake with `fab cp`, runs both items with
`fab job run`, and checks the Delta bytes that came out. Two emulator defects
were found by exactly that difference — see docs/34-fab-driven-example.md.

WHY IT RUNS ON THE HOST rather than as a compose `client` service. The example
invokes `docker compose run --rm --no-deps fab …` for every fab command, so its
runner needs a docker socket. Putting it inside a container would mean
docker-in-docker to test a thing whose whole point is that a reader runs it on
their laptop. The GitHub runner has docker; so does the reader.

IMAGES ARE BUILT FROM THIS TREE, not pulled. examples/fab-driven/.env pins the
released versions a READER should use; CI must test the code in the commit, and
a suite that silently verified last release's image would be the most
comfortable kind of useless.
"""
import os
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
EXAMPLE = ROOT / "examples" / "fab-driven"
TAG = "ci"

# Built here, in this order: the emulator, then the two compute images the
# notebook leg needs. All three carry the same tag so the compose file's
# separate version knobs cannot drift apart in CI.
IMAGES = [
    ("ghcr.io/calvinchengx/fabric-emulator", "Dockerfile"),
    ("ghcr.io/calvinchengx/fabric-emulator-sail", "docker/sail/Dockerfile"),
    ("ghcr.io/calvinchengx/fabric-emulator-spark-agent", "docker/spark-agent/Dockerfile"),
]

ENV = {
    **os.environ,
    "FABRIC_EMULATOR_VERSION": TAG,
    "SAIL_VERSION": TAG,
    "SPARK_AGENT_VERSION": TAG,
}


def run(cmd, cwd, env=None, check=True):
    print(f"==> {' '.join(cmd)}", flush=True)
    rc = subprocess.run(cmd, cwd=cwd, env=env or ENV).returncode
    if check and rc != 0:
        sys.exit(f"FAILED: {' '.join(cmd)} (exit {rc})")
    return rc


def compose(*args, check=True):
    return run(["docker", "compose", *args], cwd=EXAMPLE, check=check)


def main() -> int:
    for image, dockerfile in IMAGES:
        run(["docker", "build", "-t", f"{image}:{TAG}", "-f", dockerfile, "."], cwd=ROOT)

    try:
        compose("up", "-d", "--build", "--wait")
        # The example, exactly as its README tells a reader to run it.
        # `uv` by name, not sys.executable: this script is itself run inside the
        # emulator's own uv environment, and the example owns a separate one.
        rc = run(["uv", "run", "--frozen", "python", "pipeline.py"],
                 cwd=EXAMPLE, check=False)
        if rc == 0:
            # The gate the example is built around lives in its unit tests, and
            # a run that never executed them would leave the one assertion that
            # can distinguish a passing job from a silent zero unexercised.
            rc = run(["uv", "run", "--frozen", "--group", "dev",
                      "pytest", "test_fabctl.py", "-q"], cwd=EXAMPLE, check=False)
        if rc != 0:
            for svc in ("fabric-emulator", "spark-agent", "sail", "entra-emulator"):
                sys.stderr.write(f"\n==== {svc} logs ====\n")
                subprocess.run(["docker", "compose", "logs", "--tail", "60", svc],
                               cwd=EXAMPLE, env=ENV)
        return rc
    finally:
        compose("down", "-v", check=False)


if __name__ == "__main__":
    sys.exit(main())
