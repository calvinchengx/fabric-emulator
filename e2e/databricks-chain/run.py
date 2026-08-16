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

        pat_path = os.path.join(DATA, "admin.pat")
        for _ in range(120):
            if os.path.isfile(pat_path) and os.path.getsize(pat_path) > 0:
                break
            time.sleep(1)
        else:
            sys.stderr.write(f"FAIL: databricks-emulator never wrote {pat_path}\n")
            subprocess.run(["docker", "compose", "logs", "--tail", "60", "databricks-emulator"],
                           cwd=DIR, env=ENV)
            return 1
        with open(pat_path, encoding="utf-8") as fh:
            pat = fh.read().strip()
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
