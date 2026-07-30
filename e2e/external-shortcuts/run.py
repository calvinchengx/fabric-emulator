#!/usr/bin/env python3
import os
import subprocess

HERE = os.path.dirname(os.path.abspath(__file__))
cmd = ["docker", "compose", "-p", "fabric-external-shortcuts-e2e", "-f", os.path.join(HERE, "docker-compose.yml")]
try:
    subprocess.run(cmd + ["up", "--build", "--abort-on-container-exit", "--exit-code-from", "test"], check=True)
finally:
    subprocess.run(cmd + ["down", "-v", "--remove-orphans"], check=False)
