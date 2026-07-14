#!/usr/bin/env python3
"""e2e: Microsoft's Fabric CLI (fab) drives the emulator's control plane end to
end — auth (SPN via entra-emulator/MSAL), workspace + item CRUD, ls, get, the
raw api passthrough. The highest-authority borrowed oracle: Fabric's own tool.
Linux-friendly (fab is pure Python; the JVM-free stack is light)."""
import os, subprocess, sys
DIR = os.path.dirname(os.path.abspath(__file__))
def compose(*a): return subprocess.run(["docker", "compose", *a], cwd=DIR).returncode
try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "client")
    if rc != 0:
        for svc in ("client", "fabric", "entra"):
            sys.stderr.write(f"\n==== {svc} logs ====\n")
            subprocess.run(["docker", "compose", "logs", "--tail", "50", svc], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
