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


def main():
    if not os.environ.get("ENTRA_TOKEN_URL") or os.environ.get("AZURE_STORAGE_TOKEN"):
        os.execvp("sail", ["sail"] + sys.argv[1:])

    child = None

    def forward(signum, _frame):
        if child and child.poll() is None:
            child.send_signal(signum)

    signal.signal(signal.SIGTERM, forward)
    signal.signal(signal.SIGINT, forward)

    while True:
        token, expires_in = mint()
        os.environ["AZURE_STORAGE_TOKEN"] = token
        # Wall clock, NOT time.monotonic(): the token's expiry is a wall-clock
        # fact set by the issuer, and inside a VM (docker on macOS) the
        # monotonic clock pauses and warps against it — observed as a launcher
        # that sat through a refresh deadline while the token expired under it.
        refresh_at = time.time() + max(60, expires_in - REFRESH_MARGIN)
        verb = "restarting with fresh" if child else "minted"
        print(f"launcher: {verb} Storage token from {os.environ['ENTRA_TOKEN_URL']} "
              f"(expires_in={expires_in}s, refresh in {int(refresh_at - time.time())}s)",
              flush=True)

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
        while time.time() < refresh_at:
            code = child.poll()
            if code is not None:
                print(f"launcher: sail exited with {code}", flush=True)
                sys.exit(code)
            time.sleep(2)


if __name__ == "__main__":
    main()
