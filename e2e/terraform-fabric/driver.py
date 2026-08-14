#!/usr/bin/env python3
"""Microsoft's own terraform-provider-fabric, unmodified, driving Fabric REST
against this fabric-emulator.

entra-emulator is login.microsoftonline.com and fabric-emulator is
api.fabric.microsoft.com (compose aliases, :443) because the Go Azure SDK
drops a non-443 port from the authority — the same constraint az-rest and
fab measured.

The provider binary is whatever `terraform init` pulls from the registry
for the pin in main.tf. This harness does not rebuild or patch it.

    python3 /driver.py   # inside the hashicorp/terraform image
"""
from __future__ import annotations

import json
import os
import ssl
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import NoReturn
from uuid import UUID

CAPACITY = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
SYSTEM_CA = Path("/etc/ssl/certs/ca-certificates.crt")


def fail(msg: str) -> NoReturn:
    sys.exit(f"FAIL: {msg}")


def harvest_cert(host: str, timeout: float = 90) -> str:
    """The emulator's leaf, so Go's TLS stack verifies instead of turning it off."""
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        try:
            pem = ssl.get_server_certificate((host, 443))
            if "BEGIN CERTIFICATE" in pem:
                return pem
        except OSError as e:
            last = str(e)
        time.sleep(1)
    fail(f"{host}:443 never presented a certificate ({last})")


def trust_emulator_leaves() -> None:
    """System CAs stay: terraform init talks to the registry, and azidentity
    instance-discovers against the real login.microsoft.com. The emulator
    leaves are appended so Fabric REST and the token endpoint verify."""
    entra_pem = harvest_cert("login.microsoftonline.com")
    fabric_pem = harvest_cert("api.fabric.microsoft.com")
    if not SYSTEM_CA.is_file():
        fail(f"system CA bundle missing at {SYSTEM_CA}")
    ca = Path(tempfile.mkdtemp(prefix="tf-fabric-ca.")) / "ca.pem"
    ca.write_text(
        SYSTEM_CA.read_text(encoding="utf-8") + "\n" + entra_pem + "\n" + fabric_pem,
        encoding="utf-8",
    )
    os.environ["SSL_CERT_FILE"] = str(ca)
    os.environ["REQUESTS_CA_BUNDLE"] = str(ca)
    print(f"   CA bundle {ca} ({ca.stat().st_size} bytes)", flush=True)


def tf(*args: str, check: bool = True, capture: bool = False) -> subprocess.CompletedProcess:
    cmd = ["terraform", *args]
    print(f"    $ {' '.join(cmd)}", flush=True)
    r = subprocess.run(cmd, cwd="/work", text=True, capture_output=capture)
    if capture:
        if r.stdout:
            # output -json is parsed by the caller; other captured commands
            # still need to be visible when they fail.
            pass
        if check and r.returncode != 0:
            fail(f"{' '.join(cmd)}\n{(r.stderr or r.stdout)[:4000]}")
        return r
    if check and r.returncode != 0:
        fail(f"{' '.join(cmd)} exited {r.returncode}")
    return r


def require_uuid(name: str, value: object) -> str:
    text = str(value or "")
    try:
        UUID(text)
    except ValueError:
        fail(f"{name} is not a UUID: {value!r}")
    return text


def outputs() -> dict:
    r = tf("output", "-json", capture=True)
    blob = json.loads(r.stdout)
    return {k: v.get("value") for k, v in blob.items()}


def driver() -> None:
    print("-- 0. wait for TLS, trust the emulator leaves")
    trust_emulator_leaves()

    print("-- 1. terraform init (unmodified microsoft/fabric from the registry)")
    tf("init", "-input=false", "-no-color")

    print("-- 2. terraform apply")
    tf("apply", "-auto-approve", "-input=false", "-no-color")

    print("-- 3. assert outputs")
    out = outputs()
    wsid = require_uuid("workspace_id", out.get("workspace_id"))
    cap = require_uuid("capacity_id", out.get("capacity_id"))
    if cap != CAPACITY:
        fail(f"capacity_id = {cap}; want seeded {CAPACITY}")
    if out.get("capacity_state") != "Active":
        fail(f"capacity_state = {out.get('capacity_state')!r}; want Active")
    require_uuid("folder_id", out.get("folder_id"))
    require_uuid("lakehouse_id", out.get("lakehouse_id"))
    require_uuid("role_assignment_id", out.get("role_assignment_id"))
    print(f"   workspace {wsid} on capacity {cap}; folder + lakehouse + Viewer grant exist")

    print("-- 4. terraform destroy")
    tf("destroy", "-auto-approve", "-input=false", "-no-color")
    print("PASS terraform-fabric")


if __name__ == "__main__":
    driver()
