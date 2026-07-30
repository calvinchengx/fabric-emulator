#!/usr/bin/env python3
"""e2e: Azure PowerShell drives the deployment-pipeline surface.

A second, independent client family for the same surface — the fabric-cli e2e
reaches it from Python/MSAL, this one from .NET/MSAL via Az.Accounts, using the
Connect-AzAccount + Get-AzAccessToken flow that Microsoft's own
DeploymentPipelines-DeployAll.ps1 authenticates with, then that script's exact
REST sequence.

Two measured constraints shape the compose file (see docs/23):
  * MSAL refuses a non-HTTPS authority outright, so the plain-HTTP workaround
    used for OpenMetadata's JVM is unavailable.
  * MSAL drops a non-443 port from the authority, so entra must answer on :443
    with no port in the URL — hence the network aliases.
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
