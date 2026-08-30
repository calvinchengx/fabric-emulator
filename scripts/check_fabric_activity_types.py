#!/usr/bin/env python3
"""`fabricActivityTypes` must equal the vendored Fabric table, exactly.

The dispatch is checked for completeness by
`TestEveryDocumentedFabricActivityTypeIsHandled`, which walks
`internal/api/fabricActivityTypes`. That test is only as good as the list, and
the list was TRANSCRIBED BY HAND from Microsoft's article — so a type Fabric
adds or renames stayed invisible until someone happened to re-read the docs.
That is the same defect the twelve fabricated successes came from, one level up:
a list nothing checks.

This closes it. The list is held to `third_party/fabric-activity-types/`, whose
provenance and refresh command live beside it. Three ways to fail, all of them
silent before:

  * a name in Microsoft's table that the Go list omits — the dispatch would
    never be checked for it, and it would fall to the success stub;
  * a name in the Go list that the table does not have — an invented type, or
    one Microsoft removed;
  * a vendored file whose bytes do not match the sha256 in PROVENANCE.md — the
    integrity check third_party/README.md requires of every vendored artifact.

Offline on purpose: it compares two files in the repo, so `make check` needs no
network. Upstream drift is caught by the separate, deliberate
`vendor_fabric_activity_types.py --check`.
"""
import hashlib
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
GO = ROOT / "internal" / "api" / "fabricactivitytypes.go"
VENDOR = ROOT / "third_party" / "fabric-activity-types"
ARTIFACT = VENDOR / "activity-types.json"
PROVENANCE = VENDOR / "PROVENANCE.md"

LIST = re.compile(r"var fabricActivityTypes = \[\]string\{(.*?)\n\}", re.S)
ENTRY = re.compile(r'"([^"]+)"')
SHA = re.compile(r"`sha256:([0-9a-f]{64})`")


def go_list(text: str) -> list[str]:
    m = LIST.search(text)
    if not m:
        raise SystemExit("fabricActivityTypes is not a []string literal any more — "
                         "this checker parses it, so its shape is part of the contract")
    return ENTRY.findall(m.group(1))


def main() -> int:
    for p in (GO, ARTIFACT, PROVENANCE):
        if not p.exists():
            print(f"check_fabric_activity_types: missing {p.relative_to(ROOT)}")
            return 1

    raw = ARTIFACT.read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    pinned = SHA.search(PROVENANCE.read_text(encoding="utf-8"))
    if not pinned:
        print("check_fabric_activity_types: PROVENANCE.md records no sha256 — "
              "third_party/README.md requires one for every vendored file")
        return 1
    if pinned.group(1) != digest:
        print("check_fabric_activity_types: the vendored table does not match its own hash.\n"
              f"  PROVENANCE.md: sha256:{pinned.group(1)}\n"
              f"  activity-types.json: sha256:{digest}\n"
              "Re-pin deliberately with scripts/vendor_fabric_activity_types.py, and READ the "
              "diff — a hash mismatch is either an upstream change or a bad fetch, and they "
              "are not the same event.")
        return 1

    have = go_list(GO.read_text(encoding="utf-8"))
    want = json.loads(raw)["types"]

    dupes = sorted({t for t in have if have.count(t) > 1})
    missing = sorted(set(want) - set(have))
    extra = sorted(set(have) - set(want))

    if missing or extra or dupes:
        print("check_fabric_activity_types: internal/api/fabricactivitytypes.go does not match "
              "the vendored table.\n")
        for t in missing:
            print(f"  MISSING from the Go list: {t} — {want[t]}")
            print("      the dispatch is never checked for it, so it falls to the success stub")
        for t in extra:
            print(f"  NOT in Microsoft's table: {t}")
            print("      invented, renamed upstream, or removed — check the article before deleting")
        for t in dupes:
            print(f"  duplicated in the Go list: {t}")
        print("\nRefresh the vendored table first (scripts/vendor_fabric_activity_types.py), "
              "then reconcile the Go list with it.")
        return 1

    print(f"check_fabric_activity_types: {len(have)} types, "
          f"internal/api/fabricactivitytypes.go matches the vendored table")
    return 0


if __name__ == "__main__":
    sys.exit(main())
