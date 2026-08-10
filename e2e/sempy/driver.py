"""Runs INSIDE the container. Drives real sempy and reports per-function outcome.

Separate from `e2e/xmla`'s probe because the client is different: that one is a
hand-written ADOMD.NET program, this one is Microsoft's `sempy`, the library
`semantic-link-labs` and every Fabric notebook actually use. The distinction is
load-bearing — `getDatabaseName` exists on the wire ONLY because a real client
resolves a dataset name it does not already know, and no hand-written probe
would ever issue it.
"""
# ruff: noqa: E402, I001
#   Import ORDER is load-bearing and must not be organised. The Fabric context
#   override and the credential injection have to run BEFORE `sempy.fabric` is
#   imported, or sempy resolves a real endpoint and a real credential at import
#   time. Sorting these into one block silently breaks the harness.
import base64
import json
import os
import time
import traceback

os.environ.setdefault("PYTHONNET_RUNTIME", "coreclr")

HOST = os.environ["SEMPY_HOST"]
WS = os.environ["SEMPY_WORKSPACE"]
DATASET = os.environ["SEMPY_DATASET"]
MARKER = os.environ["SEMPY_MARKER"]

from fabric.analytics.environment.context import (          # noqa: E402
    StaticFabricContext, fabric_default_context_override)

fabric_default_context_override.context_global = StaticFabricContext(
    pbi_shared_host=HOST, workspace_id=WS,
)

from azure.core.credentials import AccessToken             # noqa: E402
from fabric.analytics.environment.credentials import (      # noqa: E402
    SetFabricAnalyticsDefaultTokenCredentialsGlobally)


def _mint_jwt():
    """A DECODABLE token. Every segment must be valid base64url.

    Three separate failures hide behind one message here, and all three report
    `ArgumentException: The token has expired` for a token that never expired:

      1. an opaque string is not a JWT at all;
      2. `alg: none` makes PyJWT raise `DecodeError`;
      3. a SIGNATURE segment that is not valid base64url (a 21-char marker is
         `len % 4 == 1`, undecodable at any padding) raises
         `DecodeError: Invalid crypto padding`.

    sempy catches DecodeError into `exp = 0` (`get_token_expiry_raw_timestamp`),
    so a token that cannot be READ is indistinguishable from one that is stale.
    Nothing verifies the signature; it only has to decode.
    """
    def b64(d):
        return base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()

    exp = int(time.time()) + 3600
    head = b64({"alg": "HS256", "typ": "JWT"})
    body = b64({"aud": "https://analysis.windows.net/powerbi/api",
                "iss": "https://sts.windows.net/emulator/",
                "exp": exp, "nbf": int(time.time()) - 60,
                "upn": "e2e@emulator", "marker": MARKER})
    sig = base64.urlsafe_b64encode(b"emulator-not-verified").rstrip(b"=").decode()
    return f"{head}.{body}.{sig}", exp


TOKEN, TOKEN_EXP = _mint_jwt()


class FakeCredential:
    def get_token(self, *scopes, **kw):
        return AccessToken(TOKEN, TOKEN_EXP)


# `SetFabricAnalyticsDefaultTokenCredentials` is a CONTEXT MANAGER; calling it
# bare returns a generator that never runs and silently changes nothing. The
# `...Globally` variant assigns.
SetFabricAnalyticsDefaultTokenCredentialsGlobally(FakeCredential())

from sempy.fabric._credentials import get_access_token      # noqa: E402
from sempy.fabric._utils import get_token_seconds_remaining  # noqa: E402

# POSITIVE CONTROL. Asserting that the injection call did not throw is what
# reported success for a no-op earlier; assert the EFFECT instead — that sempy's
# own resolver hands back OUR token, decodable, with time left on it. Checking
# `MARKER in token` would be a FALSE NEGATIVE: the marker lives inside the
# base64 payload, so a literal substring search cannot see it.
_t = get_access_token().token
_payload = _t.split(".")[1]
_payload += "=" * (-len(_payload) % 4)
_mine = json.loads(base64.urlsafe_b64decode(_payload)).get("marker") == MARKER
print(f"###AUTH mine={_mine} seconds_left={get_token_seconds_remaining(_t)}", flush=True)

import sempy.fabric as fx                                   # noqa: E402

CASES = [
    ("list_workspaces",    lambda: fx.list_workspaces()),
    ("list_datasets",      lambda: fx.list_datasets(workspace=WS)),
    ("evaluate_dax",       lambda: fx.evaluate_dax(DATASET, "EVALUATE {1}", workspace=WS)),
    ("list_measures",      lambda: fx.list_measures(DATASET, workspace=WS)),
    ("list_tables",        lambda: fx.list_tables(DATASET, workspace=WS)),
    ("list_columns",       lambda: fx.list_columns(DATASET, workspace=WS)),
    ("list_relationships", lambda: fx.list_relationships(DATASET, workspace=WS)),
    ("list_partitions",    lambda: fx.list_partitions(DATASET, workspace=WS)),
]

for name, fn in CASES:
    try:
        r = fn()
        # CONTENT, not just a type. A DataFrame with zero rows reads as success
        # and proves nothing — the failure mode this whole suite exists to avoid.
        n = len(r) if hasattr(r, "__len__") else -1
        cells = ""
        try:
            if n:
                cells = " | ".join(str(v)[:24] for v in r.iloc[0].tolist()[:4])
        except Exception:
            cells = "(unreadable)"
        print(f"###RESULT {name} :: OK :: {type(r).__name__} :: rows={n}",
              flush=True)
        if cells:
            print(f"###ROW {name} :: {cells}", flush=True)
    except Exception as e:
        first = str(e).splitlines()[0][:150] if str(e).strip() else "(no message)"
        print(f"###RESULT {name} :: {type(e).__name__} :: {first}", flush=True)
        tb = traceback.extract_tb(e.__traceback__)
        for fr in tb[-2:]:
            print(f"###FRAME {name} :: {os.path.basename(fr.filename)}:{fr.lineno} "
                  f"{fr.name}", flush=True)
