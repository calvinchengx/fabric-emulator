#!/usr/bin/env python3
"""e2e: the Flow view's terminal pane, end to end.

Boots the emulator with the terminal pane turned on THE DOCUMENTED WAY — the
`terminal` compose profile to start ttyd, plus docker-compose.terminal.yml to
hand the emulator its URL — and drives the pane in a real browser (pane.js).

WHY THE ROOT COMPOSE FILES rather than a private one, as most suites here use:
the pairing IS the thing under test. Two opt-ins that must agree, a service
behind a profile and an environment variable in an overlay, is exactly the shape
of a bug this repo has already shipped: the medallion compose set
FABRIC_SPARK_AGENT_URL unconditionally while `spark-agent` sat behind
`profiles: ["livy"]`, so the emulator drove notebooks at a container nobody
started. A suite with its own compose file could not catch that class of bug at
all — it would be testing a stack no user ever runs.

Requires: docker, node, and the portal's dev dependencies installed
(`pnpm install`) with a chromium (`pnpm --filter fabric-emulator-portal exec
playwright install chromium`).

    python3 e2e/portal-terminal/run.py

Ports are overridable for a machine where the defaults are taken:
    TERM_FABRIC_PORT=9543 TERM_ENTRA_PORT=8543 TERM_KV_PORT=8544 \\
        python3 e2e/portal-terminal/run.py
"""
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))

FABRIC_PORT = os.environ.get("TERM_FABRIC_PORT", "9443")
# Pinned in build-override.yml: the browser has to know it, and scraping a
# generated one out of container logs mid-run is a worse trade.
TOKEN = "e2e-terminal-token"

COMPOSE = ["docker", "compose", "-p", "fabricterm-e2e",
           "-f", os.path.join(REPO, "docker-compose.yml"),
           "-f", os.path.join(DIR, "build-override.yml"),
           # The overlay under test: it is what sets FABRIC_TERMINAL_URL, and
           # therefore what decides the routes exist at all.
           "-f", os.path.join(REPO, "docker-compose.terminal.yml"),
           # Repeatable flag, one profile per use — a comma-joined value is read
           # as a single profile name that matches nothing.
           "--profile", "terminal"]


def log(msg):
    print(f"==> {msg}", flush=True)


def compose(*args, check=True):
    return subprocess.run(COMPOSE + list(args), check=check,
                          env={**os.environ, "TERM_BUILD_CONTEXT": REPO})


def wait_for_portal(url, timeout=180):
    """Poll the portal itself, not /health: the pane is a portal feature."""
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


def main():
    node_modules = os.path.join(REPO, "portal", "node_modules", "@playwright", "test")
    if not os.path.isdir(node_modules):
        sys.exit("portal dev dependencies are missing — run `pnpm install` "
                 "(pane.js drives the portal's own Playwright, as "
                 "e2e/medallion-governance does)")

    log("building the emulator from this tree and starting it with ttyd")
    compose("up", "-d", "--build", "--wait", "--wait-timeout", "600",
            "entra-emulator", "fabric-emulator", "ttyd")

    portal = f"https://localhost:{FABRIC_PORT}"
    wait_for_portal(f"{portal}/_emulator/portal/workspaces")
    log("portal is serving; driving the pane in chromium")

    rc = subprocess.run(
        ["node", os.path.join(DIR, "pane.js")],
        env={**os.environ, "TERM_PORTAL_URL": portal, "TERM_TOKEN": TOKEN},
    ).returncode
    if rc != 0:
        raise RuntimeError(f"pane.js failed (exit {rc})")

    log("PASS: the pane reached ttyd through the portal's origin, attached, ran "
        "a command, refused a wrong token, and left plain portal routes working")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        for svc in ("fabric-emulator", "ttyd"):
            sys.stderr.write(f"\n==== {svc} log tail ====\n")
            compose("logs", "--tail", "60", svc, check=False)
        raise
    finally:
        compose("down", "-v", "--remove-orphans", check=False)
