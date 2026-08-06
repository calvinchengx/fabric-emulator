#!/usr/bin/env python3
"""Regenerate docs/demo/flow.gif: a medallion drawing itself in the flow view.

    uv run --frozen --group demo python docs/demo/flow.py

The companion of demo.tape. That records a terminal with VHS; this records a
browser with Playwright and converts the take to a GIF. Both are the source of
truth for the image they produce, which flow.gif previously was not: it was a
hand-cropped excerpt of a run in the contoso-data-platform repository, so nobody
could regenerate the README's hero image from this one, and it had gone stale
enough to predate the terminal pane.

WHAT IT FILMS. examples/medallion-advanced-pyspark, unmodified — the repository's
own advanced example, and the run the README describes: three source systems
landing, conformed, resolved into one customer identity, joined into a Warehouse
star, and served to a semantic model. 23 steps, PySpark on Sail for
bronze -> silver and dbt-fabric over real TDS for gold.

It films the example rather than a purpose-built seed because the hero image
should show the thing the README claims, driven by the code a reader can run.
Nothing here is staged for the camera: `pipeline.py` asserts its own results, so
a recording that completes is also a passing test.

Requires: docker, node, ffmpeg, Microsoft ODBC Driver 18 (dbt-fabric builds gold
over real TDS), and the portal's dev dependencies installed (`pnpm install`)
with a chromium
(`pnpm --filter fabric-emulator-portal exec playwright install chromium`).

Every published port is remapped away from the defaults, so this can run beside
a dev stack — or beside a sibling project holding 9443, which is why. The
example is pointed at them through its own environment variables.
"""
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))
CAPTURE = os.path.join(DIR, ".capture")
GIF = os.path.join(DIR, "flow.gif")

FABRIC_PORT = os.environ.get("DEMO_FABRIC_PORT", "9843")
ENTRA_PORT = os.environ.get("DEMO_ENTRA_PORT", "8843")
KV_PORT = os.environ.get("DEMO_KV_PORT", "8844")
TDS_PORT = os.environ.get("DEMO_TDS_PORT", "11533")
SPARK_PORT = os.environ.get("DEMO_SPARK_PORT", "50351")
OM_PORT = os.environ.get("DEMO_OM_PORT", "8685")
EXAMPLE = os.path.join(REPO, "examples", "medallion-advanced-pyspark")

# Recorded at this size; the GIF ships narrower (GIF_WIDTH) so seven columns
# of medallion fit the frame instead of four.
WIDTH = os.environ.get("DEMO_WIDTH", "1280")
HEIGHT = os.environ.get("DEMO_HEIGHT", "800")
# 8fps is enough for a graph that changes every second or two, and a third of
# the frames of the 25fps take. The old asset was 5fps.
FPS = os.environ.get("DEMO_FPS", "6")
# Played at 12x. The advanced example is 23 steps over about five minutes, and
# nobody watches a five-minute README image. Speeding the playback keeps every
# frame of what happened rather than trimming a window out of the middle — the
# graph still fills in visibly, because the steps that change it are spread
# through the run. docs/demo/README.md says the GIF is sped up.
SPEED = os.environ.get("DEMO_SPEED", "16")
# The width the GIF ships at, scaled down from the recording.
GIF_WIDTH = os.environ.get("DEMO_GIF_WIDTH", "960")
# A README image nobody waits for. The previous asset was 2.1 MB.
MAX_BYTES = int(os.environ.get("DEMO_MAX_BYTES", str(4 * 1024 * 1024)))

COMPOSE = ["docker", "compose", "-p", "fabricdemo-flow",
           "-f", os.path.join(REPO, "docker-compose.yml"),
           # The auto-override is what attaches Sail and SQL Server, and naming
           # any -f disables it — so it is named explicitly. Without it the
           # example's bronze->silver and gold steps have no engine.
           "-f", os.path.join(REPO, "docker-compose.override.yml"),
           "-f", os.path.join(DIR, "flow-override.yml"),
           # The example publishes to OpenMetadata at the end.
           "--profile", "governance"]


def log(msg):
    print(f"==> {msg}", flush=True)


def compose(*args, check=True):
    return subprocess.run(COMPOSE + list(args), check=check,
                          env={**os.environ, "DEMO_BUILD_CONTEXT": REPO})


def wait_for_portal(url, timeout=240):
    import ssl

    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    end = time.time() + timeout
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, timeout=3, context=ctx) as r:
                if r.status == 200:
                    return
        except (urllib.error.URLError, OSError):
            pass
        time.sleep(2)
    raise RuntimeError(f"the portal never answered at {url} within {timeout}s")


def run_example():
    """Run examples/medallion-advanced-pyspark, unmodified.

    Pointed at this recording's remapped ports through the environment variables
    the example already reads (examples/contoso-fixtures/common.py), so nothing
    about the example changes for the camera.
    """
    env = {
        **os.environ,
        "FABRIC_REST_URL": f"https://localhost:{FABRIC_PORT}",
        "ENTRA_URL": f"https://localhost:{ENTRA_PORT}",
        "KV_URL": f"https://localhost:{KV_PORT}",
        "TDS_SERVER": f"localhost,{TDS_PORT}",
        "SPARK_REMOTE": f"sc://localhost:{SPARK_PORT}",
        "OM_URL": f"http://localhost:{OM_PORT}",
    }
    log("resolving the example's own dependencies")
    subprocess.run(["uv", "sync", "--frozen"], cwd=EXAMPLE, check=True,
                   stdout=subprocess.DEVNULL)
    log("running the advanced medallion — 23 steps, about five minutes")
    subprocess.run(["uv", "run", "--frozen", "python", "pipeline.py"],
                   cwd=EXAMPLE, check=True, env=env)


def to_gif(webm):
    """Two-pass palette: one pass to choose 128 colours, one to map to them.

    A single-pass GIF of a dark UI bands badly in the chip colours, which are
    the part a reader is meant to read.

    64 colours, not more: the portal's dark palette is a handful of greys and one
    green, so the extra entries cost bytes and buy nothing. At 128 this take is
    4.2 MiB and over the ceiling; at 64 it is 3.3 MiB and looks the same.
    """
    log(f"converting to a {GIF_WIDTH}px {FPS}fps GIF at {SPEED}x")
    # setpts BEFORE fps: rescaling the timestamps and then sampling gives an
    # even frame spacing. The other order samples first and then stretches, which
    # leaves the motion juddering.
    filters = (f"setpts=PTS/{SPEED},fps={FPS},scale={GIF_WIDTH}:-1:flags=lanczos,"
               f"split[a][b];[a]palettegen=max_colors=64[p];"
               f"[b][p]paletteuse=dither=bayer:bayer_scale=3")
    subprocess.run(["ffmpeg", "-y", "-v", "error", "-i", webm,
                    "-vf", filters, "-loop", "0", GIF], check=True)
    size = os.path.getsize(GIF)
    log(f"{os.path.relpath(GIF, REPO)} is {size / 1024:.0f} KiB")
    if size > MAX_BYTES:
        raise RuntimeError(
            f"the GIF is {size / 1024 / 1024:.1f} MiB, over the "
            f"{MAX_BYTES / 1024 / 1024:.1f} MiB ceiling — shorten the take, drop "
            f"the frame rate, or raise DEMO_MAX_BYTES deliberately")


def main():
    for tool in ("docker", "node", "ffmpeg"):
        if not shutil.which(tool):
            sys.exit(f"{tool} is not on PATH — this recording needs docker, node "
                     f"and ffmpeg (brew install ffmpeg)")
    if not os.path.isdir(os.path.join(REPO, "portal", "node_modules",
                                      "@playwright", "test")):
        sys.exit("portal dev dependencies are missing — run `pnpm install`, then "
                 "`pnpm --filter fabric-emulator-portal exec playwright install "
                 "chromium`")

    shutil.rmtree(CAPTURE, ignore_errors=True)
    os.makedirs(CAPTURE, exist_ok=True)
    rolling = os.path.join(CAPTURE, ".rolling")
    stop = os.path.join(CAPTURE, ".stop")

    log("building the emulator from this tree and starting the full stack")
    # Two phases: `up --wait` chokes on om-migrate (a one-shot that exits 0 is
    # counted as failed on some compose versions), so wait on the long-running
    # services and let the catalog come up behind them.
    compose("up", "-d", "--build", "--wait", "--wait-timeout", "900",
            "entra-emulator", "keyvault-emulator", "fabric-emulator",
            "sail", "spark-agent", "sqlserver")
    compose("up", "-d", "--no-recreate", "openmetadata")
    portal = f"https://localhost:{FABRIC_PORT}"
    wait_for_portal(f"{portal}/_emulator/portal/workspaces")

    log("rolling: the graph is empty and the camera is on it")
    recorder = subprocess.Popen(
        ["node", os.path.join(DIR, "flow_scene.js")],
        env={**os.environ, "FLOW_OUT": CAPTURE, "FLOW_PORTAL": portal,
             "FLOW_WIDTH": WIDTH, "FLOW_HEIGHT": HEIGHT})
    end = time.time() + 90
    while not os.path.exists(rolling):
        if recorder.poll() is not None:
            raise RuntimeError(f"the recorder exited before it started rolling "
                               f"({recorder.returncode})")
        if time.time() > end:
            raise RuntimeError("the recorder never signalled that it was rolling")
        time.sleep(0.5)

    try:
        run_example()
    finally:
        open(stop, "w").close()

    if recorder.wait(timeout=180) != 0:
        raise RuntimeError(f"the recorder refused the take ({recorder.returncode})")

    to_gif(os.path.join(CAPTURE, "flow.webm"))
    log("PASS: docs/demo/flow.gif regenerated from this repository alone")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        for svc in ("fabric-emulator", "entra-emulator"):
            sys.stderr.write(f"\n==== {svc} log tail ====\n")
            compose("logs", "--tail", "40", svc, check=False)
        raise
    finally:
        compose("down", "-v", "--remove-orphans", check=False)
