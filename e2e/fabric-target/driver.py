"""Runs inside the uv environment: fabric_target's emulator profile drives the live
control plane end to end, and the toggle's guards behave."""
import os

import fabric_target
from fabric_target import FABRIC_SCOPE, STORAGE_SCOPE, Target, TargetError, target

t = target()
assert t.is_emulator, t.name

# The credential mints real entra tokens for multiple scopes (TokenCredential shape).
tok = t.credential.get_token(FABRIC_SCOPE)
assert tok.token.count(".") == 2 and tok.expires_on > 0
assert t.credential.get_token(STORAGE_SCOPE).token != tok.token

# Session: create a workspace through the authed control plane...
s = t.session()
r = s.post("/workspaces", json={"displayName": "target-e2e"})
assert r.status_code == 201, (r.status_code, r.text)

# ...resolve it BY NAME — the cross-target contract...
ws = t.workspace("target-e2e")
assert ws.id == r.json()["id"]

# ...create an item and ride the LRO helper to a terminal state.
r = s.post(f"/workspaces/{ws.id}/items",
           json={"displayName": "nb", "type": "Notebook",
                 "definition": {"parts": []}})
assert r.status_code in (201, 202), (r.status_code, r.text)
final = t.poll_lro(r)
assert final.status_code == 200 or final.status_code == 201, final.status_code
if r.status_code == 202:
    assert final.json().get("status") == "Succeeded", final.text

items = s.get(f"/workspaces/{ws.id}/items").json()["value"]
assert any(i["displayName"] == "nb" for i in items), items

# Guards: emulator_only passes here; DELETE is ungated locally.
t.emulator_only("clock control")
assert s.delete(f"/workspaces/{ws.id}/items/{items[0]['id']}").status_code in (200, 202, 204)

# Real-mode rules hold without any network: no workspace scope -> refused;
# workspace + no credential source -> refused with the az-login message.
os.environ.pop("FABRIC_WORKSPACE", None)
try:
    Target("real")
    raise SystemExit("real mode without FABRIC_WORKSPACE was accepted")
except TargetError as e:
    assert "FABRIC_WORKSPACE" in str(e)

os.environ["FABRIC_WORKSPACE"] = "x"
fabric_target._az_logged_in = lambda: False
try:
    Target("real")
    raise SystemExit("real mode without a credential source was accepted")
except TargetError as e:
    assert "az login" in str(e)

print("driver: all assertions passed")
