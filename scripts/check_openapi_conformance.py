#!/usr/bin/env python3
r"""Responses the emulator really returned, against Microsoft's own schema.

WHY THIS EXISTS, and it is a different KIND of evidence from the rest. Every
`ci:` witness in docs/witnesses.json is a CLIENT: it drives a surface and its
own model rejects a wrong shape. That is strong, and it is narrow -- fifteen
claims have no client witness at all, because no packaged client speaks those
surfaces, so the implementation and the expectation behind them were written by
the same hand. This reads a machine-readable spec that is Microsoft's, and
covers every route a suite happens to touch rather than the few a client
bothers with.

HOW THE INPUT IS PRODUCED. `FABRIC_RECORD_RESPONSES=<file>` makes the emulator
append one JSON object per line for each documented-surface response: method,
path, status, body. No headers, ever -- see internal/server/record.go. The e2e
suites already generate the traffic; recording is the only new thing, and a
suite that does not set the variable simply contributes nothing.

WHAT IS CHECKED, three classes, chosen because they are what a typed client
stumbles into:

  1. UNDOCUMENTED STATUS -- the emulator answered a code the spec does not list
     for that route.
  2. MISSING REQUIRED PROPERTY -- including inside arrays, which is where most
     of the surface lives.
  3. WRONG PRIMITIVE TYPE, and enum membership.

WHAT IS DELIBERATELY NOT CHECKED. Unexpected properties. Swagger omits
`additionalProperties` almost everywhere, so "extra field" would fire on nearly
every response, and a checker that cries wolf gets muted -- docs/10 has the
full account of what that costs.

WHAT A PASS MEANS. Shape, never semantics. A job reported `Succeeded` that ran
nothing is perfectly conformant. This says the answer was SHAPED right, not
that it was TRUE.

TWO MATCHING RULES THAT WERE MEASURED RATHER THAN ASSUMED, both of which
produced wrong answers first:

  * A `$ref`'s ORIGIN TRAVELS WITH IT. A ref into `common/definitions.json`
    lands in a document whose own refs are relative to THAT file; keeping the
    pointing document's directory makes every nested ref resolve against the
    wrong place, and the first one crashes.
  * THE SPEC ENCODES QUERY VARIANTS AS SEPARATE PATH KEYS --
    `/v1/admin/domains?preview=true` and `?preview=false` are two entries for
    one route. Matching a key literally makes a real, served, documented route
    look undocumented.

Usage:
    check_openapi_conformance.py <recording.jsonl>
    check_openapi_conformance.py <recording.jsonl> --strict   exit non-zero on findings
"""
import argparse
import collections
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent

# TWO SPEC TREES, because Fabric and Power BI are two products with two
# published definitions and the emulator serves both surfaces. Without the
# second, every /v1.0/myorg response landed in "no documented route" -- not a
# finding, and therefore not checked at all, which is the quietest way for a
# surface to go unvalidated.
#
# powerbi-rest-swagger was already vendored here as a "golden reference" and
# had only ever been READ: cited in Go comments, never compared against a
# response. It is executed against now.
SPEC_ROOTS = (
    ROOT / "third_party" / "fabric-rest-api-specs",
    ROOT / "third_party" / "powerbi-rest-swagger",
)

# Disagreements this tree still has, pinned so a NEW one fails.
#
# The first run of this checker found ELEVEN, every one on a surface that was
# already passing a client witness -- the deployment-pipeline driver asserts on
# the fields it uses and never looks at the rest, which is exactly the blind
# spot a schema oracle is for. Ten were the emulator UNDER-ANSWERING and are
# fixed: create now returns the stages it made, and an operation reports its
# type, status, lastUpdatedTime, a principal-shaped performedBy and an
# object-shaped note.
#
# An entry is a SUBSTRING of the finding text. Adding one is a claim that the
# disagreement is known and unadjudicated; the list is auditable and it shrinks
# -- it has already gone from eleven to one.
KNOWN = {
    # NOT IMPLEMENTED, and the 404 is the honest answer rather than a wrong
    # shape. Each is graded in docs/parity.md and asserted as refused by
    # e2e/powerbi-ps, which pins the gap so it cannot close unnoticed. They are
    # here because the spec documents only a 200 for them, so an unimplemented
    # route reads to this checker as a status disagreement.
    "GET /v1.0/myorg/reports: answered 404":
        "Power BI Report items over the /v1.0 surface are not served.",
    "GET /v1.0/myorg/groups/{groupId}/reports: answered 404": "as above.",
    "GET /v1.0/myorg/capacities: answered 404":
        "the Power BI spelling of capacities is not served; the Fabric "
        "spelling /v1/capacities is.",
    "GET /v1.0/myorg/admin/groups: answered 404":
        "the Power BI spelling of tenant-wide workspace admin is not served; "
        "the Fabric spelling /v1/admin/workspaces is, and is what the claim "
        "in docs/witnesses.json is about.",
    "POST /v1.0/myorg/groups: answered 404":
        "New-PowerBIWorkspace has no route here; creation goes through the "
        "Fabric surface.",
    "microsoftEntraMembers[0]: MISSING required property 'tenantId'":
        "NOT the emulator inventing a shape: OneLake roles are stored as the "
        "raw body the caller PUT and echoed back, so this is a test fixture's "
        "omission reflected. The spec marks tenantId required on "
        "MicrosoftEntraMember, so the faithful fix is to REFUSE a member "
        "without one at authoring time -- which is a behaviour change across "
        "several fixtures and deserves its own change rather than being "
        "folded into the one that made this visible.",
}


def is_known(finding):
    return any(pin in finding for pin in KNOWN)


def stale_pins(raw):
    """Entries in KNOWN that no finding matches any more.

    A PIN THAT MATCHES NOTHING IS A DEAD LINE: it reads as a known problem
    while describing one that no longer exists, and it hides the good news,
    because a pin goes stale exactly when somebody fixes the thing.

    REPORTED, NEVER FATAL, and that distinction was wrong in the first draft.
    Staleness is a claim about THIS run, not about the tree: a pin is silent
    when its route was not exercised, which happens whenever a suite fails and
    uploads no recording, or when somebody runs one suite locally. Failing on
    it would turn an unrelated failure into a second, more confusing one --
    the exact thing the aggregate job's comment says it is avoiding. So this
    prints, and a human decides whether the entry has outlived its defect.
    """
    return sorted(pin for pin in KNOWN if not any(pin in f for f in raw))

TYPES = {
    "string": str, "integer": int, "number": (int, float),
    "boolean": bool, "array": list, "object": dict,
}

# Statuses never reported as undocumented.
#
# AUTHENTICATION IS NOT A ROUTE'S BUSINESS. A 401 or 403 is the same answer
# everywhere and specs enumerate it globally rather than per operation, so
# flagging it would fire on any suite that exercises an auth failure -- which
# is a thing suites SHOULD do. Measured: medallion drives executeQueries
# without a token and the emulator correctly answers 401, which read as
# "answered 401, spec documents ['200']".
#
# 404 IS DELIBERATELY NOT HERE. A 404 where the spec documents a 200 is the
# "not implemented" signal, and it is worth seeing: the Power BI routes pinned
# in KNOWN are exactly that, recorded rather than hidden.
AUTH_STATUSES = frozenset({401, 403})

# How many entries of an array to validate. The whole point of an array
# response is that its entries share a schema, so the tenth is evidence of
# little the first did not give -- and a 5,000-row list would dominate the run
# for nothing.
ARRAY_SAMPLE = 5


class Specs:
    """Microsoft's swagger, indexed by (method, path) with refs resolvable."""

    def __init__(self, roots=SPEC_ROOTS):
        self.roots = tuple(roots)
        self.docs = {}
        self.routes = []
        for root in self.roots:
            for swagger in sorted(root.rglob("swagger.json")):
                self._load_paths(swagger)

    def _load_paths(self, swagger):
            doc = self.load(swagger)
            # `or ""` rather than a default: the Power BI document has no
            # basePath at all and its paths already carry /v1.0/myorg, while
            # Fabric's factors /v1 out. A None default would concatenate onto
            # None and take the whole run down on the first path.
            base = doc.get("basePath") or ""
            for template, operations in doc.get("paths", {}).items():
                # See the module docstring: the query half of a path key names
                # a variant of the same route, not a different route.
                full = (base + template).split("?")[0]
                pattern = re.compile("^" + re.sub(r"\{[^}]+\}", "[^/]+", full) + "$")
                for method, operation in operations.items():
                    if method in ("parameters", "x-ms-examples"):
                        continue
                    self.routes.append(
                        (method.upper(), pattern, full, swagger,
                         operation.get("responses", {})))

    def load(self, path):
        path = path.resolve()
        if path not in self.docs:
            self.docs[path] = json.loads(path.read_text(encoding="utf-8"))
        return self.docs[path]

    def deref(self, node, origin, depth=0):
        """Resolve a $ref to (schema, the file it now lives in)."""
        if not isinstance(node, dict) or "$ref" not in node or depth > 12:
            return node, origin
        file_part, _, fragment = node["$ref"].partition("#")
        target = (origin.parent / file_part).resolve() if file_part else origin.resolve()
        if not target.is_file():
            return {}, origin
        current = self.load(target)
        for part in [p for p in fragment.split("/") if p]:
            current = current.get(part.replace("~1", "/"), {}) if isinstance(current, dict) else {}
        return self.deref(current, target, depth + 1)

    def match(self, method, path):
        for spec_method, pattern, template, origin, responses in self.routes:
            if spec_method == method and pattern.match(path):
                return template, origin, responses
        return None, None, None


def validate(value, schema, specs, origin, where, found):
    """Append a finding for every way `value` disagrees with `schema`."""
    schema, origin = specs.deref(schema, origin)
    if not isinstance(schema, dict) or value is None:
        return

    for sub in schema.get("allOf", []):
        validate(value, sub, specs, origin, where, found)

    # A bool is an int in Python, and reporting `true` as a bad integer would
    # be this checker's own type system leaking into its findings.
    expected = schema.get("type")
    mistyped = expected in TYPES and not isinstance(value, TYPES[expected])
    if mistyped and not (expected == "integer" and isinstance(value, bool)):
        found.append(f"{where}: type is {type(value).__name__}, spec says {expected}")
        return

    if isinstance(value, dict):
        for name in schema.get("required", []):
            if name not in value:
                found.append(f"{where}: MISSING required property '{name}'")
        for name, sub in schema.get("properties", {}).items():
            if name in value:
                validate(value[name], sub, specs, origin, f"{where}.{name}", found)
    elif isinstance(value, list):
        item = schema.get("items")
        if item:
            for index, entry in enumerate(value[:ARRAY_SAMPLE]):
                validate(entry, item, specs, origin, f"{where}[{index}]", found)

    choices = schema.get("enum")
    if choices and isinstance(value, (str, int)) and value not in choices:
        found.append(f"{where}: {value!r} is not in the spec's enum {choices}")


def read_recording(path):
    """One JSON object per line; a truncated final line is skipped, not fatal.

    A suite killed mid-run leaves a partial last line, and refusing to read the
    thousands of complete ones before it would turn a timeout into a second,
    unrelated failure.
    """
    entries = []
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            entries.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return entries


def conformance(entries, specs):
    """(findings, matched, unmatched-paths) for a recording."""
    found, matched, unmatched = [], 0, set()
    for entry in entries:
        template, origin, responses = specs.match(entry["method"], entry["path"])
        if not template:
            unmatched.add(f"{entry['method']} {entry['path']}")
            continue
        matched += 1
        # MEMBERSHIP, NOT TRUTHINESS. A spec documents a status with an empty
        # object -- `"429": {}` -- when it has no body to describe, and an
        # empty dict is falsy. Asking whether it is truthy reports a status the
        # spec explicitly lists as one it does not.
        if str(entry["status"]) in responses:
            documented = responses[str(entry["status"])] or {}
        else:
            documented = None
        if documented is None:
            # `default` covers the error shape for most routes; a status the
            # spec does not list at all is the finding -- unless it is an auth
            # outcome, which no route documents and every route can give.
            if "default" not in responses and entry["status"] not in AUTH_STATUSES:
                found.append(
                    f"{entry['method']} {template}: answered {entry['status']}, "
                    f"spec documents {sorted(responses)}")
            continue
        schema = documented.get("schema")
        if schema and entry.get("body") is not None:
            validate(entry["body"], schema, specs, origin,
                     f"{entry['method']} {template}", found)
    return found, matched, unmatched


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("recordings", nargs="+", type=pathlib.Path,
                        help="one or more JSONL files written by FABRIC_RECORD_RESPONSES")
    parser.add_argument("--strict", action="store_true",
                        help="exit non-zero on any finding")
    arguments = parser.parse_args()

    present = [r for r in arguments.recordings if r.is_file()]
    if not present:
        print(f"check_openapi_conformance: no recording at "
              f"{', '.join(str(r) for r in arguments.recordings)}. Set "
              f"FABRIC_RECORD_RESPONSES on the emulator and re-run the suite.",
              file=sys.stderr)
        return 1

    specs = Specs()
    # THE UNION, because each suite drives a slice of the surface and a
    # disagreement is a disagreement whichever suite happened to provoke it.
    entries = [e for r in present for e in read_recording(r)]
    if not entries:
        print("check_openapi_conformance: the recording is empty. A suite that "
              "records nothing proves nothing, so this is a failure rather "
              "than a pass.", file=sys.stderr)
        return 1

    raw, matched, unmatched = conformance(entries, specs)

    # ONE LINE PER DISTINCT DISAGREEMENT, with how many responses carried it.
    # A suite calls the same route many times -- `GET /v1/workspaces` runs 51
    # times in the fabric-cli driver -- so two real defects printed 102 lines
    # before this. That is the crying wolf the docstring above warns about,
    # produced by this checker's own reporting, and a reader who scrolls past
    # 102 identical lines is a reader who stops reading the output.
    occurrences = collections.Counter(raw)
    found = [f for f in occurrences if not is_known(f)]
    pinned = sum(n for f, n in occurrences.items() if is_known(f))

    print(f"check_openapi_conformance: {len(entries)} recorded responses from "
          f"{len(present)} recording(s), {matched} matched to one of "
          f"{len(specs.routes)} documented routes")
    if pinned:
        print(f"  {pinned} known disagreement(s) pinned in KNOWN, not counted")

    stale = stale_pins(raw)
    if stale:
        print(f"\n  {len(stale)} pinned disagreement(s) did not occur in this "
              f"run. Not a failure — a pin is silent when its route was not\n"
              f"  exercised — but if the defect is genuinely gone, delete the "
              f"entry rather than leaving history in the file:")
        for pin in stale:
            print(f"    - {pin}")
    if unmatched:
        # NOT a finding. The emulator serves surfaces Microsoft's swagger does
        # not cover (its own _emulator routes are excluded already, but Power
        # BI's /v1.0 surface lives in a different spec repository), and calling
        # those violations would make the real findings harder to see.
        print(f"  {len(unmatched)} path(s) with no documented route, not counted "
              f"as findings:")
        for path in sorted(unmatched)[:10]:
            print(f"    {path}")

    if not found:
        print("no conformance findings")
        return 0

    print(f"\ncheck_openapi_conformance: {len(found)} distinct finding(s) — a "
          f"response disagrees with Microsoft's published schema:\n")
    for finding in sorted(found):
        seen = occurrences[finding]
        tally = f"  ({seen} responses)" if seen > 1 else ""
        print(f"  - {finding}{tally}")
    print("\n  -> correct the response, or if the spec is wrong for this route, "
          "say so where the route is implemented and exclude it deliberately.")
    return 1 if arguments.strict else 0


if __name__ == "__main__":
    sys.exit(main())
