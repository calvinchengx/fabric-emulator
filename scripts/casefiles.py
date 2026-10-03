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


def substitute(value, ids: dict):
    """`value` with every `{name}` in its strings replaced by ids[name].

    A case names fixture objects by role (`{model}`), because each runner
    creates its own and learns the id only at run time.
    """
    if isinstance(value, str):
        for name, real in ids.items():
            value = value.replace("{" + name + "}", real)
        return value
    if isinstance(value, list):
        return [substitute(v, ids) for v in value]
    if isinstance(value, dict):
        return {k: substitute(v, ids) for k, v in value.items()}
    return value


_MISSING = object()


def at(doc, path: list):
    """The value at `path` in a decoded JSON document.

    A string step is a key, an int a list index, and `*` collects the rest of
    the path from every element of a list. A step that does not resolve gives
    a sentinel that equals nothing, so a wrong path fails the expectation
    rather than matching null.
    """
    for n, step in enumerate(path):
        if step == "*":
            if not isinstance(doc, list):
                return _MISSING
            return [at(v, path[n + 1:]) for v in doc]
        if isinstance(step, int) and not isinstance(step, bool):
            if not isinstance(doc, list) or not -len(doc) <= step < len(doc):
                return _MISSING
            doc = doc[step]
        elif isinstance(doc, dict) and step in doc:
            doc = doc[step]
        else:
            return _MISSING
    return doc


EXPECT_OPS = ("equals", "contains", "length", "order")


def unmet(doc, expectation: dict):
    """Why `doc` fails `expectation`, or None when it holds.

    An expectation is {"at": path, <one of EXPECT_OPS>: value}; see at().
    """
    ops = [op for op in EXPECT_OPS if op in expectation]
    if len(ops) != 1 or set(expectation) != {"at", ops[0]}:
        return f"expectation {expectation!r} must have `at` and exactly one of {EXPECT_OPS}"
    op = ops[0]
    want = expectation[op]
    got = at(doc, expectation["at"])
    shown = "nothing" if got is _MISSING else repr(got)
    if op == "equals":
        ok = _same(got, want)
    elif op == "contains":
        ok = isinstance(got, list) and any(_same(v, want) for v in got)
    elif op == "length":
        ok = isinstance(got, (list, dict, str)) and len(got) == want
    elif want == "descending":
        ok = isinstance(got, list) and all(_number(v) for v in got) and got == sorted(got, reverse=True)
    else:
        return f"order {want!r} is not one this runner knows (descending)"
    return None if ok else f"at {expectation['at']}: want {op} {want!r}, got {shown}"


def _number(v) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def _same(a, b) -> bool:
    """JSON equality: 3 is 3.0, but true is not 1 and a missing value is nothing."""
    if a is _MISSING or b is _MISSING:
        return False
    if _number(a) and _number(b):
        return a == b
    if type(a) is not type(b):
        return False
    if isinstance(a, list):
        return len(a) == len(b) and all(_same(x, y) for x, y in zip(a, b, strict=True))
    if isinstance(a, dict):
        return a.keys() == b.keys() and all(_same(a[k], b[k]) for k in a)
    return a == b


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
