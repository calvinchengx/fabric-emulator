#!/usr/bin/env python3
"""The governance stack, shared by the two witnesses that boot it.

`run.py` catalogs a Delta table; `sso.py` checks OpenMetadata's trust chain.
Both bring up the same family + OpenMetadata, and each carried its own copy of
*how* — which is exactly how one of them ended up without the pull retry.

OpenMetadata used to ship here straight from docker.getcollate.io, a vendor
registry backed by neither Docker Hub nor GHCR, so a reset connection or a TLS
handshake timeout mid-pull reds an otherwise-green run. `run.py` learned that
and grew a bounded retry; `sso.py` held the same twenty lines minus the retry,
and failed on precisely that pull. Two copies of a boot sequence is one copy
that will not get the next fix.

THERE IS A MIRROR NOW (G44) and the retry stays anyway. docker-compose.yml
pulls both images from ghcr.io/calvinchengx, which removes the single-vendor
dependency; it does not make GHCR incapable of a hiccup, and the retry costs
nothing on a healthy pull. What changed is that a failure here is now the
family's own registry rather than somebody else's.

Sibling module rather than a package: every e2e suite here is a directory of
scripts run as `python e2e/<suite>/run.py`, and this follows that.
"""
import os
import subprocess
import sys
import time

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))


def log(msg):
    print(f"==> {msg}", flush=True)


class Stack:
    """One `docker compose` project, with the retry the registry needs.

    The overlay list is given per-witness and must stay exactly what each one
    used before: compose hashes the configuration it is HANDED, so booting with
    a different `-f` list recreates containers that were already up.
    """

    def __init__(self, project, *overlays, profile="governance"):
        self.argv = ["docker", "compose", "-p", project,
                     "-f", os.path.join(REPO, "docker-compose.yml")]
        for overlay in overlays:
            self.argv += ["-f", os.path.join(DIR, overlay)]
        # One `--profile` per profile: the flag is repeatable, and a
        # comma-joined value is read as a single profile name that matches
        # nothing (COMPOSE_PROFILES, the env var, is the comma-separated one).
        self.argv += ["--profile", profile]

    def compose(self, *args, check=True):
        return subprocess.run(self.argv + list(args), check=check,
                              env={**os.environ, "GOV_BUILD_CONTEXT": REPO})

    def pulling(self, *args, attempts=3, base_delay=15):
        """Run a compose command whose failure mode is usually a registry hiccup.

        Seen in CI, both from docker.getcollate.io:
        `read tcp ...:443: read: connection reset by peer`, and a TLS handshake
        timeout.

        Retries are bounded and each one is logged, so a real outage still fails
        the suite rather than being silently absorbed or hanging.
        """
        for attempt in range(1, attempts + 1):
            result = self.compose(*args, check=False)
            if result.returncode == 0:
                return result
            if attempt == attempts:
                log(f"compose {args[0]} failed {attempts}x — giving up")
                raise subprocess.CalledProcessError(result.returncode,
                                                    self.argv + list(args))
            delay = base_delay * attempt
            log(f"compose {args[0]} exited {result.returncode}; "
                f"retrying in {delay}s ({attempt}/{attempts - 1}) — "
                f"likely a registry hiccup")
            time.sleep(delay)

    def wait_for_om(self, om, timeout=900):
        """Block until OpenMetadata answers, or say plainly that it never did.

        First boot runs a schema migration that takes minutes, so this is a poll
        and not a healthcheck wait: `up --wait` counts om-migrate — a one-shot
        that exits 0 — as failed on some compose versions.
        """
        import requests  # a governance-group dependency; not imported at module load

        end = time.time() + timeout
        while time.time() < end:
            try:
                if requests.get(f"{om}/api/v1/system/version",
                                timeout=3).status_code == 200:
                    return
            except requests.RequestException:
                pass
            time.sleep(5)
        raise RuntimeError(f"OpenMetadata never became healthy within {timeout}s")

    def dump_logs(self, *services):
        """Tail the containers that explain a failure, for the CI log."""
        for svc in services:
            sys.stderr.write(f"\n==== {svc} log tail ====\n")
            self.compose("logs", "--tail", "40", svc, check=False)

    def down(self):
        self.compose("down", "-v", "--remove-orphans", check=False)
