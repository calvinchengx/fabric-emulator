#!/usr/bin/env python3
"""Every framework-conformance contract must say which backends prove it.

`docs/38-framework-conformance.md` defines the runtime contracts a Fabric
framework depends on, and states the rule that gives them teeth: each one is
proven by EXECUTING on a real backend — a real SQL Server for the Warehouse,
Parquet/Delta in OneLake written by Sail and by JVM PySpark for the Lakehouse —
and verified out of band, by a reader that is not the engine that wrote.

The proof itself needs those backends live, so it runs in CI. What this script
enforces is the half that is checkable offline, and it is the half that rots:
the correspondence between the contracts the document DEFINES, the backends it
CLAIMS prove them, and the matrix CI actually produces. Two ways that drifts,
both of which this repo has already paid for in the parity map:

  * a contract gains a section and nobody adds it to the applicability table,
    so no backend is ever asked to prove it and its absence looks deliberate;
  * a matrix cell quietly stops being produced, and a claim keeps citing a
    witness that no longer runs.

Before the kit is built there is no matrix, and this says so rather than
passing silently — the applicability table is still enforced against the
document, which is what makes the check useful on day one.

Usage:
    check_conformance.py [--strict]

    --strict  treat a missing conformance matrix as a failure. Off by default,
              because the kit is scoped and not yet built; turn it on in the
              same change that lands it, so the matrix can never later vanish.
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
DOC = REPO / "docs" / "38-framework-conformance.md"
MATRIX = REPO / "docs" / "conformance-matrix.md"
WITNESSES = REPO / "docs" / "witnesses.json"

BACKENDS = ("sail", "jvm", "warehouse")
# A cell in the applicability table. `required` and `control` both demand a
# verdict from CI; they differ in what a pass MEANS, which is the matrix's
# business, not this script's. `n/a` is a positive statement that the contract
# does not apply — contracts 1-3 are properties of a notebook session, and the
# Warehouse surface has none.
APPLICABILITY = ("required", "control", "n/a")
PROVING = ("required", "control")

APPLICABILITY_BLOCK = re.compile(
    r"<!--\s*APPLICABILITY:BEGIN.*?-->(?P<body>.*?)<!--\s*APPLICABILITY:END\s*-->",
    re.DOTALL)
# "### 4. A success claim must be witnessed by the artifact" -> 4
CONTRACT_HEADING = re.compile(r"^###\s+(\d+)\.\s+(.+?)\s*$", re.MULTILINE)


def fail(msg: str) -> None:
    print(f"  FAIL  {msg}")


def parse_table(body: str) -> list[list[str]]:
    """Rows of a GitHub markdown table, header and separator dropped."""
    rows = []
    for line in body.splitlines():
        line = line.strip()
        if not line.startswith("|"):
            continue
        cells = [c.strip() for c in line.strip("|").split("|")]
        if all(set(c) <= set("-: ") for c in cells):  # separator row
            continue
        rows.append(cells)
    return rows[1:] if rows else []


def load_applicability() -> tuple[dict[int, dict[str, str]], list[str]]:
    """{contract number: {backend: applicability}} from the doc's marked table."""
    errors: list[str] = []
    text = DOC.read_text(encoding="utf-8")
    block = APPLICABILITY_BLOCK.search(text)
    if not block:
        return {}, [f"{DOC.name}: no APPLICABILITY:BEGIN/END block"]

    rows = parse_table(block.group("body"))
    table: dict[int, dict[str, str]] = {}
    for cells in rows:
        if len(cells) != 2 + len(BACKENDS):
            errors.append(f"{DOC.name}: applicability row has {len(cells)} "
                          f"columns, expected {2 + len(BACKENDS)}: {cells}")
            continue
        try:
            num = int(cells[0])
        except ValueError:
            errors.append(f"{DOC.name}: applicability row does not start with a "
                          f"contract number: {cells}")
            continue
        verdicts = {}
        for backend, cell in zip(BACKENDS, cells[2:], strict=False):
            value = "n/a" if cell in ("—", "-", "n/a") else cell
            if value not in APPLICABILITY:
                errors.append(f"{DOC.name}: contract {num}/{backend}: "
                              f"{cell!r} is not one of {APPLICABILITY}")
            verdicts[backend] = value
        table[num] = verdicts
    return table, errors


def check_doc_covers_every_contract(table: dict[int, dict[str, str]]) -> list[str]:
    """The applicability table lists every contract the document defines.

    This is the invariant that catches the drift that matters: a contract
    written up in prose but never assigned to a backend is a contract nobody
    proves, and its absence from CI looks like a decision rather than an
    oversight.
    """
    errors = []
    text = DOC.read_text(encoding="utf-8")
    defined = {int(n): title for n, title in CONTRACT_HEADING.findall(text)}
    if not defined:
        return [f"{DOC.name}: no '### N. Title' contract headings found"]

    for num, title in sorted(defined.items()):
        if num not in table:
            errors.append(f"contract {num} ({title}) is defined but missing "
                          f"from the applicability table")
        elif not any(v in PROVING for v in table[num].values()):
            errors.append(f"contract {num} ({title}) is n/a on every backend, "
                          f"so nothing would ever prove it")
    for num in sorted(table):
        if num not in defined:
            errors.append(f"applicability table lists contract {num}, which no "
                          f"'### {num}.' section defines")
    return errors


def check_matrix(table: dict[int, dict[str, str]]) -> list[str]:
    """Every cell CI is asked to prove carries a verdict, and every ❌ a pointer."""
    errors = []
    rows = parse_table(MATRIX.read_text(encoding="utf-8"))
    seen: dict[tuple[int, str], str] = {}
    for cells in rows:
        if len(cells) < 2 + len(BACKENDS):
            continue
        try:
            num = int(cells[0])
        except ValueError:
            continue
        for backend, cell in zip(BACKENDS, cells[2:], strict=False):
            seen[(num, backend)] = cell

    for num, verdicts in sorted(table.items()):
        for backend, applicability in verdicts.items():
            if applicability not in PROVING:
                continue
            cell = seen.get((num, backend))
            if cell is None:
                errors.append(f"contract {num}/{backend} is {applicability} but "
                              f"{MATRIX.name} has no cell for it")
            elif "❌" in cell and not re.search(r"\[.+?\]\(.+?\)|#\d+", cell):
                # A failing cell is allowed - the kit lands before every
                # contract passes - but it must point at what closes it, or the
                # matrix becomes a list of unexplained reds nobody can action.
                errors.append(f"contract {num}/{backend} is ❌ with no pointer "
                              f"to the gap or issue that closes it")
    return errors


def check_witnesses(table: dict[int, dict[str, str]]) -> list[str]:
    """Witnesses the matrix names must exist in the witness map."""
    if not MATRIX.exists() or not WITNESSES.exists():
        return []
    known = set()
    data = json.loads(WITNESSES.read_text(encoding="utf-8"))
    for key, value in data.items():
        if key == "_gated":
            known.update(value)
            continue
        known.update(value.get("witnesses", []))
    errors = []
    for name in sorted(set(re.findall(r"ci:conformance-[a-z0-9-]+",
                                      MATRIX.read_text(encoding="utf-8")))):
        if name not in known:
            errors.append(f"{MATRIX.name} names witness {name}, which "
                          f"{WITNESSES.name} does not define")
    return errors


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--strict", action="store_true",
                    help="a missing conformance matrix is a failure")
    args = ap.parse_args()

    if not DOC.exists():
        print(f"  FAIL  {DOC.name} is missing")
        return 1

    table, errors = load_applicability()
    errors += check_doc_covers_every_contract(table)

    proving = sum(1 for v in table.values() for a in v.values() if a in PROVING)
    print(f"conformance: {len(table)} contracts, {proving} backend cells to prove "
          f"across {', '.join(BACKENDS)}")

    if MATRIX.exists():
        errors += check_matrix(table)
        errors += check_witnesses(table)
    elif args.strict:
        errors.append(f"{MATRIX.name} is missing and --strict was given")
    else:
        # Loud, not silent: "the check is green" must never come to mean "the
        # kit was never built" without someone reading that it said so.
        print(f"  NOTE  {MATRIX.name} does not exist yet — the kit is scoped in "
              f"{DOC.name} and not built. Cell coverage NOT enforced.")

    for e in errors:
        fail(e)
    if errors:
        print(f"\n{len(errors)} problem(s)")
        return 1
    print("  ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
