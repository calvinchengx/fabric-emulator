#!/usr/bin/env python3
"""In-container driver: provision a workspace + lakehouse in fabric-emulator,
mint a token from entra-emulator, template profiles.yml, then run Microsoft's
real dbt-fabricspark through debug -> seed -> run -> test against the emulator.

Everything here uses the *real* dbt-fabricspark adapter (installed in the image)
and the emulator's real Fabric REST + Livy surfaces — no mocks. The only test
hook is dbt-fabricspark's own ``authentication: int_tests`` mode, which takes a
pre-minted ``accessToken`` (so we skip the interactive/MSAL token dance while
still exercising the emulator's token validation, RBAC, Livy HC, and Spark
execution end to end).

Runs entirely on the compose network: entra-emulator (plain HTTP, for token
forging), fabric-emulator (TLS, Fabric REST + Livy), spark-agent (real Spark
behind the emulator's Livy layer).
"""
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

ENTRA = os.environ["ENTRA_URL"]  # http://entra-emulator:8443
FABRIC = os.environ["FABRIC_URL"]  # https://api.fabric.microsoft.com:9443
CA = os.environ["REQUESTS_CA_BUNDLE"]  # emulator cert, shared via volume
# entra-emulator's confidential daemon SP (client-credentials identity). The
# workspace it creates makes it Admin, so its token passes Livy RBAC.
DAEMON_CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
# One audience serves both surfaces: it is in the emulator's ControlPlaneAudiences
# (Fabric REST) and is exactly what dbt's scope
# `https://analysis.windows.net/powerbi/api/.default` resolves to (Livy).
AUDIENCE = "https://analysis.windows.net/powerbi/api"

PROJECT = "/project"


def log(msg):
    print(f"==> {msg}", flush=True)


def http(method, url, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    # verify against the emulator's own CA for HTTPS; plain HTTP for entra.
    import ssl

    ctx = ssl.create_default_context(cafile=CA) if url.startswith("https") else None
    try:
        with urllib.request.urlopen(req, context=ctx, timeout=30) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, {"error": e.read().decode(errors="replace")}


def main():
    # 1) Mint a control-plane token (audience valid for REST + Livy).
    log("minting token from entra-emulator")
    status, tok = http(
        "POST", f"{ENTRA}/admin/api/tokens",
        body={"clientId": DAEMON_CLIENT_ID, "audience": AUDIENCE})
    token = tok.get("accessToken") or tok.get("token")
    if status // 100 != 2 or not token:
        sys.exit(f"token forge failed: {status} {tok}")

    # 2) Create a workspace (creator becomes Admin) + a Lakehouse item.
    log("creating workspace + lakehouse")
    status, ws = http("POST", f"{FABRIC}/v1/workspaces", token,
                      {"displayName": "dbt_fabricspark_e2e"})
    if status // 100 != 2:
        sys.exit(f"workspace create failed: {status} {ws}")
    wsid = ws["id"]
    status, lh = http("POST", f"{FABRIC}/v1/workspaces/{wsid}/lakehouses", token,
                      {"displayName": "e2e_lh"})
    if status // 100 != 2:
        sys.exit(f"lakehouse create failed: {status} {lh}")
    lhid = lh["id"]
    log(f"workspace={wsid} lakehouse={lhid}")

    # 3) Template profiles.yml (dbt-fabricspark, Livy method, int_tests auth).
    profile = f"""dbt_fabricspark_e2e:
  target: dev
  outputs:
    dev:
      type: fabricspark
      method: livy
      livy_mode: fabric
      authentication: int_tests
      accessToken: "{token}"
      endpoint: "{FABRIC}/v1"
      workspaceid: "{wsid}"
      lakehouseid: "{lhid}"
      lakehouse: "e2e_lh"
      schema: "e2e_lh"
      threads: 1
      connect_retries: 3
      connect_timeout: 30
      spark_config:
        name: "dbt-fabricspark-e2e"
"""
    os.makedirs(PROJECT, exist_ok=True)
    with open(os.path.join(PROJECT, "profiles.yml"), "w") as f:
        f.write(profile)

    # 4) Drive the real dbt through the emulator. debug proves auth + Livy
    #    session + a statement on real Spark; seed/run/test build & verify models.
    env = {**os.environ, "DBT_PROFILES_DIR": PROJECT}
    stages = [
        ["dbt", "debug"],
        ["dbt", "seed", "--full-refresh"],
        ["dbt", "run"],
        ["dbt", "test"],
    ]
    for cmd in stages:
        log(" ".join(cmd))
        rc = subprocess.run(cmd, cwd=PROJECT, env=env).returncode
        if rc != 0:
            sys.exit(f"stage failed ({' '.join(cmd)}): exit {rc}")
    log("all dbt stages passed")


if __name__ == "__main__":
    main()
