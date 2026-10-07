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

WHAT IS CHECKED, four classes, chosen because they are what a typed client
stumbles into:

  1. UNDOCUMENTED STATUS -- the emulator answered a code the spec does not list
     for that route.
  2. MISSING REQUIRED PROPERTY -- including inside arrays, which is where most
     of the surface lives.
  3. WRONG PRIMITIVE TYPE, and enum membership.
  4. WRONG STRING FORMAT -- `date-time`, `uuid`, `int32`, `int64`.

WHY THE FOURTH CLASS IS WORTH A PASS OF ITS OWN. `type: string` is satisfied by
any string at all, and the vendored specs carry 2,315 `format` annotations over
these four spellings alone -- 2,095 `uuid`, 120 `date-time`, 92 `int32`, 8
`int64`. Format is EXACTLY what a swagger-generated typed client enforces, at
the deserializer and before the caller's own code runs: `uuid` becomes a Guid,
`date-time` becomes a DateTime, `int32` becomes an int. So a zone-less
timestamp or a non-UUID id satisfies all three classes above and throws inside
the generated client -- the one contract class that can be green here and
broken there.

AND ITS LIMIT, which is why class 4 arrived carrying pins rather than fixes. A
`format` is Microsoft's claim about what a route renders, and a MEASUREMENT
against a real tenant outranks it: internal/api/items.go documents the
operation timestamps as observed zone-less with a trimmed fraction, which
`format: date-time` reads as RFC 3339 and therefore calls wrong. Every class-4
pin below cites evidence of that kind. The gate's value is the surfaces NOT yet
measured -- a NEW zone-less timestamp or non-UUID id there now fails.

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
# disagreement is known and ADJUDICATED -- the reason string is the
# adjudication, and a pin without one fails its own meta-test.
#
# THE LIST DOES NOT ONLY SHRINK, and an earlier draft of this comment claimed
# it did. It shrinks when a defect is fixed and it GROWS when a new class of
# oracle starts looking: adding the `format` pass surfaced four disagreement
# classes at once, of which one was a real defect and is fixed and three are
# pinned below with their evidence. A gate that could only ever shrink its
# exclusion list is a gate nobody can extend.
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
    # A VALUE THE SPEC'S ENUM DOES NOT HAVE, emitted deliberately. Found the day
    # e2e/eventstream started recording: its five Reflex-triggered pipeline runs
    # report `invokeType: "EventTriggered"`, and Microsoft's ItemJobInstance
    # enumerates only Scheduled and Manual.
    #
    # PINNED RATHER THAN FIXED, and the reason is not that the spec is wrong.
    # The value is the emulator's own distinction between a job an Activator
    # trigger started and one a person or a schedule did, five e2e assertions
    # depend on it, and reporting Manual would erase exactly the fact that
    # makes a trigger observable. What real Fabric reports for an
    # Activator-launched run is NOT SOMETHING THIS REPOSITORY CAN SETTLE
    # without a tenant: the trigger binding has no public REST (docs/parity.md
    # names it emulator-native), so there is no documented request whose answer
    # could be compared. Recorded as an open question, not as a claim.
    "invokeType: 'EventTriggered' is not in the spec's enum ['Scheduled', 'Manual']":
        "Reflex-triggered runs report EventTriggered, outside the documented "
        "enum. Deliberate; unsettled without a tenant.",
    # THE IMPORTS SURFACE, all nine operations, refused.
    #
    # THIS IS THE TWO GATES PULLING AGAINST EACH OTHER, and the pull is by
    # design rather than a mistake in either. check_surface_ledger rewards
    # ASKING: an operation nothing serves and nothing has ever asked about is
    # SILENT, which is the state where an integration finds out at runtime, and
    # driving it turns it into a measured REFUSAL. But refusing IS a
    # disagreement with a spec that documents 200, so every operation moved out
    # of silent arrives here as a finding. Nine appeared the moment
    # e2e/semantic-model started asking.
    #
    # KNOWN is where that is reconciled, and the reconciliation is the point:
    # the gap is now written down twice, as a refusal with evidence and as a
    # pinned disagreement with a reason, instead of being invisible to both.
    # This emulator publishes through the Fabric item-definition API; the
    # .pbix upload path is not implemented and is graded in docs/parity.md.
    "GET /v1.0/myorg/imports: answered 404":
        "the Imports surface is not served; publishing goes through the "
        "Fabric item-definition API. See e2e/semantic-model, which asserts "
        "all nine refusals so the gap cannot close or widen unnoticed.",
    "GET /v1.0/myorg/imports/{importId}: answered 404": "as above.",
    "GET /v1.0/myorg/admin/imports: answered 404": "as above.",
    "GET /v1.0/myorg/groups/{groupId}/imports: answered 404": "as above.",
    "GET /v1.0/myorg/groups/{groupId}/imports/{importId}: answered 404": "as above.",
    "POST /v1.0/myorg/imports: answered 404": "as above.",
    "POST /v1.0/myorg/imports/createTemporaryUploadLocation: answered 404": "as above.",
    "POST /v1.0/myorg/groups/{groupId}/imports: answered 404": "as above.",
    "POST /v1.0/myorg/groups/{groupId}/imports/createTemporaryUploadLocation: answered 404":
        "as above.",

    # REFUSALS THAT ARE THE HONEST ANSWER, where answering the documented
    # status would be a well-formed lie. Both are reached only by
    # e2e/semantic-model, which is the only suite that publishes a real
    # SemanticModel, and both are asserted there rather than merely tolerated.
    "GET /v1.0/myorg/datasets: answered 404":
        "the MY-WORKSPACE list. This emulator models workspaces and has no "
        "personal workspace, so there is no set for this route to describe. "
        "The spec documents a 200, and `{\"value\": []}` would satisfy it -- "
        "and would be indistinguishable to a caller from a personal workspace "
        "that happens to be empty, which is the reading that costs someone an "
        "afternoon believing their model failed to publish. The 404 names the "
        "reason instead.",
    "POST /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/refreshes: answered 400":
        "refresh of an INLINE-DATA model, whose rows are a data.json "
        "definition part with nothing behind them. The spec documents 202 "
        "Accepted; accepting here would tell a caller their numbers had been "
        "brought up to date when nothing was re-read. The same predicate "
        "drives isRefreshable=false on the dataset, so a client that trusts "
        "the flag is never then contradicted. A Direct Lake model takes the "
        "positive branch and is witnessed in e2e/data-science-loop.",
    # ---- class 4, WRONG STRING FORMAT: three pins, each with its evidence ----
    #
    # The `format` pass arrived with four disagreement classes in this tree.
    # ONE WAS A REAL DEFECT AND IS FIXED rather than pinned: GET /v1/admin/items
    # rendered `lastUpdatedDate` zone-less with nothing defending it, and now
    # sends RFC 3339 (internal/api/adminitems.go). The three below are pinned,
    # and the reason is NOT laziness in each case -- it is that a MEASUREMENT
    # outranks a `format`, or that the value is not the emulator's to choose.

    # TENANT-MEASURED, and the spec is wrong for this route. internal/api/items.go
    # defines `fabricOperationTime` from samples taken against a real tenant on
    # 2026-08-11: ISO 8601, fraction trimmed, and NO `Z`. Two samples minutes
    # apart carried 7 and 6 fractional digits, which is what settles the trimming
    # rule. `format: date-time` reads as RFC 3339 and would have this emulator
    # send a shape the tenant does not. Fixing it would mean making the emulator
    # LESS faithful than the thing it emulates, so the finding is the honest
    # record and the pin names where the measurement lives.
    "GET /v1/operations/{operationId}.createdTimeUtc: is not RFC 3339":
        "the operation timestamps are zone-less because a real tenant was "
        "MEASURED sending them that way -- see fabricOperationTime in "
        "internal/api/items.go, which cites the samples. The spec's "
        "`format: date-time` is wrong for this route; matching it would make "
        "the emulator diverge from the tenant.",
    "GET /v1/operations/{operationId}.lastUpdatedTimeUtc: is not RFC 3339":
        "as above, same rendering rule and same measurement.",

    # NOT THE EMULATOR'S VALUE TO CHOOSE. A schedule's startDateTime and
    # endDateTime are supplied by the CALLER and echoed back verbatim, so this
    # finding describes a request body, not a rendering decision -- and the two
    # suites in this tree send two different spellings, both accepted:
    # e2e/az-rest/driver.py formats with a `Z` and produces no finding, while
    # e2e/fabric-cli/driver.sh drives `fab job run-sch --start
    # 2026-01-01T09:00:00`, and MICROSOFT'S OWN PUBLISHED CLI builds the body
    # from that and sends it zone-less. A gate demanding `Z` here would be
    # demanding the emulator refuse what fabric-cli sends.
    #
    # AND THE SPEC CONTRADICTS ITSELF HERE, which is the deciding half.
    # common/job_scheduling.json makes `localTimeZoneId` a REQUIRED sibling of
    # these two fields -- the timestamps are local to a named zone, which is why
    # internal/schedule/schedule.go's parseLocalTime reads them in that zone and
    # says so. A local time carrying a mandatory UTC offset is incoherent, and
    # the property's own description ("in UTC, using the YYYY-MM-DDTHH:mm:ssZ
    # format") disagrees with the schema it sits in. Unsettled in Microsoft's
    # favour would break a documented client; recorded instead.
    "configuration.startDateTime: is not RFC 3339":
        "caller-supplied and echoed. Microsoft's own fabric-cli sends these "
        "zone-less, and the schema requires a separate `localTimeZoneId`, so "
        "the timestamps are local to a named zone rather than UTC. The "
        "spec's `format: date-time` contradicts its own required sibling "
        "field. See internal/schedule/schedule.go parseLocalTime.",
    "configuration.endDateTime: is not RFC 3339":
        "as above, the other half of the same schedule window.",

    # CALLER-SUPPLIED AND ECHOED, exactly like the tenantId pin below, and the
    # same resolution for the same reason. e2e/az-rest/driver.py assigns a role
    # to `{"id": "az-rest-viewer", "type": "User"}` and the emulator stores and
    # returns the principal it was handed. The spec marks `principal.id`
    # `format: uuid`, so the faithful fix is to REFUSE a non-UUID principal at
    # authoring time -- which is a behaviour change across several fixtures and
    # deserves its own change rather than being folded into the one that made it
    # visible.
    ".principal.id: is not a canonical 8-4-4-4-12 UUID":
        "a test fixture's non-UUID principal id, stored and echoed rather than "
        "invented. The faithful fix is refusing it at authoring time, which is "
        "a behaviour change owed its own PR -- the same call already made for "
        "microsoftEntraMembers tenantId below.",

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

# RFC 3339, which is what `format: date-time` means and what a generated
# client's DateTime deserializer accepts. The OFFSET IS MANDATORY in RFC 3339 --
# either `Z` or +/-hh:mm -- and that is the whole substance of this pattern:
# every class-4 finding in this tree is a timestamp that is otherwise perfectly
# well formed and carries no zone, so a pattern with an optional offset would
# find nothing at all and read as a clean tree.
#
# The fractional part is optional and unbounded in width, deliberately: a
# tenant was measured sending 6 digits and 7 digits minutes apart
# (internal/api/items.go), so a fixed width here would invent a disagreement.
RFC3339 = re.compile(
    r"^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$")

# The canonical 8-4-4-4-12 spelling. Case-insensitive, and the braced and
# urn: spellings are NOT accepted: `format: uuid` in a response is what becomes
# a Guid in a typed client, and the wire form Fabric uses everywhere else in
# these same specs is the bare canonical one.
UUID = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
                  r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

# Inclusive bounds of the two signed integer widths a typed client deserializes
# into. An int32 field answered 2**31 is not a large number to a C# client; it
# is an OverflowException.
INT_RANGES = {
    "int32": (-2**31, 2**31 - 1),
    "int64": (-2**63, 2**63 - 1),
}


def format_violation(value, fmt):
    """Why `value` is not a valid `fmt`, or None if it is fine.

    FORMATS NOT LISTED HERE RETURN None RATHER THAN RAISING, and that is a
    deliberate choice about what this gate claims. The specs also carry `uri`,
    `double`, `binary`, `duration` and a handful of one-off spellings, and a
    checker that guessed at those would manufacture findings on surfaces nobody
    has measured. Four formats, each with an unambiguous wire grammar and each
    enforced by a generated client's deserializer.

    EACH BRANCH RE-CHECKS THE PYTHON TYPE, which looks redundant beside the
    caller's own type check and is not. A schema may carry a `format` and NO
    `type` -- the specs do this -- and then nothing upstream has established
    that a `uuid` field holds a string at all. Matching a regex against an int
    raises, and a gate that dies on one odd schema stops checking every
    response after it.
    """
    if fmt == "date-time":
        if isinstance(value, str) and not RFC3339.match(value):
            return ("is not RFC 3339 (`format: date-time`); a generated "
                    "client's DateTime deserializer requires a `Z` or "
                    "+/-hh:mm offset")
    elif fmt == "uuid":
        if isinstance(value, str) and not UUID.match(value):
            return "is not a canonical 8-4-4-4-12 UUID (`format: uuid`)"
    elif fmt in INT_RANGES:
        low, high = INT_RANGES[fmt]
        # A bool is an int in Python, the same leak the type check guards
        # against: `true` is not an out-of-range int32.
        if isinstance(value, int) and not isinstance(value, bool) \
                and not low <= value <= high:
            return f"is outside the range of an {fmt} (`format: {fmt}`)"
    return None


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


def validate(value, schema, specs, origin, where, found, samples=None):
    """Append a finding for every way `value` disagrees with `schema`.

    `samples` is an optional dict that collects the FIRST value seen for each
    finding text. It exists because the finding text deliberately does NOT
    name the offending value -- see note() -- and a reader still needs one
    concrete example to go and look at.
    """
    schema, origin = specs.deref(schema, origin)
    if not isinstance(schema, dict) or value is None:
        return

    def note(finding, value=None):
        """Record a finding, and the value that provoked it, SEPARATELY.

        THE VALUE MUST NOT GO IN THE FINDING TEXT, and this was measured. KNOWN
        pins match by substring and main() dedups by finding text, so a
        timestamp in the text turns ONE rendering rule into one line per
        distinct instant: the local run over these recordings produces 17
        different `createdTimeUtc` values from a single defect, which is
        unpinnable and is exactly the crying-wolf the module docstring warns
        about. So the text names the route, the field path and the expected
        format, and the example lives here.
        """
        found.append(finding)
        if samples is not None and value is not None:
            samples.setdefault(finding, value)

    for sub in schema.get("allOf", []):
        validate(value, sub, specs, origin, where, found, samples)

    # A bool is an int in Python, and reporting `true` as a bad integer would
    # be this checker's own type system leaking into its findings.
    expected = schema.get("type")
    mistyped = expected in TYPES and not isinstance(value, TYPES[expected])
    if mistyped and not (expected == "integer" and isinstance(value, bool)):
        note(f"{where}: type is {type(value).__name__}, spec says {expected}")
        return

    # FORMAT, and only once the TYPE already agreed -- a mistyped value
    # returned above. Reporting a format on top of a type mismatch would be one
    # defect wearing two hats, and of the two the type is the more useful
    # finding: `format: uuid` on an integer is not a malformed UUID, it is not
    # a string.
    fmt = schema.get("format")
    if fmt:
        why = format_violation(value, fmt)
        if why:
            note(f"{where}: {why}", value)

    if isinstance(value, dict):
        for name in schema.get("required", []):
            if name not in value:
                found.append(f"{where}: MISSING required property '{name}'")
        for name, sub in schema.get("properties", {}).items():
            if name in value:
                validate(value[name], sub, specs, origin, f"{where}.{name}",
                         found, samples)
    elif isinstance(value, list):
        item = schema.get("items")
        if item:
            for index, entry in enumerate(value[:ARRAY_SAMPLE]):
                validate(entry, item, specs, origin, f"{where}[{index}]",
                         found, samples)

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


def conformance(entries, specs, samples=None):
    """(findings, matched, unmatched-paths) for a recording.

    `samples` is an optional dict filled in place with one example value per
    distinct finding. An OUT-PARAMETER rather than a fourth element of the
    return, deliberately: this tuple is unpacked in several tests and the shape
    is the kind of thing a caller elsewhere would silently mis-destructure.
    """
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
                     f"{entry['method']} {template}", found, samples)
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

    samples = {}
    raw, matched, unmatched = conformance(entries, specs, samples)

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
        # The example, on its own line, because the finding text cannot carry
        # it without breaking both the dedup and the pins. A format
        # disagreement is close to unactionable without one: "not RFC 3339"
        # does not say whether the zone is missing or the whole string is junk.
        if finding in samples:
            print(f"      e.g. {samples[finding]!r}")
    print("\n  -> correct the response, or if the spec is wrong for this route, "
          "say so where the route is implemented and exclude it deliberately.")
    return 1 if arguments.strict else 0


if __name__ == "__main__":
    sys.exit(main())
