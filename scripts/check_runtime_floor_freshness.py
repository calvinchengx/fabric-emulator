#!/usr/bin/env python3
"""Contract 3's runtime floor is hand-maintained. Say so, and make it expire.

WHY THIS EXISTS. Contract 3 asserts the image declares a Fabric Runtime and
MEETS its floor, comparing numerically -- `3.11.15` satisfies a `3.11` floor.
The comparison is MONOTONE, which is correct and is also the problem: an
upgrade keeps passing, so **nothing about this contract will ever go red on
its own**. If Microsoft raises the floor, `fabric-runtimes.json` keeps the old
number, the emulator keeps advertising a version that is no longer current,
and the probe keeps approving.

That is the opposite failure direction from the one that made this family look:
contract 6 read an engine's capability GAIN as a defect and was noisy within
hours (G59). A stale floor is silent, and silence is what a conformance suite
is worst at noticing about itself.

THERE IS NO ORACLE TO VENDOR. Contracts 2 and 8 hold their premises against
machine-readable Microsoft artefacts -- the `dummy-notebookutils` wheel, the
`StorageErrorCode` enum -- both pinned under `third_party/`. Fabric's runtime
versions are published as documentation pages and nothing else, so the
transcription cannot be corroborated the same way. Pretending otherwise would
be worse than admitting it.

SO THE CLAIM IS BOUNDED IN TIME INSTEAD. Each runtime entry already carries the
page it came from and the date somebody read it. This asserts that both are
present and that the reading is not older than MAX_AGE_DAYS -- the same move as
`check_cron_workflow_freshness.py`, which asks the cheaper checkable question
("has anyone run THIS version?") rather than the unanswerable one.

This check WILL eventually fail without anybody changing the code. That is the
design: re-reading a page twice a year is the work, and a green that never
expires is exactly what a single-sourced number does not deserve.

Usage:
    check_runtime_floor_freshness.py     exit non-zero if provenance is missing or stale
"""
import datetime as dt
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
RUNTIMES = ROOT / "e2e" / "conformance" / "fabric-runtimes.json"

# Six months. Fabric runtimes move on the order of a year, so this asks for a
# re-read twice per runtime generation -- often enough that a raised floor is
# caught within a release cycle, rarely enough that it is not noise.
MAX_AGE_DAYS = 183

ISO = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def entries(text: str) -> dict:
    return json.loads(text)["runtimes"]


def problems(runtimes: dict, today: dt.date) -> list[str]:
    out = []
    if not runtimes:
        return ["fabric-runtimes.json declares no runtimes, so contract 3 "
                "grades against nothing"]
    for name in sorted(runtimes):
        entry = runtimes[name]
        source = entry.get("source", "")
        read = entry.get("read", "")
        if not source:
            out.append(f"runtime {name}: no `source` — a hand-maintained number "
                       "with no citation cannot be re-read by anyone else")
        if not ISO.match(str(read)):
            out.append(f"runtime {name}: `read` is {read!r}, not an ISO date — "
                       "without it there is no way to tell a current "
                       "transcription from an old one")
            continue
        age = (today - dt.date.fromisoformat(read)).days
        if age > MAX_AGE_DAYS:
            out.append(
                f"runtime {name}: last read {read} ({age} days ago, limit "
                f"{MAX_AGE_DAYS}). Re-read {source}, confirm `python` is still "
                f"{entry.get('python', '?')}, and update `read`. If the floor "
                "moved, contract 3 has been passing against a stale one")
    return out


def main() -> int:
    if not RUNTIMES.is_file():
        print(f"check_runtime_floor_freshness: {RUNTIMES} is missing",
              file=sys.stderr)
        return 1
    runtimes = entries(RUNTIMES.read_text(encoding="utf-8"))
    found = problems(runtimes, dt.date.today())
    if found:
        print("check_runtime_floor_freshness:\n  " + "\n  ".join(found),
              file=sys.stderr)
        return 1
    for name in sorted(runtimes):
        e = runtimes[name]
        print(f"  runtime {name}: python {e.get('python')} floor, read "
              f"{e.get('read')} — within {MAX_AGE_DAYS} days")
    print(f"check_runtime_floor_freshness: {len(runtimes)} runtime(s), each "
          "citing a page and a reading date")
    return 0


if __name__ == "__main__":
    sys.exit(main())
