#!/usr/bin/env python3
"""Re-fetch and re-pin the vendored OpenMetadata column schema.

    scripts/vendor_openmetadata_schema.py <commit-sha> [--version 1.14.0]

third_party/README.md requires a PROVENANCE.md carrying a sha256 and byte size
for every vendored file. Maintaining that by hand is how a refresh ships with
stale hashes — the one field whose whole job is to be exact. This does the fetch
and rewrites the table, so bumping the pin is a command rather than a ritual.

The FILE LIST is derived, not hardcoded: it walks `$ref`s from the validated
node (`table.json#/definitions/column`) to a fixed point. If a future
OpenMetadata adds a reference to the column subtree, the refresh picks it up
instead of silently vendoring an unresolvable schema.
"""
import argparse
import hashlib
import json
import pathlib
import posixpath
import re
import sys
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
VENDOR = ROOT / "third_party" / "openmetadata-schema"
RAW = "https://raw.githubusercontent.com/open-metadata/OpenMetadata"
SPEC = "openmetadata-spec/src/main/resources/json/schema"
ENTRY = "entity/data/table.json"
NODE = "definitions/column"

REF = re.compile(r'"\$ref":\s*"([^"#][^"]*)"')


def fetch(url: str) -> bytes:
    with urllib.request.urlopen(url, timeout=30) as r:
        return r.read()


def closure(sha: str) -> dict[str, bytes]:
    """Every schema file reachable from the column node, fetched.

    The entry document is included whole — it is where `column` lives, and
    subsetting it would make the recorded sha256 describe a file that exists
    nowhere upstream.
    """
    entry = fetch(f"{RAW}/{sha}/{SPEC}/{ENTRY}")
    node = json.loads(entry)
    for part in NODE.split("/"):
        node = node[part]

    out = {ENTRY: entry}
    queue = [posixpath.normpath(posixpath.join(posixpath.dirname(ENTRY), r.split("#")[0]))
             for r in REF.findall(json.dumps(node))]
    while queue:
        rel = queue.pop()
        if rel in out:
            continue
        out[rel] = fetch(f"{RAW}/{sha}/{SPEC}/{rel}")
        queue += [posixpath.normpath(posixpath.join(posixpath.dirname(rel), r.split("#")[0]))
                  for r in REF.findall(out[rel].decode())]
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("sha", help="upstream commit SHA (never a branch or tag)")
    ap.add_argument("--version", help="the release this SHA is, e.g. 1.14.0")
    args = ap.parse_args()
    if len(args.sha) != 40 or not all(c in "0123456789abcdef" for c in args.sha):
        print("refusing: pass a full 40-character commit SHA, not a tag or branch")
        return 1

    files = closure(args.sha)
    files["LICENSE"] = fetch(f"{RAW}/{args.sha}/LICENSE")
    for rel, body in files.items():
        dest = VENDOR / ("schema/" + rel if rel != "LICENSE" else rel)
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(body)

    rows = []
    for p in sorted(VENDOR.rglob("*")):
        if p.is_file() and p.name != "PROVENANCE.md":
            b = p.read_bytes()
            rel = p.relative_to(VENDOR).as_posix()
            rows.append(f"| `{rel}` | {len(b)} | `{hashlib.sha256(b).hexdigest()}` |")

    print(f"fetched {len(files)} files at {args.sha[:8]}")
    print("\nReplace the Integrity table in PROVENANCE.md with:\n")
    print("| File | Bytes | sha256 |")
    print("|---|---|---|")
    print("\n".join(rows))
    print("\nAlso update: Pinned revision, Retrieved, and the version the "
          "docker-compose.yml pins must agree with.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
