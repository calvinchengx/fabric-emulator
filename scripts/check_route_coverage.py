#!/usr/bin/env python3
r"""A route the emulator serves must be exercised, or listed as not yet.

THE RATCHET, and it is the piece that makes a coverage number stay true.
`check_openapi_conformance.py` validates the responses a suite happened to
produce. It says nothing about the routes no suite touches, and those are the
ones where a wrong shape lives longest -- nobody is looking.

Measured when this landed: the emulator registers 133 routes under the
documented prefixes and one suite exercises 19 of them. A target of "all 133"
is not the gate. THE GATE IS THAT THE NUMBER CANNOT GET WORSE:

  * a NEW route registered with no conformance traffic fails, so an endpoint
    arrives with its evidence instead of acquiring it later;
  * a route that STOPS being exercised fails, because coverage silently
    regressing is the same defect a stale witness is;
  * a route that STARTS being exercised also fails, which sounds perverse and
    is the point -- the baseline is a record of what is not yet proved, and an
    improvement that does not shrink it leaves a lie in the file. Updating it
    is one command and the diff is the review.

WHY NOT SIMPLY REQUIRE FULL COVERAGE. Because the honest denominator is not
729 documented routes but the ones this emulator serves, and even those cannot
all be reached by the suites that exist today. A gate nobody can pass is a gate
somebody deletes. A gate that only ratchets is one that keeps paying.

WHAT COUNTS AS EXERCISED. One recorded response on a route, from any suite that
records. Not every status, not every parameter -- those are a second-order
target the recording already captures the data for. This is the crude, useful
question: has anything ever driven this route with a schema watching.

Usage:
    check_route_coverage.py <recording.jsonl> [...]        report, exit 0
    check_route_coverage.py <recording.jsonl> --strict     exit non-zero on drift
    check_route_coverage.py <recording.jsonl> --update     rewrite the baseline
"""
import argparse
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASELINE = ROOT / "docs" / "route-coverage.json"

# Where routes are registered. Both packages mount on the same mux.
SOURCES = (ROOT / "internal" / "api", ROOT / "internal" / "server")

# The prefixes the recorder writes down, and therefore the only ones a
# recording can ever prove. Kept in step with recordable() in
# internal/server/record.go: widening one without the other makes this gate
# demand evidence the recorder cannot produce.
IN_SCOPE = ("/v1/", "/v1.0/")

_REGISTRATION = re.compile(
    r'HandleFunc\("(GET|POST|PUT|PATCH|DELETE|HEAD) (/[^"]*)"')


def registered():
    """Every (method, path template) the emulator mounts, in scope."""
    found = set()
    for source in SOURCES:
        for path in sorted(source.rglob("*.go")):
            if path.name.endswith("_test.go"):
                continue
            for method, route in _REGISTRATION.findall(path.read_text(encoding="utf-8")):
                if route.startswith(IN_SCOPE):
                    found.add(f"{method} {route}")
    return found


def exercised(recordings):
    """The registered routes at least one recorded response matched.

    Matching is the template's, not the literal path's: a recording holds
    `/v1/workspaces/<guid>/items` and the registration holds
    `{wid}`. Longest template first, so `/items/{iid}` is credited before the
    shorter `/items` when both could match.
    """
    patterns = []
    for label in registered():
        method, _, template = label.partition(" ")
        regex = re.compile("^" + re.sub(r"\{[^}]+\}", "[^/]+", template) + "$")
        patterns.append((method, regex, template, label))
    patterns.sort(key=lambda p: len(p[2]), reverse=True)

    seen = set()
    for recording in recordings:
        for line in recording.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                continue
            for method, regex, _, label in patterns:
                if method == entry.get("method") and regex.match(entry.get("path", "")):
                    seen.add(label)
                    break
    return seen


def read_baseline():
    if not BASELINE.is_file():
        return None
    return set(json.loads(BASELINE.read_text(encoding="utf-8"))["notYetExercised"])


def write_baseline(uncovered, total):
    BASELINE.write_text(json.dumps({
        "_comment": [
            "Routes the emulator serves that no recording has exercised.",
            "A RECORD OF WHAT IS NOT YET PROVED, not a list of exemptions:",
            "every line is a surface whose response shape nothing has ever",
            "checked against Microsoft's schema. It is meant to shrink.",
            "scripts/check_route_coverage.py fails when this file and the",
            "tree disagree in either direction -- a new unexercised route,",
            "or an exercised one still listed here. Regenerate with",
            "check_route_coverage.py <recording> --update and let the diff",
            "be the review.",
        ],
        "exercised": total - len(uncovered),
        "registered": total,
        "notYetExercised": sorted(uncovered),
    }, indent=2) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("recordings", nargs="+", type=pathlib.Path)
    parser.add_argument("--strict", action="store_true",
                        help="exit non-zero when the baseline and the tree disagree")
    parser.add_argument("--update", action="store_true",
                        help="rewrite the baseline from this run")
    arguments = parser.parse_args()

    present = [r for r in arguments.recordings if r.is_file()]
    if not present:
        print("check_route_coverage: no recording exists. Set "
              "FABRIC_RECORD_RESPONSES on the emulator and re-run a suite.",
              file=sys.stderr)
        return 1

    all_routes = registered()
    covered = exercised(present)
    uncovered = all_routes - covered

    if arguments.update:
        write_baseline(uncovered, len(all_routes))
        print(f"check_route_coverage: baseline rewritten — "
              f"{len(covered)}/{len(all_routes)} exercised")
        return 0

    pinned = read_baseline()
    if pinned is None:
        print(f"check_route_coverage: no baseline at {BASELINE}. "
              f"Create one with --update.", file=sys.stderr)
        return 1

    print(f"check_route_coverage: {len(covered)}/{len(all_routes)} registered "
          f"routes exercised by {len(present)} recording(s)")

    newly_uncovered = sorted(uncovered - pinned)
    newly_covered = sorted(pinned - uncovered)
    if not newly_uncovered and not newly_covered:
        print(f"  {len(pinned)} route(s) not yet exercised, as recorded")
        return 0

    if newly_uncovered:
        print(f"\n{len(newly_uncovered)} route(s) the emulator serves with no "
              f"conformance traffic, and not in the baseline:\n")
        for route in newly_uncovered:
            print(f"  - {route}")
        print("\n  -> drive it from a suite that records, so its shape is "
              "checked against Microsoft's schema. If it genuinely cannot be "
              "driven yet, add it with --update and say why in the change.")
    if newly_covered:
        print(f"\n{len(newly_covered)} route(s) are now exercised but still "
              f"listed as not yet:\n")
        for route in newly_covered:
            print(f"  - {route}")
        print("\n  -> run --update. The baseline records what is NOT proved, "
              "so leaving a proved route in it is a lie in the file.")
    return 1 if arguments.strict else 0


if __name__ == "__main__":
    sys.exit(main())
