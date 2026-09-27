#!/usr/bin/env python3
"""Test cases kept as data, read by every runner that executes them.

A suite whose cases are a table — many rows, one procedure — keeps the rows in
`cases/<suite>.json` and the procedure in code. The unit test, an e2e runner,
and later a Go table test all read the same file, so a case cannot be edited in
one place and left stale in another. The convention, and when NOT to use it, is
in docs/10-testing.md ("Test cases as data").

A suite file is

    {
      "suite": "<the file's own stem>",
      "about": "<what one case means and what the runners owe it>",
      "cases": [
        {"id": "kebab-case", "why": "<the break this case guards>", ...},
        ...
      ]
    }

Every field past `id` and `why` belongs to the suite and is checked by the
suite's own test. `executed_by`, when a case has it, lists the runners beyond
the unit test that must execute the case, as paths from the repository root;
each path must exist, and the suite's test holds the runner to the list.

Standard library only: the e2e runners import this outside the test venv, and
`make check` runs it under $(PY), which has no third-party packages.

Usage:
    casefiles.py --check    validate every file under cases/
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CASES_DIR = ROOT / "cases"

_ID = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
_SUITE_KEYS = {"suite", "about", "cases"}


class CaseFileError(ValueError):
    """A case file that no runner should trust."""


def problems(path: Path, root: Path = ROOT) -> list:
    """Every reason `path` is not a valid suite file; empty when it is."""
    try:
        doc = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        return [f"{path.name}: not readable JSON ({exc})"]
    if not isinstance(doc, dict):
        return [f"{path.name}: the top level must be an object"]

    out = []
    extra = sorted(set(doc) - _SUITE_KEYS)
    if extra:
        out.append(f"{path.name}: unknown top-level key(s) {extra}")
    if doc.get("suite") != path.stem:
        out.append(f"{path.name}: \"suite\" is {doc.get('suite')!r}, "
                   f"want {path.stem!r} (the file's own name)")
    if not isinstance(doc.get("about"), str) or not doc["about"].strip():
        out.append(f"{path.name}: \"about\" must say what one case means")
    cases = doc.get("cases")
    if not isinstance(cases, list) or not cases:
        out.append(f"{path.name}: \"cases\" must be a non-empty list")
        return out

    seen = set()
    for n, case in enumerate(cases):
        where = f"{path.name} case {n}"
        if not isinstance(case, dict):
            out.append(f"{where}: must be an object")
            continue
        cid = case.get("id")
        if not isinstance(cid, str) or not _ID.match(cid):
            out.append(f"{where}: id {cid!r} must be kebab-case")
        else:
            where = f"{path.name} {cid}"
            if cid in seen:
                out.append(f"{where}: duplicate id")
            seen.add(cid)
        why = case.get("why")
        if not isinstance(why, str) or not why.strip():
            out.append(f"{where}: \"why\" must name the break this case guards")
        runners = case.get("executed_by", [])
        if not isinstance(runners, list) or not all(isinstance(r, str) for r in runners):
            out.append(f"{where}: \"executed_by\" must be a list of paths")
        else:
            for r in runners:
                if not (root / r).is_file():
                    out.append(f"{where}: executed_by names {r!r}, which does not exist")
    return out


def load(suite: str, cases_dir: Path = CASES_DIR, root: Path = ROOT) -> list:
    """The cases of `suite`, or CaseFileError naming everything wrong with it."""
    path = cases_dir / f"{suite}.json"
    found = problems(path, root)
    if found:
        raise CaseFileError("\n".join(found))
    return json.loads(path.read_text(encoding="utf-8"))["cases"]


def executed_by(cases: list, runner: str) -> set:
    """Ids of the cases that name `runner` in executed_by."""
    return {c["id"] for c in cases if runner in c.get("executed_by", [])}


def text(value) -> str:
    """A string field that may be written as a list of lines.

    JSON has no multi-line strings, and a statement copied verbatim from a log
    has to keep every blank line and indent. Lines are joined with a newline and
    nothing is added at the end.
    """
    if isinstance(value, str):
        return value
    if isinstance(value, list) and all(isinstance(v, str) for v in value):
        return "\n".join(value)
    raise TypeError(f"expected a string or a list of lines, got {value!r}")


def check(cases_dir: Path = CASES_DIR, root: Path = ROOT) -> int:
    files = sorted(cases_dir.glob("*.json"))
    found = [p for f in files for p in problems(f, root)]
    if found:
        print("case files:", file=sys.stderr)
        for p in found:
            print(f"  {p}", file=sys.stderr)
        return 1
    total = sum(len(json.loads(f.read_text(encoding="utf-8"))["cases"]) for f in files)
    print(f"case files: {len(files)} suite(s), {total} case(s); every id unique "
          f"and kebab-case, every case says why, every runner it names exists")
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--check", action="store_true", help="validate every file under cases/")
    args = ap.parse_args(argv)
    if not args.check:
        ap.print_help()
        return 2
    return check()


if __name__ == "__main__":
    sys.exit(main())
