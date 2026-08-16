#!/usr/bin/env python3
"""e2e: the family chain — fabric → databricks-emulator → Spark agent → Sail.

TWO PHASES, and the reason is not style. databricks-emulator MINTS its admin
PAT on first start and writes it to its data dir; it does not take one from the
environment. fabric needs that PAT at startup, and both emulators are
distroless, so there is no shell to wait-and-exec with inside the compose. So:
bring up the databricks side, read the PAT it wrote to the bind-mounted data
dir, then bring up fabric and the client with it.
"""
import os
import shutil
import subprocess
import sys
import tempfile
import time

DIR = os.path.dirname(os.path.abspath(__file__))
DATA = tempfile.mkdtemp(prefix="dbx-chain-data.")
os.chmod(DATA, 0o777)
ENV = {**os.environ, "DBX_DATA_DIR": DATA, "DATABRICKS_PAT": "pending"}


def compose(*a, env=None):
    return subprocess.run(["docker", "compose", *a], cwd=DIR, env=env or ENV).returncode


def main() -> int:
    try:
        print(f"-- phase 1: databricks side (data dir {DATA})", flush=True)
        rc = compose("up", "-d", "--build", "--wait", "databricks-emulator")
        if rc != 0:
            return rc

        # Read the PAT through the DAEMON, not off the host filesystem. The
        # container writes it as root with secret permissions, and a real Linux
        # runner enforces that: reading the bind mount directly died with
        # `PermissionError: [Errno 13] admin.pat`. Docker Desktop's file sharing
        # masks the difference locally, which is why this passed on a laptop and
        # failed in CI. `docker compose cp` runs as root and writes the copy out
        # owned by the caller.
        out_path = os.path.join(DATA, "admin.pat.copy")
        pat = ""
        for _ in range(120):
            rc = subprocess.run(
                ["docker", "compose", "cp", "databricks-emulator:/data/admin.pat", out_path],
                cwd=DIR, env=ENV, capture_output=True).returncode
            if rc == 0 and os.path.isfile(out_path) and os.path.getsize(out_path) > 0:
                with open(out_path, encoding="utf-8") as fh:
                    pat = fh.read().strip()
                if pat:
                    break
            time.sleep(1)
        if not pat:
            sys.stderr.write("FAIL: databricks-emulator never wrote a readable admin.pat\n")
            subprocess.run(["docker", "compose", "logs", "--tail", "60", "databricks-emulator"],
                           cwd=DIR, env=ENV)
            return 1
        print(f"   admin PAT minted ({len(pat)} chars)", flush=True)

        print("-- phase 2: fabric + client, pointed at it", flush=True)
        env2 = {**ENV, "DATABRICKS_PAT": pat}
        rc = compose("up", "--build", "--abort-on-container-exit",
                     "--exit-code-from", "client", env=env2)
        if rc != 0:
            for svc in ("client", "fabric", "databricks-emulator", "spark-agent", "sail", "entra"):
                sys.stderr.write(f"\n==== {svc} logs ====\n")
                subprocess.run(["docker", "compose", "logs", "--tail", "40", svc],
                               cwd=DIR, env=env2)
        return rc
    finally:
        compose("down", "-v")
        shutil.rmtree(DATA, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
