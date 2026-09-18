#!/usr/bin/env python3
"""e2e: Microsoft's Fabric CLI (fab) drives the emulator's control plane end to
end — auth (SPN via entra-emulator/MSAL), workspace + item CRUD, ls, get, the
raw api passthrough, and the full deployment-pipeline promotion flow. The
highest-authority borrowed oracle: Fabric's own tool. Linux-friendly (fab is
pure Python; the JVM-free stack is light).

The deployment-pipeline leg follows the call order of Microsoft's own
DeploymentPipelines-DeployAll.ps1 (fabric-samples): list pipelines, list
stages, POST deploy, poll the operation, read the result. fab 1.6.1 ships no
deployment-pipeline verbs (nothing in the package mentions them), so those
calls go through `fab api` — still fab's auth and HTTP stack, just untyped."""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
def compose(*a): return subprocess.run(["docker", "compose", *a], cwd=DIR).returncode
# The recorder APPENDS, and the recording is a bind mount that outlives the
# stack. Without this a second local run validates the first run's traffic as
# well as its own -- harmless in CI, where the checkout is fresh, and quietly
# confusing on a laptop.
recording = os.path.join(DIR, "recording", "responses.jsonl")
if os.path.exists(recording):
    os.remove(recording)

try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "client")
    if rc != 0:
        for svc in ("client", "fabric", "entra"):
            sys.stderr.write(f"\n==== {svc} logs ====\n")
            subprocess.run(["docker", "compose", "logs", "--tail", "50", svc], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
