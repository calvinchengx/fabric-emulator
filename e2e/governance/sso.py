#!/usr/bin/env python3
"""e2e: OpenMetadata joins the family's trust chain.

With `sso-override.yml` layered on the governance profile, OM's authenticator
is entra-emulator: it validates bearer JWTs against entra's JWKS with the same
issuer fabric-emulator and azure-keyvault-emulator use. This witnesses that
headlessly — entra mints a *user* token (the forge, so the token carries the
email/preferred_username claims OM maps to a principal) and OM's API accepts
it, while a token from outside that trust chain is rejected.

The browser login flow (OIDC_* confidential client) rides the same trust edge
but needs a real browser, so it is deliberately not asserted here.
"""
import json
import os
import subprocess
import sys
import time

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))

FABRIC_PORT = os.environ.get("GOV_FABRIC_PORT", "9443")
ENTRA_PORT = os.environ.get("GOV_ENTRA_PORT", "8443")
OM_PORT = os.environ.get("GOV_OM_PORT", "8585")
TENANT = "11111111-1111-1111-1111-111111111111"
CLIENT_ID = "cccccccc-0000-0000-0000-000000000002"
# entra-emulator's seeded user (alice@entraemulator.dev) — a *user* token is
# required: client-credentials tokens carry no email/preferred_username, and
# that is what OM maps a principal from.
ALICE_OID = "aaaaaaaa-0000-0000-0000-000000000001"
ALICE_UPN = "alice@entraemulator.dev"

COMPOSE = ["docker", "compose", "-p", "fabricgov-sso",
           "-f", os.path.join(REPO, "docker-compose.yml"),
           "-f", os.path.join(DIR, "build-override.yml"),
           "-f", os.path.join(DIR, "sso-override.yml"),
           "--profile", "governance"]


def log(msg):
    print(f"==> {msg}", flush=True)


def compose(*args, check=True):
    return subprocess.run(COMPOSE + list(args), check=check,
                          env={**os.environ, "GOV_BUILD_CONTEXT": REPO})


def main():
    import requests
    import urllib3
    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

    log("starting family + OM backing stores (SSO overlay)")
    compose("up", "-d", "--build", "--wait", "--wait-timeout", "600",
            "entra-emulator", "keyvault-emulator", "fabric-emulator",
            "om-postgresql", "om-elasticsearch")
    log("starting OpenMetadata with entra as its authenticator")
    compose("up", "-d", "--no-recreate", "openmetadata")

    # The SSO overlay runs the issuer over plain HTTP (see sso-override.yml).
    entra = f"http://localhost:{ENTRA_PORT}"
    om = f"http://localhost:{OM_PORT}"

    end = time.time() + 900
    while time.time() < end:
        try:
            if requests.get(f"{om}/api/v1/system/version", timeout=3).status_code == 200:
                break
        except requests.RequestException:
            pass
        time.sleep(5)
    else:
        raise RuntimeError("OpenMetadata never became healthy")

    def forge(**over):
        # A real Entra delegated token carries the user's identity claims; the
        # forge mints a minimal one, so add them explicitly — without them OM
        # falls through to the pairwise `sub` and looks up a user named after
        # an opaque identifier. `alice` is an OM admin principal (see the
        # overlay's AUTHORIZER_ADMIN_PRINCIPALS), created at OM boot.
        body = {"userId": ALICE_OID, "audience": CLIENT_ID,
                "extraClaims": {"preferred_username": ALICE_UPN,
                                "email": ALICE_UPN, "name": "Alice Example"}}
        body.update(over)
        r = requests.post(f"{entra}/admin/api/tokens", json=body,
                          verify=False, timeout=15)
        r.raise_for_status()
        return r.json()["token"]

    log("minting a user token from entra-emulator")
    token = forge()

    log("calling OpenMetadata's API with the entra token")
    resp = requests.get(f"{om}/api/v1/tables", params={"limit": 1},
                        headers={"Authorization": "Bearer " + token}, timeout=30)
    assert resp.status_code == 200, (
        f"OM rejected an entra-minted token: {resp.status_code} {resp.text[:400]}")

    # A token entra did not sign must be refused — the trust edge has to be
    # real, not "any bearer accepted". The forge mints this for us.
    bogus = forge(signature="invalid")
    resp = requests.get(f"{om}/api/v1/tables", params={"limit": 1},
                        headers={"Authorization": "Bearer " + bogus}, timeout=30)
    assert resp.status_code in (401, 403), \
        f"OM accepted a badly-signed token: {resp.status_code}"

    # …and so must an expired one.
    expired = forge(expiresInSeconds=-60)
    resp = requests.get(f"{om}/api/v1/tables", params={"limit": 1},
                        headers={"Authorization": "Bearer " + expired}, timeout=30)
    assert resp.status_code in (401, 403), \
        f"OM accepted an expired token: {resp.status_code}"

    log("PASS: OpenMetadata authenticates entra-minted tokens and rejects "
        "unsigned ones — the catalog is inside the family trust chain")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        for svc in ("openmetadata", "om-migrate"):
            sys.stderr.write(f"\n==== {svc} log tail ====\n")
            compose("logs", "--tail", "40", svc, check=False)
        raise
    finally:
        compose("down", "-v", "--remove-orphans", check=False)
