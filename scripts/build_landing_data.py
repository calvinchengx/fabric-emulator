#!/usr/bin/env python3
"""Assemble the landing page's data, and refuse a page that hardcodes a number.

The landing page states six totals. Every one of them moves, and a number typed
into a page has no idea a row was added: this repo's own docs site carried
"113 supported capability claims" long after the figure was 120, and the README
still states a green-row count as a literal bound to nothing. The landing page
is the front door, so it gets the check the prose never had.

So the page reads its totals at run time from JSON copied beside it, and this
script FAILS when the page carries a literal where a placeholder belongs, or
stops reading one of the manifests. Copying the files is the easy half; the
check is the point.

NOTHING IS COUNTED TWICE. `website/scripts/sync-docs.mjs` already derives the
parity tally, the claim and witness counts and the e2e suite count while it
renders the docs, and publishes them as `website/src/data/site-stats.json`.
This script COPIES that file rather than re-deriving it, because two
derivations of one number on one published site is how the two halves come to
disagree in public. What it derives itself is only what nothing else counts:
the two generated matrices, and how many claims rest on a real-tenant witness.

Run it after the Astro build, which is what writes site-stats.json:

    pnpm --filter fabric-emulator-docs build
    ./scripts/build_landing_data.py --out _site --landing site/index.html
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent

WITNESSES = ROOT / "docs" / "witnesses.json"
ENGINE_MATRIX = ROOT / "docs" / "engine-matrix.md"
CONFORMANCE = ROOT / "docs" / "conformance-matrix.md"
SITE_STATS = ROOT / "website" / "src" / "data" / "site-stats.json"

# Each is (an id the page fills, the file it reads to fill it). Several ids may
# share a file. A page that stops reading one of these shows a dash forever,
# which is worse than a wrong number because nothing looks broken.
BINDINGS = {
    "witness-count": "site-stats.json",
    "claims-count": "site-stats.json",
    "parity-real": "site-stats.json",
    "e2e-suites": "site-stats.json",
    "engine-pass": "evidence-summary.json",
    "conformance-proven": "evidence-summary.json",
    "verified-count": "evidence-summary.json",
}

# The stat row is delimited in the page by these comments, so this script reads
# exactly the region where a total is stated as a headline rather than guessing
# at the shape of the markup around it.
STATS_REGION = re.compile(r"<!-- stats:start -->(.*?)<!-- stats:end -->", re.S)
ID_ELEMENT = re.compile(r"<(b|span)\b[^>]*\bid=\"([^\"]+)\"[^>]*>(.*?)</\1>", re.S)
BOLD_ELEMENT = re.compile(r"<b\b[^>]*>(.*?)</b>", re.S)
PLACEHOLDER = "&mdash;"


# A manifest is "read" only when the page FETCHES it. Matching the bare
# filename anywhere in the source is not enough: the first version of this
# check passed a mutation that renamed the fetch, because the old name was
# still sitting in the comment above it explaining what the fetch was for.
def fetches(text: str, name: str) -> bool:
    return re.search(
        r"(?:load|fetch)\(\s*['\"]" + re.escape(name) + r"['\"]", text
    ) is not None


# Keys site-stats.json must carry for the page to fill. Listed so that a
# generator that stops emitting one fails here rather than on the published
# page, where the only symptom is a dash nobody reports.
REQUIRED_STATS = (
    ("parity", "real"),
    ("parity", "total"),
    ("witnesses", "claims"),
    ("witnesses", "distinct"),
    ("e2e",),
)

# How a real-tenant witness would be spelled. `check_witnesses.py` names
# `ci:real-fabric` explicitly as the differential leg against a real tenant, so
# this is the repository's own spelling rather than one invented here.
REAL_TENANT = "real-fabric"


def shown(path: pathlib.PurePath) -> str:
    """A path as a reader wants to see it: relative to the repo when it is inside it.

    Every caller is inside a failure message, and `Path.relative_to` RAISES for
    a path outside ROOT. So the branch whose whole job is to explain a problem
    replaced the explanation with a ValueError from pathlib. Invisible in
    production, where these are module constants under ROOT, and that is
    exactly why nothing had ever executed one of them: the tests moved ROOT to
    sit above their fixtures, which is the workaround this removes.

    `as_posix()` rather than `str()`, as the sibling checkers already do: on
    Windows `str()` renders `docs\\witnesses.json`, so one failure would read
    two ways across the three platforms this suite runs on.

    `PurePath`, not `Path`: nothing here touches the filesystem, and the wider
    type is what lets the Windows rendering be tested from any platform.
    """
    try:
        return path.relative_to(ROOT).as_posix()
    except ValueError:
        return path.as_posix()


def read(path: pathlib.Path) -> str:
    """UTF-8 explicitly. The matrices' glyphs are what is being matched, and a
    locale-dependent read turns them into mojibake that matches nothing while
    still exiting 0 -- the failure `check_witnesses.py` records having hit on
    Windows for the whole of its life."""
    return path.read_text(encoding="utf-8")


def table_rows(text: str):
    """Yield the cells of every markdown table row that is not a separator."""
    for line in text.splitlines():
        if not line.startswith("|"):
            continue
        cells = [cell.strip() for cell in line.strip().strip("|").split("|")]
        if not cells or set("".join(cells)) <= set("-: "):
            continue
        yield cells


def real_tenant_claims() -> int:
    """Parity claims resting on a witness that talks to real Microsoft Fabric.

    Zero today, and the honest rendering of that is a number the page reads
    rather than a sentence somebody has to remember to delete on the day it
    stops being true.
    """
    data = json.loads(read(WITNESSES))
    claims = {k: v for k, v in data.items() if k != "_gated"}
    if not claims:
        raise SystemExit(f"FAIL: no claims parsed from {shown(WITNESSES)}")
    return sum(
        1
        for claim in claims.values()
        if any(REAL_TENANT in name for name in claim.get("witnesses") or [])
    )


def engine_matrix() -> dict:
    """Per-column pass counts from the generated Spark engine matrix.

    Three columns, because engine and emulator are different things: bare Sail,
    Sail as the emulator ships it (the delta-rs interception the Livy agent
    installs for every session), and the JVM overlay. The middle one is what a
    user actually gets, so it is the column the page quotes.
    """
    columns: list[str] = []
    passes: dict[str, int] = {}
    probes = 0
    for cells in table_rows(read(ENGINE_MATRIX)):
        if cells[0] == "Capability":
            columns = cells[1:]
            continue
        if not columns or len(cells) - 1 != len(columns):
            continue
        probes += 1
        for name, cell in zip(columns, cells[1:], strict=True):
            passes[name] = passes.get(name, 0) + (1 if cell.startswith("✅") else 0)
    if not probes:
        raise SystemExit(
            f"FAIL: no probes parsed from {shown(ENGINE_MATRIX)} -- the "
            "matrix is not empty, so this is a parsing failure, and the page "
            "would show a dash while looking fine."
        )
    return {"probes": probes, "passes": passes, "columns": columns}


def conformance() -> dict:
    """Proven and applicable cells from the generated conformance matrix.

    A cell is one contract on one backend. An em dash means the contract does
    not apply to that backend rather than that it failed, so it leaves the
    denominator. This is the page's least flattering figure and the reason the
    matrix is quoted at all.
    """
    contracts = proven = applicable = 0
    for cells in table_rows(read(CONFORMANCE)):
        if not cells[0].isdigit():
            continue
        contracts += 1
        for cell in cells[2:]:
            if cell == "—":
                continue
            applicable += 1
            if cell.startswith("✅"):
                proven += 1
    if not contracts:
        raise SystemExit(
            f"FAIL: no contracts parsed from {shown(CONFORMANCE)}"
        )
    return {"contracts": contracts, "proven": proven, "applicable": applicable}


def site_stats() -> dict:
    """The docs build's own tally, read rather than recomputed."""
    if not SITE_STATS.exists():
        raise SystemExit(
            f"FAIL: {shown(SITE_STATS)} does not exist. It is written by "
            "the Astro build (`pnpm --filter fabric-emulator-docs build`), which "
            "must run before this script."
        )
    stats = json.loads(read(SITE_STATS))
    for path in REQUIRED_STATS:
        node = stats
        for key in path:
            if not isinstance(node, dict) or key not in node:
                raise SystemExit(
                    f"FAIL: {shown(SITE_STATS)} no longer carries "
                    f"{'.'.join(path)}, which the landing page reads. Something in "
                    "website/scripts/sync-docs.mjs stopped counting it."
                )
            node = node[key]
    return stats


def check_page(page: pathlib.Path) -> tuple[int, str]:
    """Refuse a page that types a total, or stops reading one."""
    if not page.exists():
        return 1, f"FAIL: {page} does not exist."
    text = read(page)

    region = STATS_REGION.search(text)
    if not region:
        return 1, (
            f"FAIL: {page} has no <!-- stats:start --> ... <!-- stats:end --> region, "
            "so this script cannot tell a placeholder from a typed number."
        )
    stats = region.group(1)

    for match in ID_ELEMENT.finditer(stats):
        element_id, inner = match.group(2), match.group(3)
        if inner.strip() != PLACEHOLDER:
            return 1, (
                f"FAIL: {page} fills #{element_id} with {inner.strip()!r}. Every "
                "figure in the stat row is read at run time; a typed number goes "
                "stale the day the count moves, and this project has already "
                "published one that did."
            )

    for match in BOLD_ELEMENT.finditer(stats):
        body = re.sub(r"<[^>]+>", "", match.group(1))
        if any(char.isdigit() for char in body):
            return 1, (
                f"FAIL: {page} states {body.strip()!r} as a headline figure with no "
                "placeholder behind it. Bind it to an id and fill it from a manifest."
            )

    for element_id, source in BINDINGS.items():
        if f'id="{element_id}"' not in text:
            return 1, (
                f"FAIL: {page} no longer has #{element_id}, so a headline number "
                "would never fill."
            )
        if not fetches(text, source):
            return 1, (
                f"FAIL: {page} no longer reads {source}, so #{element_id} would "
                "show a dash forever."
            )

    return 0, ""


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", required=True)
    parser.add_argument("--landing", required=True)
    args = parser.parse_args()

    code, message = check_page(pathlib.Path(args.landing))
    if code:
        print(message)
        return code

    stats = site_stats()
    evidence = {
        "engine": engine_matrix(),
        "conformance": conformance(),
        # Absent from every ledger today. 0 is the honest answer rather than a
        # missing key the page would render as a dash, which reads as broken.
        "real_tenant": real_tenant_claims(),
    }

    out = pathlib.Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    (out / "site-stats.json").write_text(
        json.dumps(stats, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    (out / "evidence-summary.json").write_text(
        json.dumps(evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    engine = evidence["engine"]
    default_engine = next(
        (name for name in engine["columns"] if "emulator" in name), engine["columns"][1]
    )
    print(
        "landing data: "
        f"{stats['witnesses']['distinct']} distinct witnesses, "
        f"{stats['witnesses']['claims']} parity claims, "
        f"{stats['parity']['real']}/{stats['parity']['total']} ledger rows real, "
        f"{stats['e2e']} e2e suites, "
        f"{engine['passes'][default_engine]}/{engine['probes']} engine probes pass, "
        f"{evidence['conformance']['proven']}/{evidence['conformance']['applicable']} "
        "conformance cells proven, "
        f"{evidence['real_tenant']} compared against a real tenant"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
