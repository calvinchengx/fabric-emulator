#!/usr/bin/env python3
"""e2e: Microsoft's Azure CLI (`az rest`) drives Fabric REST + Power BI admin
against this checkout. The existing fabric-cli job is fab; this is az — the
same packaged CLI entra already uses as a stranger, speaking the surfaces
fab's verbs do not cover (admin, activityevents, provisionIdentity, …).

Nothing in the CLI is patched. entra-emulator IS login.microsoftonline.com
and fabric IS api.fabric.microsoft.com, the same alias trick the fab and
Az.Accounts jobs use, because MSAL drops a non-443 port from the authority.
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))


def compose(*a):
    return subprocess.run(["docker", "compose", *a], cwd=DIR).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "client")
    if rc != 0:
        for svc in ("client", "fabric", "entra"):
            sys.stderr.write(f"\n==== {svc} logs ====\n")
            subprocess.run(["docker", "compose", "logs", "--tail", "60", svc], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
