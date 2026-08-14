#!/usr/bin/env python3
"""e2e: Microsoft's terraform-provider-fabric drives Fabric REST against
this checkout. The az-rest job is `az`; this is Terraform — the same
packaged Microsoft client a developer runs, speaking workspaces, folders,
lakehouses, capacities, and workspace RBAC.

Nothing in Terraform or the provider is patched. entra-emulator IS
login.microsoftonline.com and fabric IS api.fabric.microsoft.com, the
same alias trick the fab and az-rest jobs use, because the Go Azure SDK
drops a non-443 port from the authority.
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
            subprocess.run(["docker", "compose", "logs", "--tail", "80", svc], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
