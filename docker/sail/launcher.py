#!/usr/bin/env python3
"""Sail launcher: mint an entra Storage-audience token, then exec `sail`.

Sail reads Azure credentials from process env vars at startup (object_store's
MicrosoftAzureBuilder::from_env) — there is no per-session credential channel.
So the OAuth handshake happens HERE, before the server starts: a real
client-credentials grant against entra-emulator (or any Entra-shaped issuer),
exported as AZURE_STORAGE_TOKEN for object_store to use as the bearer.

Skipped when AZURE_STORAGE_TOKEN is already set or ENTRA_TOKEN_URL is unset,
so the image also works against real Azure (or with static test tokens).
"""
import json
import os
import ssl
import sys
import urllib.parse
import urllib.request

def mint():
    url = os.environ.get("ENTRA_TOKEN_URL", "")
    if not url or os.environ.get("AZURE_STORAGE_TOKEN"):
        return
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
        token = json.loads(r.read())["access_token"]
    os.environ["AZURE_STORAGE_TOKEN"] = token
    print(f"launcher: minted Storage token from {url}", flush=True)

mint()
os.execvp("sail", ["sail"] + sys.argv[1:])
