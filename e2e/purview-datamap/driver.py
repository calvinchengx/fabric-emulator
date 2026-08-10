#!/usr/bin/env python3
"""Microsoft's Purview Data Map, driven by a REAL third-party Atlas client.

`pyapacheatlas` is the witness `docs/parity.md`'s Purview row names as the one
thing standing between it and 🟢: the row is 🟡 for exactly one stated reason,
that its evidence is 14 Go tests and this repo requires a real-client witness in
CI. This is that witness.

WHAT IT MAY AND MAY NOT TOUCH. The emulator implements the Data Map's TYPE
SYSTEM and ENTITIES only — 96 routes exist in the spec and glossary, lineage,
relationships, classifications, business metadata and search are NOT among the
implemented ones. This suite therefore drives only the first two, deliberately.
A witness that also drove the unimplemented routes would fail for an honest
reason while making the row look worse than the code is, and a witness expanded
until it goes green would not be evidence of anything.

WHY THE TOKEN IS THE INTERESTING PART. The Data Map is Atlas v2 on its OWN Entra
audience (`internal/server/server.go`: it "validates a purview.azure.net token,
not a Fabric control-plane one"). A Fabric token here produces a 401 that reads
like a routing or path error, which is the most likely way to misdiagnose this
suite. So the audience is asserted explicitly below rather than assumed.
"""

import json
import os
import ssl
import sys
import urllib.parse
import urllib.request

from pyapacheatlas.auth.base import AtlasAuthBase
from pyapacheatlas.core import AtlasClient, AtlasEntity
from pyapacheatlas.core.typedef import AtlasAttributeDef, EntityTypeDef

FABRIC = os.environ["FABRIC_BASE"]
ENTRA = os.environ["ENTRA_BASE"]
TENANT = os.environ.get("TENANT", "6f89cf12-978b-4d23-ac18-9ef0c127cf87")
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
# The spec's OAuth2 flow names this scope; the issued token's audience is the
# resource without the `.default` suffix, which is what the emulator validates.
SCOPE = "https://purview.azure.net/.default"

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE

failures = []


def check(ok, what, detail=""):
    print(f"  {'PASS' if ok else 'FAIL'}  {what}{(' — ' + detail) if detail else ''}",
          flush=True)
    if not ok:
        failures.append(what)


def entra_token():
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET,
        "scope": SCOPE,
    }).encode()
    req = urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form)
    with urllib.request.urlopen(req, context=_CTX) as r:
        return json.loads(r.read())["access_token"]


class EmulatorAuth(AtlasAuthBase):
    """Hands pyapacheatlas a bearer it did not have to negotiate.

    pyapacheatlas ships `ServicePrincipalAuthentication`, which talks to real
    Entra. Subclassing the base instead keeps the token path in ONE place we
    control, so a 401 is unambiguous about which audience was sent.
    """

    def __init__(self, token):
        self.token = token

    def get_authentication_headers(self, **kwargs):
        return {"Authorization": "Bearer " + self.token}


def main():
    token = entra_token()

    # Assert the audience BEFORE using it. A wrong-audience token fails at the
    # route with a 401 that reads like "the path is wrong", and that misreading
    # is the one this suite is most likely to produce.
    body = json.loads(
        __import__("base64").urlsafe_b64decode(
            token.split(".")[1] + "=" * (-len(token.split(".")[1]) % 4)))
    aud = body.get("aud", "")
    check(aud.rstrip("/") == "https://purview.azure.net",
          "the token carries the Data Map audience", f"aud={aud!r}")

    client = AtlasClient(
        endpoint_url=f"{FABRIC}/datamap/api",
        authentication=EmulatorAuth(token),
        requests_args={"verify": False},
    )

    # ---- type system -----------------------------------------------------
    tname = "e2e_witness_table"
    td = EntityTypeDef(
        name=tname,
        superTypes=["DataSet"],
        attributeDefs=[AtlasAttributeDef(name="rowCount", typeName="int").to_json()],
    )
    up = client.upload_typedefs(entityDefs=[td], force_update=True)
    check(isinstance(up, dict), "pyapacheatlas uploads a typedef", str(up)[:80])

    got = client.get_typedef(name=tname)
    check(got.get("name") == tname, "the typedef reads back by name")
    check(any(a["name"] == "rowCount" for a in got.get("attributeDefs", [])),
          "its declared attribute survives the round trip")

    # ---- entities --------------------------------------------------------
    qn = "emulator://witness/table/1"
    ent = AtlasEntity(name="witness_table", typeName=tname,
                      qualified_name=qn, guid="-1")
    res = client.upload_entities(batch=[ent])
    guids = list((res.get("guidAssignments") or {}).values())
    check(len(guids) == 1,
          "Atlas's negative-GUID protocol returns a real guid", str(guids))
    guid = guids[0] if guids else None

    if guid:
        e = client.get_single_entity(guid=guid)
        attrs = (e.get("entity") or {}).get("attributes", {})
        check(attrs.get("qualifiedName") == qn,
              "the entity reads back by guid with its qualifiedName")

        # createOrUpdate on the SAME qualifiedName must UPDATE, not duplicate —
        # the row claims qualifiedName is the per-type identity, so this is the
        # claim under test rather than a smoke check.
        again = client.upload_entities(
            batch=[AtlasEntity(name="witness_table_renamed", typeName=tname,
                               qualified_name=qn, guid="-1")])
        again_guids = set((again.get("guidAssignments") or {}).values())
        check(again_guids == {guid},
              "the same qualifiedName updates rather than duplicating",
              f"{again_guids} vs {guid}")

        # Deletes are SOFT per the spec's EntityStatus: the entity stays
        # readable with status DELETED. Asserting a 404 here would be asserting
        # the opposite of the documented behaviour.
        client.delete_entity(guid=guid)
        after = client.get_single_entity(guid=guid)
        status = (after.get("entity") or {}).get("status")
        check(status == "DELETED", "delete is soft, and status says so",
              f"status={status!r}")

    print()
    if failures:
        print(f"FAILED: {len(failures)} check(s): {', '.join(failures)}")
        return 1
    print("PASSED: pyapacheatlas drives the Data Map's type system and entities.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
