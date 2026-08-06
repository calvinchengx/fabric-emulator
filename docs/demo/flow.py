#!/usr/bin/env python3
"""Regenerate docs/demo/flow.gif: a medallion drawing itself in the flow view.

    uv run --frozen --group demo python docs/demo/flow.py

The companion of demo.tape. That records a terminal with VHS; this records a
browser with Playwright and converts the take to a GIF. Both are the source of
truth for the image they produce, which flow.gif previously was not: it was a
hand-cropped excerpt of a run in the contoso-data-platform repository, so nobody
could regenerate the README's hero image from this one, and it had gone stale
enough to predate the terminal pane.

WHAT IT FILMS, and why it is all local. The graph is built from recorded
lineage, and lineage comes from movements the emulator can KNOW: here, two Copy
activities run by its own executor over real Delta directories written by
delta-rs. So the medallion on camera needs no Spark, no ODBC driver and no
catalog — two containers and this script. The chain is
bronze -> silver -> gold, which is the shape a reader recognises.

PACED ON PURPOSE. Each step pauses so the graph fills in visibly rather than
appearing in one frame. A GIF of a finished graph says nothing about watching
data move, which is the whole claim of the view.

Requires: docker, node, ffmpeg, and the portal's dev dependencies installed
(`pnpm install`) with a chromium
(`pnpm --filter fabric-emulator-portal exec playwright install chromium`).

Ports are overridable for a machine where the defaults are taken:
    DEMO_FABRIC_PORT=9643 DEMO_ENTRA_PORT=8643 DEMO_KV_PORT=8644 \\
        uv run --frozen --group demo python docs/demo/flow.py
"""
import base64
import json
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

FABRIC_PORT = os.environ.get("DEMO_FABRIC_PORT", "9443")
ENTRA_PORT = os.environ.get("DEMO_ENTRA_PORT", "8443")
TENANT = "11111111-1111-1111-1111-111111111111"

# The GIF's own dimensions: recorded at this size rather than cropped later.
WIDTH = os.environ.get("DEMO_WIDTH", "900")
HEIGHT = os.environ.get("DEMO_HEIGHT", "620")
# 8fps is enough for a graph that changes every second or two, and a third of
# the frames of the 25fps take. The old asset was 5fps.
FPS = os.environ.get("DEMO_FPS", "8")
# Played at 2x. The take runs ~40s because a Delta write and two pipeline runs
# take as long as they take; nobody watches a 40s README image. Speeding the
# playback keeps every frame of what happened rather than trimming a window out
# of the middle, and docs/demo/README.md says the GIF is sped up.
SPEED = os.environ.get("DEMO_SPEED", "2")
# A README image nobody waits for. The previous asset was 2.1 MB.
MAX_BYTES = int(os.environ.get("DEMO_MAX_BYTES", str(4 * 1024 * 1024)))

COMPOSE = ["docker", "compose", "-p", "fabricdemo-flow",
           "-f", os.path.join(REPO, "docker-compose.yml"),
           "-f", os.path.join(DIR, "flow-override.yml")]


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


def seed(fabric, entra, beat):
    """Build the medallion the recording films, one visible step at a time."""
    import pyarrow as pa
    import requests
    import urllib3
    from deltalake import write_deltalake

    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

    def token(scope):
        r = requests.post(
            f"{entra}/{TENANT}/oauth2/v2.0/token",
            data={"grant_type": "client_credentials",
                  "client_id": "cccccccc-0000-0000-0000-000000000002",
                  "client_secret": "daemon-app-secret", "scope": scope},
            verify=False, timeout=15)
        r.raise_for_status()
        return r.json()["access_token"]

    s = requests.Session()
    s.verify = False
    s.headers["Authorization"] = "Bearer " + token(
        "https://api.fabric.microsoft.com/.default")

    log("creating the workspace and its lakehouses")
    r = s.post(f"{fabric}/v1/workspaces", json={"displayName": "contoso"}, timeout=15)
    assert r.status_code == 201, (r.status_code, r.text)
    ws = r.json()["id"]
    for name in ("lake", "warehouse"):
        r = s.post(f"{fabric}/v1/workspaces/{ws}/lakehouses",
                   json={"displayName": name}, timeout=15)
        assert r.status_code in (201, 202), (r.status_code, r.text)
    items = s.get(f"{fabric}/v1/workspaces/{ws}/items",
                  params={"type": "Lakehouse"}, timeout=15).json()["value"]
    by_name = {i["displayName"]: i["id"] for i in items}
    beat(2)

    log("writing bronze_orders with delta-rs")
    storage = {
        "azure_storage_account_name": "onelake",
        "azure_storage_token": token("https://storage.azure.com/.default"),
        "azure_endpoint": f"{fabric}/onelake",
        "allow_invalid_certificates": "true",
    }
    orders = pa.table({
        "order_id": pa.array(list(range(1, 25)), pa.int64()),
        "region": [["us", "eu", "apac"][i % 3] for i in range(24)],
        "amount": pa.array([9.5 + i for i in range(24)], pa.float64()),
    })
    write_deltalake("az://contoso/lake.Lakehouse/Tables/bronze_orders", orders,
                    storage_options=storage)
    beat(3)

    def copy_step(name, src_item, src_path, dst_item, dst_path):
        """One Copy activity, created and run — an edge the emulator KNOWS."""
        spec = {"properties": {"activities": [{
            "name": name, "type": "Copy", "typeProperties": {
                "source": {"location": {"itemId": src_item, "path": src_path}},
                "sink": {"location": {"itemId": dst_item, "path": dst_path}},
            }}]}}
        payload = base64.b64encode(json.dumps(spec).encode()).decode()
        r = s.post(f"{fabric}/v1/workspaces/{ws}/items", json={
            "displayName": name, "type": "DataPipeline",
            "definition": {"parts": [{"path": "pipeline-content.json",
                                      "payloadType": "InlineBase64",
                                      "payload": payload}]}}, timeout=15)
        assert r.status_code == 202, (r.status_code, r.text)
        opid = r.headers["x-ms-operation-id"]
        for _ in range(100):
            op = s.get(f"{fabric}/v1/operations/{opid}", timeout=15).json()
            if op.get("status") == "Succeeded":
                pid = s.get(f"{fabric}/v1/operations/{opid}/result",
                            timeout=15).json()["id"]
                break
            time.sleep(0.1)
        else:
            raise RuntimeError(f"{name}: pipeline create did not complete")
        r = s.post(f"{fabric}/v1/workspaces/{ws}/items/{pid}/jobs/instances",
                   params={"jobType": "Pipeline"}, json={}, timeout=60)
        assert r.status_code == 202, (r.status_code, r.text)
        job = s.get(r.headers["Location"], timeout=15).json()
        assert job["status"] == "Completed", job

    log("Copy: bronze_orders -> silver_orders")
    copy_step("RefineOrders", by_name["lake"], "Tables/bronze_orders",
              by_name["lake"], "Tables/silver_orders")
    beat(3)

    log("Copy: silver_orders -> the warehouse's gold table")
    copy_step("BuildGold", by_name["lake"], "Tables/silver_orders",
              by_name["warehouse"], "Tables/gold_orders")
    beat(3)


def to_gif(webm):
    """Two-pass palette: one pass to choose 128 colours, one to map to them.

    A single-pass GIF of a dark UI bands badly in the chip colours, which are
    the part a reader is meant to read.
    """
    log(f"converting to a {WIDTH}px {FPS}fps GIF at {SPEED}x")
    # setpts BEFORE fps: rescaling the timestamps and then sampling gives an
    # even frame spacing. The other order samples first and then stretches, which
    # leaves the motion juddering.
    filters = (f"setpts=PTS/{SPEED},fps={FPS},scale={WIDTH}:-1:flags=lanczos,"
               f"split[a][b];[a]palettegen=max_colors=128[p];"
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

    log("building the emulator from this tree and starting it")
    compose("up", "-d", "--build", "--wait", "--wait-timeout", "600",
            "entra-emulator", "fabric-emulator")
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
        seed(portal, f"https://localhost:{ENTRA_PORT}", beat=time.sleep)
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
