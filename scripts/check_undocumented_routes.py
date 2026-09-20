#!/usr/bin/env python3
r"""Routes this emulator answers that no published spec documents.

THE FOURTH DIRECTION, and the only one nothing measured. Three gates already
read the same two artifacts from three angles:

  * check_openapi_conformance validates the BODIES and STATUSES a recording
    holds against Microsoft's vendored swagger.
  * check_route_coverage ratchets which REGISTERED routes have ever seen
    recorded traffic.
  * check_surface_ledger classifies every DOCUMENTED operation as served,
    refused or silent.

All three point the same way: from what Microsoft documents, or from what we
serve, towards evidence. None of them asks the inverse of the ledger's
denominator -- WHICH ROUTES DOES THIS EMULATOR REGISTER THAT NO PUBLISHED SPEC
DESCRIBES.

WHY THAT DIRECTION IS THE DANGEROUS ONE. A documented operation we do not
serve fails here and works in Fabric: annoying, and discovered on the first
call. A route we serve that Fabric does not is the opposite -- a script is
written against the emulator, passes, ships, and 404s in production. The
emulator being MORE PERMISSIVE than the thing it emulates is the worst
direction for a fidelity bug to point, and until this landed nothing counted
it.

Measured when this landed: 613 registered in-scope routes, 1002 documented
operations, 191 routes matching no documented (method, shape) -- 148 of them
typed-collection spellings of a documented `/items` operation, leaving 43 that
are not, every one of which is adjudicated below or in the baseline.

FOUR STATES, and a route is exactly one of them:

  DOCUMENTED   -- method plus parameter-name-erased shape matches a documented
                  operation. `{wid}` and `{workspaceId}` are the same route.
  ALIAS        -- a typed-collection spelling whose `/items` equivalent is
                  documented. `GET .../notebooks/{iid}` is the generic
                  `GET .../items/{itemId}` with the type forced, so crediting
                  it is not a loophole; it is the same operation.
  NATIVE       -- declared in NATIVE below, by pattern, WITH A WRITTEN REASON.
                  A surface this emulator answers on purpose that no published
                  REST spec describes: either emulator-native, or a real
                  Fabric surface whose protocol is not REST at all.
  UNDOCUMENTED -- everything else, ratcheted against
                  docs/undocumented-routes.json in BOTH directions. The set
                  growing fails (a new invented surface arrived without a
                  decision) and a route leaving it fails too, so the file can
                  never record a lie about the tree.

WHY THIS ONE RUNS IN `make check`. Its siblings need a recording, so they live
in the aggregate CI job behind eleven e2e suites. This reads the Go source and
the vendored swagger and nothing else: offline, deterministic, and answerable
before a push.

Usage:
    check_undocumented_routes.py              report, exit 0
    check_undocumented_routes.py --strict     exit non-zero on drift
    check_undocumented_routes.py --update     rewrite the baseline
"""
import argparse
import collections
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASELINE = ROOT / "docs" / "undocumented-routes.json"

sys.path.insert(0, str(ROOT / "scripts"))

import check_openapi_conformance as conf  # noqa: E402
import check_route_coverage as cov  # noqa: E402

# Module-level so the tests can point this at a miniature swagger tree. Taken
# from the conformance checker rather than restated: two spec trees is a fact
# about what this repository vendors, and two copies of it would be two places
# for it to go stale.
SPEC_ROOTS = conf.SPEC_ROOTS


def _shape(template):
    """A path template with its parameter NAMES removed.

    MUST AGREE WITH check_surface_ledger._shape, and a test asserts that it
    does. The two gates are the two halves of one comparison -- that one reads
    spec-to-tree and this one tree-to-spec -- so a shape rule that drifted
    between them would let a route be `served` over there and `undocumented`
    here, which is not a finding, it is an inconsistency wearing one.
    """
    return re.sub(r"\{[^}]+\}", "{}", template).rstrip("/")


def documented_shapes(roots=None):
    """Every documented in-scope operation, as method -> set of shapes."""
    specs = conf.Specs(roots=SPEC_ROOTS if roots is None else roots)
    shapes = collections.defaultdict(set)
    for method, _pattern, template, _origin, _responses in specs.routes:
        if not template.startswith(cov.IN_SCOPE):
            continue
        shapes[method].add(_shape(template))
    return shapes


def alias_collections():
    """The path segments that are a TYPED COLLECTION, read out of the source.

    DERIVED, NOT GUESSED, and the difference was measured. A looser rule --
    "swap ANY segment for `items` and see if that is documented" -- credits
    `POST /v1/workspaces/{wid}/lineage` against the documented
    `POST /v1/workspaces/{workspaceId}/items`, because `lineage` is a segment
    and the substitution happens to land. Two emulator-native routes would
    have read as documented Fabric operations, which is precisely the claim
    this gate exists to refuse.

    So only a real collection name may be swapped. The names come from
    internal/api/definitions.go's typedCollections map (via the same reader
    the coverage ratchet uses) and the initial-capital variant comes from
    collectionSpellings() in that file -- ASP.NET matches path segments
    case-insensitively and Go's ServeMux does not, so the emulator registers
    both and both must be recognised here.
    """
    names = set()
    for key in cov.PARAMETERISED:
        for name in cov.alias_spellings(key):
            names.add(name)
    return names


def _alias_of(method, template, names, shapes):
    """The documented `/items` shape this typed spelling stands for, or None."""
    parts = template.split("/")
    for index, part in enumerate(parts):
        if part not in names:
            continue
        generic = "/".join([*parts[:index], "items", *parts[index + 1:]])
        if _shape(generic) in shapes.get(method, ()):
            return _shape(generic)
    return None


# Surfaces this emulator answers on purpose that no published REST spec
# describes. EACH ENTRY IS A REGEX OVER THE WHOLE `METHOD /path` LABEL AND A
# REASON, and the reason is the point: a bare allowlist would make "we decided
# this" and "nobody looked" identical, which is the failure mode every gate in
# this directory is written against.
#
# A pattern that matches NOTHING is reported, the way check_openapi_conformance
# reports a stale KNOWN pin: a declaration that outlives the route it excused
# is a claim about a tree that no longer exists.
NATIVE = {
    r"^(GET|POST|DELETE) /v1/mcp/core$":
        "Fabric's Core MCP endpoint. Real, and not a REST operation: it speaks "
        "JSON-RPC over MCP Streamable HTTP, so no swagger can describe it and "
        "none does. Witnessed by e2e/mcp-core with Microsoft's own `mcp` "
        "client.",
    r"^(GET|PUT|DELETE) /v1/workspaces/\{wid\}/items/\{iid\}/_emulator/access":
        "Emulator-native by its own name: the `_emulator/` segment is this "
        "repository's reserved prefix for levers real Fabric has no API for. "
        "Item-level access is Purview/OneLake security in Fabric, which cannot "
        "be attached offline.",
    r"^(GET|PUT) /v1/workspaces/\{wid\}/sqlEndpoints/\{epid\}/_emulator/dataAccessMode$":
        "Emulator-native by its own name. Fabric switches a SQL analytics "
        "endpoint's data access mode in the portal and documents no API for it "
        "(OneLake security for SQL analytics endpoints). Bearer-authenticated "
        "and Admin/Member-gated, so it is not under the unauthenticated "
        "/_emulator/ control prefix. Witnessed by internal/server/dataaccessmode_test.go.",
    r"/livyapi/":
        "The Livy REST protocol, which is Apache's and not Microsoft's. Fabric "
        "exposes a Livy endpoint per lakehouse and the fabric-rest-api-specs "
        "swagger does not describe it -- there is nothing to match against. "
        "Driven by e2e/livy and Microsoft's own dbt-fabricspark adapter.",
    r"/materializedlakeviews":
        "Materialized Lake Views. The emulator serves create/list/get/delete "
        "and refresh; no vendored swagger documents the surface, so there is "
        "no published shape to hold these to. Driven by e2e/sail.",
    r"^(GET|POST|PATCH|DELETE) /v1/workspaces/\{wid\}/reflexes/\{iid\}/triggers":
        "Activator trigger bindings. docs/parity.md already names the trigger "
        "binding emulator-native: it has no public REST, which is why "
        "check_openapi_conformance has to pin the `EventTriggered` invokeType "
        "rather than fix it.",
    r"/eventstreams/\{iid\}/(destinations|operators)$":
        "Eventstream topology. The item itself is documented and served; its "
        "destination and operator graph is the emulator's own model, built so "
        "a notebook-API Eventstream can be inspected at all. Driven by "
        "e2e/eventstream.",
    r"^(GET|POST) /v1(/workspaces/\{wid\})?/eventstreams/\{iid\}/sources/\{did\}":
        "The Eventstream source runtime -- reading a source and pushing events "
        "into it. Emulator-native for the same reason as the topology above, "
        "and the pair the Sail and JVM engines split between them.",
    r"/jobs/instances/\{jid\}/(notebookRun|notebookRunResult|sparkJobRun|sparkJobRunResult)$":
        "The per-cell and per-Spark-job run DETAIL, and the agent's callback "
        "that reports it. Fabric surfaces this in its portal, not over REST; "
        "here it is how a driven notebook's real output gets back to the "
        "control plane at all. internal/api/jobs.go points clients at it from "
        "the documented job-instance body.",
    r"/jobs/instances/\{jid\}/queryactivityruns$":
        "Data Factory pipeline activity runs for one job instance. Not in "
        "either vendored spec tree; the shape follows ADF's own "
        "queryActivityRuns and is asserted by e2e/pipeline-activities.",
    r"/jobs/instances/\{jid\}/webhookcallbacks/\{token\}$":
        "The WebHook activity's callBackUri target, shaped after ADF's. "
        "Emulator-native and deliberately unauthenticated -- the token in the "
        "path IS the credential, because an external receiver holds no Fabric "
        "token (internal/api/api.go says so at the registration).",
}

_NATIVE = {re.compile(pattern): reason for pattern, reason in NATIVE.items()}


def _native_reason(label):
    for pattern, reason in _NATIVE.items():
        if pattern.search(label):
            return reason
    return None


def stale_native(labels):
    """Declared patterns that no registered route matches any more."""
    return sorted(pattern.pattern for pattern in _NATIVE
                  if not any(pattern.search(label) for label in labels))


def classify():
    """Every registered in-scope route, in exactly one of the four states."""
    shapes = documented_shapes()
    names = alias_collections()
    states = {}
    for label in cov.registered():
        method, _, template = label.partition(" ")
        if _shape(template) in shapes.get(method, ()):
            states[label] = "documented"
        elif _alias_of(method, template, names, shapes):
            states[label] = "alias"
        elif _native_reason(label):
            states[label] = "native"
        else:
            states[label] = "undocumented"
    return states


# What is known about each route left in the baseline, written into the file so
# the diff a reviewer reads is not six bare strings. THESE ARE NOT EXCUSES:
# every one of them is a place where this emulator answers a URL real Fabric
# may not, and the note says what was checked.
NOTES = {
    "GET /v1/admin/labels":
        "The sensitivity-label taxonomy is the emulator's own -- real Fabric "
        "gets labels and their order from Purview, which cannot be attached "
        "offline. internal/api/labels.go calls this read 'an emulator "
        "affordance, not a Fabric API' at the registration. The two label "
        "operations Microsoft DOES document (bulkSetLabels, bulkRemoveLabels) "
        "are served and classify as documented.",
    "GET /v1/workspaces/{wid}/lineage":
        "Workspace lineage as a REST collection is emulator-native: Fabric "
        "shows lineage in its portal and publishes no operation for it. Note "
        "this is the route the loose alias rule falsely credited against the "
        "documented POST .../items -- see alias_collections().",
    "POST /v1/workspaces/{wid}/lineage":
        "The write half of the same emulator-native graph: an engine that is "
        "not a queued notebook run reports what it moved, so an interactive "
        "step reaches the graph too (internal/api/reportlineage.go).",
    "POST /v1/workspaces/{wid}/items/{iid}/jobs/instances":
        "A SPELLING DISAGREEMENT, not an invented surface, and left here "
        "rather than matched away because it is exactly what this gate is for. "
        "The operation is documented -- JobScheduler_RunOnDemandItemJob -- but "
        "the vendored swagger spells it "
        "`/items/{itemId}/jobs/{jobType}/instances` with jobType as a PATH "
        "parameter, while this emulator (and internal/api/copyjob.go's "
        "citation of Microsoft's REST reference, and the traffic Microsoft's "
        "own `fab` CLI records against it) uses `/jobs/instances?jobType=`. "
        "Teaching the matcher to fold the two would hide the divergence; "
        "recording it keeps it reviewable.",
    "POST /v1/workspaces/{wid}/mirroredDatabases/{iid}/refreshMirror":
        "Checked against the swagger first: Microsoft documents startMirroring, "
        "stopMirroring, getMirroringStatus and getTablesMirroringStatus on "
        "mirroredDatabases, and no refreshMirror. This is the emulator's "
        "explicit on-demand snapshot standing in for the continuous "
        "replication it cannot run offline (internal/api/mirror.go).",
    "POST /v1/workspaces/{wid}/sqlDatabases/{iid}/refreshMirror":
        "As above, on the sqlDatabases spelling: the swagger documents "
        "startMirroring and stopMirroring there and no refreshMirror.",
}


def read_baseline():
    if not BASELINE.is_file():
        return None
    return json.loads(BASELINE.read_text(encoding="utf-8"))


def write_baseline(states):
    counts = collections.Counter(states.values())
    undocumented = sorted(k for k, v in states.items() if v == "undocumented")
    BASELINE.write_text(json.dumps({
        "_comment": [
            "Routes this emulator registers that no published spec documents.",
            "Written by scripts/check_undocumented_routes.py --update; do not",
            "hand-edit. The emulator answering a URL real Fabric does not is",
            "the worst direction for a fidelity bug to point -- a script",
            "written here passes and 404s in production -- so this set is",
            "ratcheted in BOTH directions: it may not grow without a decision,",
            "and a route that leaves it must leave the file too. `notes` says",
            "what was checked for each one. Surfaces that are emulator-native",
            "on purpose are declared with their reason in NATIVE, in the",
            "script, and are counted here but not listed.",
        ],
        "registered": len(states),
        "documented": counts["documented"],
        "alias": counts["alias"],
        "native": counts["native"],
        "undocumented": counts["undocumented"],
        "notes": {route: NOTES[route] for route in undocumented if route in NOTES},
        "undocumentedRoutes": undocumented,
    }, indent=2) + "\n", encoding="utf-8")


def main_for_test(strict=False, update=False):
    """main() without argparse, so the gate itself can be tested.

    The gate is the half that rots unnoticed: a ratchet that never fires and a
    healthy tree read the same from outside.
    """
    return _run(strict, update)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--strict", action="store_true",
                        help="exit non-zero when the baseline and the tree disagree")
    parser.add_argument("--update", action="store_true",
                        help="rewrite the baseline from this run")
    args = parser.parse_args()
    return _run(args.strict, args.update)


def _run(strict, update):
    # THE SAME GUARD THE COVERAGE RATCHET RUNS FIRST, and for a stronger
    # reason here. That gate's denominator shrinking makes it demand less; this
    # one's shrinking makes it REPORT LESS, so a registration form the parser
    # stops understanding is an invented surface silently leaving the set.
    surprises = cov.unparsed_registrations()
    if surprises:
        print("check_undocumented_routes: a route registration could not be parsed, "
              "so it cannot be classified at all:", file=sys.stderr)
        for rel, found, allowed in surprises:
            print(f"    {rel}: {found} unresolved HandleFunc call(s), "
                  f"{allowed} allowed in check_route_coverage.UNPARSED_OK",
                  file=sys.stderr)
        return 1

    states = classify()
    counts = collections.Counter(states.values())
    undocumented = {k for k, v in states.items() if v == "undocumented"}
    print(f"check_undocumented_routes: {len(states)} registered in-scope routes — "
          f"{counts['documented']} documented, {counts['alias']} typed alias, "
          f"{counts['native']} declared emulator-native, "
          f"{counts['undocumented']} undocumented")

    if update:
        write_baseline(states)
        # Repo-relative when it is in the repo. `Path.relative_to` RAISES
        # rather than degrading, and the tests point BASELINE at a temp
        # directory -- so the reporter crashed on exactly the path the tests
        # exist to cover. check_route_coverage._rel carries the same scar.
        try:
            where = BASELINE.relative_to(ROOT)
        except ValueError:
            where = BASELINE
        print(f"  baseline written to {where}")
        return 0

    problems = []
    stale = stale_native(states)
    for pattern in stale:
        problems.append(
            f"NATIVE declares {pattern!r} and nothing registers a route it "
            f"matches — delete the declaration or restore the route")

    base = read_baseline()
    if base is None:
        print(f"check_undocumented_routes: no baseline at {BASELINE}. "
              f"Create one with --update.", file=sys.stderr)
        return 1

    pinned = set(base.get("undocumentedRoutes", ()))
    for label in sorted(undocumented - pinned):
        problems.append(
            f"undocumented and not in the baseline: {label}")
    for label in sorted(pinned - undocumented):
        problems.append(
            f"the baseline still calls this undocumented: {label}")

    if not problems:
        print(f"  {len(pinned)} undocumented route(s), as recorded")
        return 0

    print()
    print(f"{len(problems)} disagreement(s) between the tree and the baseline:")
    for problem in problems[:40]:
        print(f"  - {problem}")
    if len(problems) > 40:
        print(f"  … and {len(problems) - 40} more")
    print("\n  -> a NEW undocumented route is a surface no published spec "
          "describes: a client that binds to it binds to a fiction real Fabric "
          "404s. Either it is documented and the matcher is wrong, or it is "
          "deliberate and belongs in NATIVE with a reason, or it is a real "
          "divergence — add it with --update and a note saying what you "
          "checked. A route that LEFT the set must leave the file too.")
    return 1 if strict else 0


if __name__ == "__main__":
    sys.exit(main())
