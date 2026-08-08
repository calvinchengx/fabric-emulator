#!/usr/bin/env python3
"""Capture the SHAPE of a real Fabric item definition — types and keys, no values.

Why this exists
---------------
Several emulator activities are blocked on one thing: nobody has captured the
wire `type` string and `typeProperties` shape that Fabric actually sends. The
repo's rule is that a wire name without a published schema or a captured
payload does not get written, so those activities refuse by name instead of
guessing. One real pipeline containing them unblocks the lot.

Why it prints a shape and not the definition
--------------------------------------------
THIS REPOSITORY IS PUBLIC, and so are its Actions logs and artifacts. A raw
pipeline definition from a real tenant carries workspace and item GUIDs,
connection and dataset display names, URLs, hostnames, file paths, SQL, and
parameter defaults — none of which the emulator needs, and all of which would
be published permanently by printing it here.

What the oracle actually needs is the STRUCTURE: which `type` discriminators
appear, and which property names hang off each. So every key path is kept,
every `type` value is kept (the discriminators ARE the capture), and every
other scalar is replaced by its JSON type. The output is safe to paste into an
issue; the raw definition should stay in the tenant.

To capture a raw definition anyway, do it on your own machine where the output
is yours — this script is not the tool for that, deliberately.

Usage (as the workflow runs it):
    AZURE_TENANT_ID=… AZURE_CLIENT_ID=… AZURE_CLIENT_SECRET=… \\
    FABRIC_TEST_WORKSPACE='my throwaway ws' \\
    capture_definition_shape.py --type dataPipelines --name 'capture-me'
"""

import argparse
import base64
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

FABRIC = "https://api.fabric.microsoft.com"

# The values worth keeping: they are the discriminators the emulator dispatches
# on, and they are Microsoft's vocabulary rather than the tenant's.
KEEP_VALUE_KEYS = {"type", "activityType", "kind"}


def call(method, url, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req) as resp:
            raw = resp.read()
            return resp.status, dict(resp.headers), json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read()
        # The BODY of an error is Fabric's own message and may name the item;
        # only the status is echoed, for the same reason values are redacted.
        return e.code, dict(e.headers), {"error": "<redacted: see the tenant>"} if raw else {}


def bearer():
    tenant = os.environ["AZURE_TENANT_ID"]
    form = urllib.parse.urlencode({
        "client_id": os.environ["AZURE_CLIENT_ID"],
        "client_secret": os.environ["AZURE_CLIENT_SECRET"],
        "scope": f"{FABRIC}/.default",
        "grant_type": "client_credentials",
    }).encode()
    req = urllib.request.Request(
        f"https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token", data=form, method="POST")
    with urllib.request.urlopen(req) as resp:
        return json.loads(resp.read())["access_token"]


def shape(node, key=None):
    """Recursively replace values with their types, keeping keys and the
    discriminator values named in KEEP_VALUE_KEYS."""
    if isinstance(node, dict):
        return {k: shape(v, k) for k, v in sorted(node.items())}
    if isinstance(node, list):
        if not node:
            return []
        # One element's shape plus the count: a 40-activity pipeline should
        # print 40 shapes, but a 40-element parameter array need not.
        first = shape(node[0], key)
        if all(shape(item, key) == first for item in node):
            return [first, f"<... {len(node)} items>"] if len(node) > 1 else [first]
        return [shape(item, key) for item in node]
    if key in KEEP_VALUE_KEYS and isinstance(node, str):
        return node
    if node is None:
        return "<null>"
    if isinstance(node, bool):
        return "<bool>"
    if isinstance(node, (int, float)):
        return "<number>"
    if isinstance(node, str):
        # An expression is structure, not content: `@pipeline().parameters.x`
        # tells the emulator an expression is legal here, and says nothing
        # about the tenant.
        return "<expression>" if node.startswith("@") else "<string>"
    return "<unknown>"


def discriminators(node, found):
    """Every `type` value anywhere in the tree — the list this capture is for."""
    if isinstance(node, dict):
        for k, v in node.items():
            if k in KEEP_VALUE_KEYS and isinstance(v, str):
                found.add(v)
            discriminators(v, found)
    elif isinstance(node, list):
        for item in node:
            discriminators(item, found)
    return found


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--type", default="dataPipelines",
                    help="typed collection segment, e.g. dataPipelines, notebooks, lakehouses")
    ap.add_argument("--name", required=True, help="display name of the item to capture")
    args = ap.parse_args()

    token = bearer()
    _, _, body = call("GET", f"{FABRIC}/v1/workspaces", token=token)
    wanted = os.environ["FABRIC_TEST_WORKSPACE"]
    matches = [w for w in body.get("value", []) if w["displayName"] == wanted]
    if len(matches) != 1:
        print(f"::error::expected exactly one workspace matching FABRIC_TEST_WORKSPACE, "
              f"found {len(matches)}")
        return 1
    wid = matches[0]["id"]

    status, _, body = call("GET", f"{FABRIC}/v1/workspaces/{wid}/{args.type}", token=token)
    if status >= 300:
        print(f"::error::listing {args.type} returned {status}")
        return 1
    items = [i for i in body.get("value", []) if i["displayName"] == args.name]
    if len(items) != 1:
        print(f"::error::expected exactly one {args.type} item named as requested, "
              f"found {len(items)}")
        return 1

    status, _, body = call(
        "POST", f"{FABRIC}/v1/workspaces/{wid}/items/{items[0]['id']}/getDefinition", token=token)
    if status >= 300:
        print(f"::error::getDefinition returned {status}")
        return 1

    parts = body.get("definition", {}).get("parts", [])
    found = set()
    print("=" * 70)
    print("SHAPE ONLY — every value is redacted except type discriminators.")
    print("Safe to paste into an issue. The raw definition stays in the tenant.")
    print("=" * 70)
    for part in parts:
        path = part.get("path", "<part>")
        try:
            decoded = json.loads(base64.b64decode(part["payload"]))
        except (ValueError, KeyError):
            # A non-JSON part (a notebook's .py, a binary) has no shape to
            # take, and printing it would print content. Named, not dumped.
            print(f"\n--- {path}: non-JSON part, skipped (content is not shape)")
            continue
        print(f"\n--- {path}")
        print(json.dumps(shape(decoded), indent=2))
        discriminators(decoded, found)

    print("\n" + "=" * 70)
    print("DISCRIMINATORS FOUND (this is the list the emulator needs):")
    for name in sorted(found):
        print(f"  {name}")
    print("=" * 70)
    return 0


if __name__ == "__main__":
    sys.exit(main())
