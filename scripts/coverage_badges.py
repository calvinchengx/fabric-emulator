#!/usr/bin/env python3
"""Emit shields.io endpoint JSON for the numbers this repo can honestly claim.

Self-hosted on purpose: no third-party coverage service, no upload token, no
account. CI computes the numbers, this writes them as shields `endpoint`
documents, and the docs site serves them from its own origin — so the badges
are as trustworthy as the site, and nothing leaves the project.

FOUR NUMBERS, because one would lie. Coverage percentages describe the unit
suites and nothing else; the work that actually catches consumer-facing
defects is the e2e fleet, which no statement counter can score. Publishing a
single "coverage" badge would quietly claim the opposite:

  go          statement coverage of the Go unit + in-process server tests
  python      statement coverage of the scoped Python unit suite
  witnesses   supported parity claims that name a test which exists
  e2e         how many end-to-end suites CI runs

`witnesses` is the integration measure. "67/67 claims witnessed" is a stronger
statement than any percentage: it says every claim of support is backed by
something that ran, which is precisely what a coverage number cannot say.

Usage:
    coverage_badges.py --out DIR [--go PCT] [--python PCT]

Percentages are supplied by the caller because only CI knows them — the Go
figure in particular is only comparable on a leg that had a real SQL Server.
Omit either and its badge is written as "n/a" rather than a wrong number.
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
WITNESSES = REPO / "docs" / "witnesses.json"
CI = REPO / ".github" / "workflows" / "ci.yml"

# Thresholds are deliberately not flattering. A repo enforcing a 90% Go floor
# should not paint 75% green.
SCALE = ((90, "brightgreen"), (80, "green"), (70, "yellowgreen"),
         (60, "yellow"), (40, "orange"))


def colour(pct: float | None) -> str:
    if pct is None:
        return "lightgrey"
    for floor, name in SCALE:
        if pct >= floor:
            return name
    return "red"


def badge(label: str, message: str, colour_name: str) -> dict:
    """A shields.io `endpoint` document.

    schemaVersion is shields' own contract and must be 1; the rest is ours.
    """
    return {"schemaVersion": 1, "label": label,
            "message": message, "color": colour_name}


def pct_badge(label: str, pct: float | None) -> dict:
    message = "n/a" if pct is None else f"{pct:.1f}%"
    return badge(label, message, colour(pct))


def witness_counts(path: Path | None = None) -> tuple[int, int]:
    """(claims that name a witness, total claims) from the witness map.

    `_gated` is not a claim — it records WHY a witness can skip — so it is
    excluded rather than inflating both halves of the ratio.
    """
    data = json.loads((path or WITNESSES).read_text(encoding="utf-8"))
    claims = {k: v for k, v in data.items() if k != "_gated"}
    witnessed = sum(1 for v in claims.values() if v.get("witnesses"))
    return witnessed, len(claims)


def witness_badge(path: Path | None = None) -> dict:
    witnessed, total = witness_counts(path)
    # Anything short of "all of them" is the interesting case, so it is not
    # green until it is complete.
    ok = total > 0 and witnessed == total
    return badge("parity claims witnessed", f"{witnessed}/{total}",
                 "brightgreen" if ok else "orange")


def e2e_suite_count(path: Path | None = None) -> int:
    """Top-level CI jobs, less the ones that are not end-to-end suites.

    Counted from the workflow rather than from a hand-maintained list, because
    a hand-maintained list is exactly the thing that goes stale and then
    overstates the fleet.
    """
    text = (path or CI).read_text(encoding="utf-8")
    jobs = re.findall(r"^  ([a-z0-9][a-z0-9-]*):$", text, re.M)
    not_e2e = {"test", "witnesses", "example-parity", "engine-matrix", "portal"}
    return len([j for j in jobs if j not in not_e2e])


def e2e_badge(path: Path | None = None) -> dict:
    return badge("e2e suites", str(e2e_suite_count(path)), "blue")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", required=True, type=Path,
                    help="directory to write the badge documents into")
    ap.add_argument("--go", type=float, default=None,
                    help="Go statement coverage percent (omit for n/a)")
    ap.add_argument("--python", dest="py", type=float, default=None,
                    help="Python statement coverage percent (omit for n/a)")
    args = ap.parse_args()

    args.out.mkdir(parents=True, exist_ok=True)
    documents = {
        "coverage-go.json": pct_badge("go coverage", args.go),
        "coverage-python.json": pct_badge("python coverage", args.py),
        "witnesses.json": witness_badge(),
        "e2e-suites.json": e2e_badge(),
    }
    for name, doc in documents.items():
        (args.out / name).write_text(json.dumps(doc) + "\n", encoding="utf-8")
        print(f"  {name}: {doc['label']} = {doc['message']} ({doc['color']})")
    print(f"wrote {len(documents)} badge documents to {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
