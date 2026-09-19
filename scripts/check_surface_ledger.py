#!/usr/bin/env python3
"""Every operation Microsoft documents, in one of three states.

WHY THIS EXISTS, given there is already a route-coverage ratchet. That gate
asks "did traffic touch what we serve", and its denominator is what this
emulator REGISTERS. It is silent, by construction, about the operations
Microsoft documents and this emulator does not implement -- which is most of
them. A surface can be at 100% route coverage and still answer nothing at all
for two thirds of the API, and no gate would say so.

The three states, and only three:

  SERVED   -- a registered route matches the documented (method, path). What it
              answers is then held to the schema by check_openapi_conformance.
  REFUSED  -- not served, and MEASURED to answer a legible 404: a recording
              contains a response for a concrete path under this operation.
              A refusal is a fine answer. An unasserted one is not, so this
              state is evidence-backed rather than declared.
  SILENT   -- neither. Nothing serves it and nothing has ever asked. This is
              the state that hurts: an integration that needs the route finds
              out at runtime, and no test in this repository disagrees.

THE GATE IS ON THE SILENT SET, and it ratchets in both directions like its
sibling: silent must not grow (a new documented operation nobody looked at),
and an operation that LEAVES silent must leave the baseline too, or the file
records a lie about the tree.

WHAT THIS IS NOT. It says nothing about whether a served operation answers
correctly -- that is conformance, a different gate on a different input. An
operation can be SERVED and wrong. It can also be REFUSED and that be exactly
right: this emulator has no personal workspace and no PBIX importer, and
pretending otherwise would be worse than the 404.

Usage:
    check_surface_ledger.py [recording.jsonl ...]            report, exit 0
    check_surface_ledger.py [recording.jsonl ...] --strict   exit non-zero on drift
    check_surface_ledger.py [recording.jsonl ...] --update   rewrite the baseline
"""
import argparse
import collections
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASELINE = ROOT / "docs" / "surface-ledger.json"

sys.path.insert(0, str(ROOT / "scripts"))

import check_openapi_conformance as conf  # noqa: E402
import check_route_coverage as cov  # noqa: E402

# Only what a recording could ever prove, kept in step with the recorder.
IN_SCOPE = cov.IN_SCOPE


def _shape(template):
    """A path template with its parameter NAMES removed.

    The spec spells it {workspaceId} and this emulator spells it {wid}; they
    are the same route. Go's wildcard `{path...}` matches a whole subtree, so
    it is folded to the same placeholder -- coarser than the router, and
    coarse in the direction of claiming LESS coverage rather than more.
    """
    return re.sub(r"\{[^}]+\}", "{}", template).rstrip("/")


def documented():
    """Every documented operation in scope, as (METHOD, template, spec file)."""
    specs = conf.Specs()
    out = {}
    for method, _pattern, template, origin, _responses in specs.routes:
        if not template.startswith(IN_SCOPE):
            continue
        out[f"{method} {template}"] = (method, template, origin)
    return out, specs


def served_shapes():
    """The shapes this emulator registers, with alias families expanded.

    check_route_coverage.registered() already returns the real collection
    names rather than a `{collection}` placeholder -- a placeholder would be a
    wildcard in its matcher and would swallow its own siblings. That gate then
    COLLAPSES the family for counting, because 459 rows of one handler is a
    baseline nobody reads. This gate does not collapse: Microsoft documents
    `.../notebooks` and `.../warehouses` as SEPARATE operations and this
    emulator answers both, so each is its own fact here.

    Measured when the expansion was fixed: served went 120 -> 422 and silent
    865 -> 563. The ledger had been reporting 302 operations as
    nothing-has-ever-asked when the emulator serves them.
    """
    shapes = collections.defaultdict(set)
    for label in cov.registered():
        method, _, template = label.partition(" ")
        shapes[method].add(_shape(template))
    return shapes


def refused(specs, recordings):
    """Operations a recording shows answering 404, i.e. a MEASURED refusal.

    Read from the same recordings conformance uses, so a refusal is only
    credited when a suite really asked and really got told no.
    """
    seen = set()
    for recording in recordings:
        for entry in conf.read_recording(pathlib.Path(recording)):
            if entry.get("status") != 404:
                continue
            template, _origin, _responses = specs.match(
                entry.get("method", ""), entry.get("path", ""))
            if template:
                seen.add(f"{entry['method']} {template}")
    return seen


def classify(recordings):
    ops, specs = documented()
    shapes = served_shapes()
    measured = refused(specs, recordings)
    states = {}
    for label, (method, template, _origin) in ops.items():
        if _shape(template) in shapes.get(method, ()):
            states[label] = "served"
        elif label in measured:
            states[label] = "refused"
        else:
            states[label] = "silent"
    return states


def read_baseline():
    if not BASELINE.is_file():
        return None
    return json.loads(BASELINE.read_text(encoding="utf-8"))


def write_baseline(states):
    counts = collections.Counter(states.values())
    BASELINE.write_text(json.dumps({
        "_comment": [
            "Every operation Microsoft's published specs document, in one of",
            "three states. Written by scripts/check_surface_ledger.py --update;",
            "do not hand-edit. `silent` is the set nothing serves and nothing",
            "has ever asked about, and it is the one the gate holds down.",
            "`served` and `refused` are listed so that leaving those states is",
            "also a diff somebody reviews.",
        ],
        "documented": len(states),
        "served": counts["served"],
        "refused": counts["refused"],
        "silent": counts["silent"],
        "servedOperations": sorted(k for k, v in states.items() if v == "served"),
        "refusedOperations": sorted(k for k, v in states.items() if v == "refused"),
    }, indent=2) + "\n", encoding="utf-8")


def main_for_test(strict=False, update=False, recordings=()):
    """main() without argparse, so the gate itself can be tested.

    The gate is the part that can rot unnoticed -- a ratchet that never fires
    is indistinguishable from a healthy tree -- so it is reachable without
    building a fake argv.
    """
    return _run(list(recordings), strict, update)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("recordings", nargs="*")
    parser.add_argument("--strict", action="store_true")
    parser.add_argument("--update", action="store_true")
    args = parser.parse_args()
    return _run(args.recordings, args.strict, args.update)


def _run(recordings, strict, update):
    states = classify(recordings)
    counts = collections.Counter(states.values())
    total = len(states)
    print(f"check_surface_ledger: {total} documented operations — "
          f"{counts['served']} served, {counts['refused']} refused, "
          f"{counts['silent']} silent")

    if update:
        write_baseline(states)
        print(f"  baseline written to {BASELINE.relative_to(ROOT)}")
        return 0

    base = read_baseline()
    if base is None:
        print("  no baseline; run with --update", file=sys.stderr)
        return 1 if strict else 0

    problems = []
    was_served = set(base.get("servedOperations", ()))
    was_refused = set(base.get("refusedOperations", ()))
    now_served = {k for k, v in states.items() if v == "served"}
    now_refused = {k for k, v in states.items() if v == "refused"}

    # A regression in either direction, and an IMPROVEMENT that left the file
    # stale. The third is the one people soften; it is the one that keeps the
    # baseline from becoming a story about a tree that no longer exists.
    for label in sorted(was_served - now_served):
        problems.append(f"no longer served, and the baseline still says it is: {label}")
    for label in sorted(was_refused - now_refused):
        problems.append(f"no longer a measured refusal — did a suite stop asking? {label}")
    for label in sorted(now_served - was_served):
        problems.append(f"newly served and not in the baseline: {label}")
    for label in sorted(now_refused - was_refused):
        problems.append(f"newly refused and not in the baseline: {label}")

    if problems:
        print()
        print(f"{len(problems)} disagreement(s) between the tree and the ledger:")
        for p in problems[:40]:
            print(f"  - {p}")
        if len(problems) > 40:
            print(f"  … and {len(problems) - 40} more")
        print("\n  -> rerun with --update and review the diff; a shrinking `silent` "
              "count is the point, but it has to be recorded to count.")
        return 1 if strict else 0

    print(f"  ledger agrees with the tree ({counts['silent']} silent, unchanged)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
