#!/usr/bin/env python3
"""A plan doc that summarises another plan doc must not outlive it.

`docs/24-parity-completion.md` is the map a maintainer reads to decide what to
work on. Its tier tables summarise the sub-plans — 37, 38, 39, 51 and the rest —
in one row each, and a row states a size: `S`, `M`, `XS-M remaining`. The
sub-plan is where the work is actually tracked, and the row is a copy of it.

Nothing checked that the copy still matched, and three rows had already gone
stale before this script existed. The file says so about one of them itself:
"which is its own small lesson about a planning doc nothing checks." The
`runMultiple` row advertised two wrong answers that had been fixed for months;
the `-tsql-strict` row listed a claim that had already converted; the runtime-
divergences row asked for `input_file_name()` in SQL and async pipeline jobs,
both of which `docs/37` marks DONE. Every one of them pointed a maintainer at
work that did not exist.

The invariant, then:

    a doc-24 row that links a sub-plan whose own tracking table shows NOTHING
    open must itself be marked done.

The direction matters. This does not assert that an open sub-plan implies an
open row -- a summary is allowed to be coarser than what it summarises, and a
row may legitimately stay struck while a deferred item sits under it. What it
refuses is the one asymmetry that misleads: the sub-plan is finished and the
summary still asks for work.

TRACKING TABLES. The check reads only the two conventions this repo already
uses, because a heuristic over prose would be the same kind of guess the rows
were:

  * `## Order of work` — a markdown table whose first cell is struck through
    (`~~4~~ ✅`) when the item is done. docs/37.
  * `## Checklist` — `### Phase N — title ✅` headings, the ✅ marking done.
    docs/39.

An item that says "Deferred" is not open: a decision recorded is not a backlog.

COVERAGE IS REPORTED, NOT ASSUMED. A sub-plan with neither section cannot be
read, and the script says so by name rather than passing in silence -- an
unreadable sub-plan and a finished one must never print the same line. The
allowlist below pins today's unreadable set: --strict fails when a sub-plan
joins it, so a new plan cannot quietly opt out, and fails when one leaves it
without the allowlist being updated, so coverage cannot be won and then
forgotten.

Usage:
    check_plan_freshness.py [--strict]

    --strict  the set of sub-plans with no tracking table must equal
              UNTRACKED_SUBPLANS exactly. Armed in `make check` and CI.
"""
from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
DOCS = REPO / "docs"
SUMMARY = DOCS / "24-parity-completion.md"

# Sub-plans doc 24 links that carry no `## Order of work` / `## Checklist`
# section. Each is a narrative plan whose completion lives in its prose, so the
# summary row for it is unchecked -- stated here so that is a known hole with a
# name rather than a silent pass. Shrinking this list is the point; growing it
# needs a deliberate edit.
UNTRACKED_SUBPLANS = frozenset({
    "25-rti-kusto.md",
    "33-pbix-tooling.md",
    "36-capacity-job-queueing.md",
    "38-framework-conformance.md",
    "40-rest-connector-plan.md",
    "41-salesforce-connector-plan.md",
    "51-eventstream-kafka.md",
    "52-msmdsrv-hosts.md",
})

# A markdown link to a sibling doc, e.g. [37-runtime-fidelity-gaps.md](37-...md)
DOC_LINK = re.compile(r"\]\((?P<target>\d\d-[a-z0-9-]+\.md)(?:#[^)]*)?\)")
# "### Phase 3a - per-namespace catalog ✅"
PHASE_HEADING = re.compile(r"^###\s+Phases?\s+.+$", re.MULTILINE)

DONE = "\u2705"  # the check mark doc 24 marks a closed row with
# What marks a doc-24 row as no longer asking for work. Deliberately only two
# tokens, and NOT the bare word "done": the stale runtime-divergences row this
# script was written for says "Environments and the Files mount ... are done"
# in the middle of asking for two things that were also finished, and a looser
# pattern read that as the row closing itself. `Landed` is the tier-2 status
# column's own word. A struck-through first cell is not enough either -- doc 24
# strikes the TITLE of rows it is still describing.
ROW_DONE = re.compile(rf"{DONE}|\*\*Landed\*\*")
DEFERRED = re.compile(r"\bdeferred\b|\bskipped\b", re.IGNORECASE)


def fail(msg: str) -> None:
    print(f"  FAIL  {msg}")


def table_rows(body: str) -> list[list[str]]:
    """Rows of a GitHub markdown table, header and separator dropped."""
    rows = []
    for line in body.splitlines():
        line = line.strip()
        if not line.startswith("|"):
            continue
        cells = [c.strip() for c in line.strip("|").split("|")]
        if all(set(c) <= set("-: ") for c in cells):
            continue
        rows.append(cells)
    return rows[1:] if rows else []


def section(text: str, heading: str) -> str | None:
    """The body under `## heading`, up to the next `## `."""
    match = re.search(rf"^##\s+{re.escape(heading)}\s*$", text, re.MULTILINE)
    if not match:
        return None
    rest = text[match.end():]
    nxt = re.search(r"^##\s+", rest, re.MULTILINE)
    return rest[:nxt.start()] if nxt else rest


def open_items(text: str) -> tuple[list[str], str] | None:
    """Items still open in a sub-plan, and which convention was read.

    None when the doc carries neither tracking section -- unreadable, which is
    a different answer from "nothing open" and must not be confused with it.
    """
    order = section(text, "Order of work")
    if order is not None:
        rows = table_rows(order)
        if rows:
            openv = [" | ".join(r) for r in rows
                     if "~~" not in r[0] and not DEFERRED.search(" ".join(r))]
            return openv, "Order of work"

    checklist = section(text, "Checklist")
    if checklist is not None:
        phases = PHASE_HEADING.findall(checklist)
        if phases:
            openv = [p.strip() for p in phases
                     if DONE not in p and not DEFERRED.search(p)]
            return openv, "Checklist"

    return None


def summary_rows() -> list[list[str]]:
    """Every table row in doc 24, from every tier table."""
    rows = []
    for line in SUMMARY.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line.startswith("|"):
            continue
        cells = [c.strip() for c in line.strip("|").split("|")]
        if all(set(c) <= set("-: ") for c in cells):
            continue
        rows.append(cells)
    return rows


def check() -> tuple[list[str], set[str], int]:
    errors: list[str] = []
    untracked: set[str] = set()
    checked = 0

    for cells in summary_rows():
        row = " | ".join(cells)
        targets = {m.group("target") for m in DOC_LINK.finditer(row)}
        for target in sorted(targets):
            path = DOCS / target
            if not path.exists():
                errors.append(f"{SUMMARY.name} links {target}, which does not "
                              f"exist (check_docs_links.py owns the general "
                              f"case; this row cannot be checked at all)")
                continue
            read = open_items(path.read_text(encoding="utf-8"))
            if read is None:
                untracked.add(target)
                continue
            checked += 1
            still_open, convention = read
            if still_open:
                continue
            if not ROW_DONE.search(row):
                errors.append(
                    f"{target}: its '{convention}' has no open item left, but "
                    f"the {SUMMARY.name} row summarising it still asks for "
                    f"work and carries no {DONE} or **Landed** marker. The row "
                    f"points a maintainer at work that is finished. Row: "
                    f"{row[:220]}")

    return errors, untracked, checked


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--strict", action="store_true",
                    help="the untracked sub-plan set must equal the allowlist")
    args = ap.parse_args()

    if not SUMMARY.exists():
        print(f"  FAIL  {SUMMARY.name} is missing")
        return 1

    errors, untracked, checked = check()
    print(f"plan freshness: {checked} sub-plan link(s) read against their own "
          f"tracking table, {len(untracked)} with none to read")

    for name in sorted(untracked):
        print(f"  NOTE  {name}: no 'Order of work' or 'Checklist' section — "
              f"its {SUMMARY.name} row is NOT checked")

    if args.strict:
        for name in sorted(untracked - UNTRACKED_SUBPLANS):
            errors.append(f"{name} has no tracking table and is not in "
                          f"UNTRACKED_SUBPLANS. A new sub-plan may not opt out "
                          f"of this check silently: give it an 'Order of work' "
                          f"table, or add it to the allowlist deliberately")
        for name in sorted(UNTRACKED_SUBPLANS - untracked):
            errors.append(f"{name} is in UNTRACKED_SUBPLANS but is now readable "
                          f"or no longer linked from {SUMMARY.name}. Drop it "
                          f"from the allowlist so the coverage it won is kept")

    for e in errors:
        fail(e)
    if errors:
        print(f"\n{len(errors)} problem(s)")
        return 1
    print("  ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
