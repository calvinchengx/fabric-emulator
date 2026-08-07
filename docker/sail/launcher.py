#!/usr/bin/env python3
"""Sail launcher: mint an entra Storage-audience token, run `sail`, and KEEP the
token fresh for the server's whole life.

Sail reads Azure credentials from process env vars at startup (object_store's
MicrosoftAzureBuilder::from_env) — there is no per-session credential channel.
So the OAuth handshake happens HERE, before the server starts: a real
client-credentials grant against entra-emulator (or any Entra-shaped issuer),
exported as AZURE_STORAGE_TOKEN for object_store to use as the bearer.

WHY A SUPERVISOR, NOT AN EXEC. A token minted once expires (3600s by default),
after which every OneLake read fails with 401 Unauthorized until someone
restarts the container by hand — observed exactly that way an hour into a long
run, where it reads as a storage outage rather than an expired credential. And
because the token only enters sail through startup env, refreshing it REQUIRES
a process restart. So the launcher stays resident: it re-mints shortly before
expiry (the margin keeps a token valid across the restart window) and restarts
sail with the fresh token. Sail holds no state a restart loses that the
control plane does not re-establish — the Spark agent re-registers catalog
entries per session and reconnects on the next statement.

Skipped entirely (plain exec, original behaviour) when AZURE_STORAGE_TOKEN is
already set or ENTRA_TOKEN_URL is unset, so the image still works against real
Azure or with static test tokens, where rotation is someone else's job.
"""
import json
import os
import signal
import ssl
import subprocess
import sys
import time
import urllib.parse
import urllib.request

# Refresh this many seconds before the token expires. Large enough that a slow
# mint or restart never runs past expiry; small enough that most of each
# token's life is actually used.
REFRESH_MARGIN = int(os.environ.get("SAIL_TOKEN_REFRESH_MARGIN_SECONDS", "300"))

# How often to re-read the emulator's clock while waiting. Cheap (a local GET)
# and it only has to be faster than a person can notice a stalled pipeline.
CLOCK_POLL_SECONDS = int(os.environ.get("SAIL_CLOCK_POLL_SECONDS", "10"))


def emulator_base():
    """The emulator's origin, or None when it is not one.

    Derived from the storage endpoint the container already has rather than a
    new variable: OneLake IS the emulator, so anything configured to read
    OneLake already knows where to ask. An explicit override exists for
    deployments where those differ.
    """
    explicit = os.environ.get("FABRIC_EMULATOR_URL")
    if explicit:
        return explicit.rstrip("/")
    endpoint = os.environ.get("AZURE_STORAGE_ENDPOINT", "")
    parts = urllib.parse.urlsplit(endpoint)
    if not parts.scheme or not parts.netloc:
        return None
    return f"{parts.scheme}://{parts.netloc}"


def clock_offset():
    """Seconds the emulator's clock runs AHEAD of real time; 0 if unknown.

    WHY THE LAUNCHER CARES ABOUT SOMEBODY ELSE'S CLOCK. The token's expiry is
    set by the issuer in real time, but it is *judged* by the emulator, and the
    emulator's clock can be advanced — that is a feature, and it is the only
    way to test a schedule without waiting for a real occurrence. Once it is
    advanced, a token the issuer considers fresh is read as expired, and every
    OneLake call 401s from inside a notebook cell with nothing to point at.

    Unreachable, unconfigured or unparseable all mean 0: this must never be the
    reason the launcher fails to start sail.
    """
    base = emulator_base()
    if not base:
        return 0
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    try:
        req = urllib.request.Request(f"{base}/_emulator/clock")
        with urllib.request.urlopen(req, timeout=5, context=ctx) as r:
            return max(0, int(json.loads(r.read()).get("offset", 0)))
    except Exception:  # noqa: BLE001 - a clock we cannot read is a clock at zero
        return 0


def refresh_deadline(minted_at, expires_in, offset, margin=REFRESH_MARGIN):
    """Wall-clock instant to re-mint by, given how far the judge's clock leads.

    The emulator marks the token dead at `minted_at + expires_in - offset` in
    real time, so the whole schedule shifts earlier by the offset. Floored a
    minute out so a large advance cannot spin this into a restart loop.
    """
    return minted_at + max(60, expires_in - margin - max(0, offset))


def hopeless(expires_in, offset):
    """True when no mintable token can satisfy the emulator.

    The line is the token's LIFETIME, not lifetime-minus-margin. Between those
    two a refresh still buys real time — a 3600s token under a 1200s advance is
    accepted for 2400s, which is most of a working session — and warning there
    would cry wolf on the ordinary case this fix exists to handle. Past the
    lifetime the fresh token is born expired and restarting sail only costs a
    session.
    """
    return max(0, offset) >= expires_in


def mint():
    """Return (token, expires_in_seconds) from the configured issuer."""
    url = os.environ["ENTRA_TOKEN_URL"]
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": os.environ["ENTRA_CLIENT_ID"],
        "client_secret": os.environ["ENTRA_CLIENT_SECRET"],
        "scope": os.environ.get("ENTRA_SCOPE", "https://storage.azure.com/.default"),
    }).encode()
    # entra-emulator serves self-signed TLS on the compose network; this is a
    # local dev tool, so skip verification for the mint (mirrors
    # FABRIC_ENTRA_TLS_INSECURE on the emulator side).
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    with urllib.request.urlopen(urllib.request.Request(url, data=form), timeout=30, context=ctx) as r:
        body = json.loads(r.read())
    return body["access_token"], int(body.get("expires_in", 3600))


def main():  # pragma: no cover - the supervisor loop; witnessed by e2e/sail
    if not os.environ.get("ENTRA_TOKEN_URL") or os.environ.get("AZURE_STORAGE_TOKEN"):
        os.execvp("sail", ["sail"] + sys.argv[1:])

    child = None

    def forward(signum, _frame):
        if child and child.poll() is None:
            child.send_signal(signum)

    signal.signal(signal.SIGTERM, forward)
    signal.signal(signal.SIGINT, forward)

    while True:
        # Wall clock, NOT time.monotonic(): the token's expiry is a wall-clock
        # fact set by the issuer, and inside a VM (docker on macOS) the
        # monotonic clock pauses and warps against it — observed as a launcher
        # that sat through a refresh deadline while the token expired under it.
        minted_at = time.time()
        token, expires_in = mint()
        os.environ["AZURE_STORAGE_TOKEN"] = token
        offset = clock_offset()
        refresh_at = refresh_deadline(minted_at, expires_in, offset)
        verb = "restarting with fresh" if child else "minted"
        skew = f", emulator clock +{offset}s" if offset else ""
        print(f"launcher: {verb} Storage token from {os.environ['ENTRA_TOKEN_URL']} "
              f"(expires_in={expires_in}s{skew}, refresh in {int(refresh_at - time.time())}s)",
              flush=True)
        if hopeless(expires_in, offset):
            # No refresh can help: the fresh token is already born expired from
            # the emulator's point of view. Say so plainly rather than
            # restarting sail every minute while every call 401s anyway.
            print(f"launcher: WARNING the emulator's clock is {offset}s ahead, which "
                  f"outruns the {expires_in}s token lifetime — every OneLake call will "
                  f"401 until the clock is reset or the issuer follows it", flush=True)

        if child and child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=30)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()
        child = subprocess.Popen(["sail"] + sys.argv[1:], env=os.environ)

        # Wait for whichever comes first: the refresh deadline, or sail dying
        # on its own (in which case exit with ITS code so the container's
        # restart policy and healthcheck see the truth).
        #
        # THE DEADLINE IS RECOMPUTED, not fixed at mint time. A clock advance
        # that lands mid-wait moves the token's effective expiry earlier by the
        # amount of the jump, and a launcher holding a deadline it decided
        # minutes ago sleeps straight through it — which is exactly how a
        # scheduled notebook run 401s while the launcher reports a healthy
        # token with 40 minutes left on it.
        next_clock_check = 0.0
        while time.time() < refresh_at:
            code = child.poll()
            if code is not None:
                print(f"launcher: sail exited with {code}", flush=True)
                sys.exit(code)
            now = time.time()
            if now >= next_clock_check:
                next_clock_check = now + CLOCK_POLL_SECONDS
                moved = clock_offset()
                if moved != offset:
                    offset = moved
                    refresh_at = refresh_deadline(minted_at, expires_in, offset)
                    print(f"launcher: emulator clock moved to +{offset}s — token now "
                          f"effectively expires {int(refresh_at - now)}s from here",
                          flush=True)
            time.sleep(2)


if __name__ == "__main__":  # pragma: no cover
    main()
