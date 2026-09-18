#!/usr/bin/env python3
"""e2e: MicrosoftPowerBIMgmt drives the Power BI admin and tenant surfaces.

Microsoft's own PowerShell module for Power BI, run against the emulator with
nothing reconfigured: its stock `Public` environment resolves to
https://api.powerbi.com and https://login.microsoftonline.com, and the compose
file makes the emulator answer at both.

WHY IT EARNS A WITNESS THE az-rest SUITE DOES NOT. `az rest` is a transport --
it sends the request the script wrote and returns the body, carrying no model
of Power BI. The cmdlets here deserialise into .NET types the module ships, so
a renamed or missing field fails inside Microsoft's code before any assertion
in the driver runs. Six claims on the admin and tenant surfaces had `ci:az-rest`
as their sole CI witness; this is a second, independently-modelled client for
them.

The compose constraints are the measured MSAL ones shared with
e2e/deployment-pipelines (docs/23): no non-HTTPS authority, and a non-443 port
is dropped from the authority -- hence the network aliases on :443.
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
