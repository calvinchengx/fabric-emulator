#!/usr/bin/env python3
r"""A knob or route a released binary accepted cannot vanish unremarked.

WHY THIS EXISTS. Nothing in this tree asserts that the surface an external
user BINDS TO survives a refactor. Four gates read the HTTP surface already --
conformance validates the bodies a recording holds, the route ratchet asks
which registered routes have seen traffic, the surface ledger classifies every
documented operation, and check_undocumented_routes asks which routes we serve
that no spec describes -- and not one of them can say that a route which
existed yesterday still exists today. check_route_coverage comes closest and
stores `registered` as an INTEGER, so deleting a served route reads in review
as `174 -> 173` and never names itself.

Below the HTTP layer nothing looked at all. The emulator's CLI flags, its two
subcommands and its FABRIC_* environment knobs are a contract every compose
file, CI job, example and README snippet in this repository is written against,
and a rename would break all of them while every check here stayed green.

AND ONE SURFACE IS NOT SERVED OVER A WIRE AT ALL: the exported Go API of
`pkg/`. That directory exists for exactly one reason and its own package doc
says so -- "WHY `pkg/` AND NOT `internal/`. Go's internal rule would make this
unimportable by any other module" -- so the exported identifiers of
`pkg/onelakesec` are a COMPILE-TIME contract an importing module binds to, and
docs/release-notes/v0.37.0.md advertises `DecisionRule.Constraints` as API that
landed. Renaming `Effective`, dropping `AccessEntry.Why` or deleting an
exported struct field breaks that importer at BUILD time, and nothing in this
tree watched it: the only script that reads `pkg/` at all is
check_script_test_coverage, which counts that each package has a `_test.go`.
The glob is `pkg/*`, so a package added tomorrow is covered the day it lands.

Measured when this landed: 2 subcommands, 28 CLI flags, 37 FABRIC_* variables,
and 1056 registered in-scope routes (174 after the route ratchet's family
collapse, which this gate deliberately does NOT apply -- see below). Measured
when the Go API joined as the fifth surface: 51 exported ids, every one in
`pkg/onelakesec` -- 23 package-level funcs, types and consts, 4 methods, and
24 struct fields.

THE LEDGER IS ASYMMETRIC ON PURPOSE, and the asymmetry is the whole design:

  AN ADDITION is a STALE LEDGER. It fails, you run --update, and the diff is
  the review of what surface this repository has newly promised to keep.
  Adding a knob is cheap; promising one silently is what this prevents.

  A REMOVAL is a BREAKING CHANGE. It fails until someone writes the id into
  `removed` with the release it went away in and why. There is no --update
  path for a removal, deliberately: regenerating must not be able to launder
  one, or the gate degrades into a changelog that agrees with itself.

  A RESURRECTION fails too. An id listed in `removed` that has come back is a
  stale exemption, and a stale exemption goes on excusing a surface nobody is
  holding -- the reason every other ledger in this directory is checked in
  BOTH directions.

WHY ROUTES ARE NOT COLLAPSED HERE. check_route_coverage folds the 51 typed
item collections into one `{collection}` row because its question is "has this
HANDLER been exercised", and 909 rows of one handler is a baseline nobody
reads. This gate's question is different: `GET .../notebooks/{iid}` is a URL a
client wrote down, and it can disappear on its own while `.../warehouses`
survives. So every spelling is its own row -- the same expansion
check_surface_ledger makes, for the same reason, over the same registrations.

WHAT THIS IS NOT.

  * It says NOTHING about whether a surface still BEHAVES the same. A flag
    that still parses and now means the opposite passes here, and so does an
    exported Go function that keeps its name and returns the other answer.
    That is conformance and the e2e suites: a different gate on a different
    input.
  * A GO SIGNATURE OR TYPE CHANGE is out of scope on this pass. `Effective`
    gaining a parameter, or `AccessEntry.Rows` becoming a `[]string`, breaks
    an importer exactly as a deletion does, and this gate will not say so: a
    signature is a structured thing, reading it off source text is where the
    false positives live, and a surface recorded as a NAME is already the
    difference between a rename that nobody notices and one that fails. Doing
    it properly wants `go/types`, which is a Go program rather than a line in
    this file -- a deliberate increment, named so it is a gap and not an
    oversight.
  * `internal/` is deliberately out of scope, and for once the language does
    the work: Go's own internal rule makes those packages unimportable by any
    other module, so there is no downstream importer to break. Ledgering them
    would record a few hundred ids nobody can bind to, and that is how a
    ledger stops being read.
  * docker-compose service names and published ports are deliberately out of
    scope. They are a real surface, and they are the STACK's shape rather than
    the binary's; one gate answering both questions would answer both badly.
  * The docs/04-configuration.md cross-check below REPORTS and does not fail.
    Every flag and env var it can name is either documented in that table or
    recorded in the baseline's `docsUndocumented` with the reason it is not a
    user-facing knob, so promoting it into --strict is a one-line change once
    that set is agreed to be complete. It does not land muted on day one: it
    lands empty. Measured before it was closed: 3 flags (-kql-url,
    -list-page-size, -tsql-strict) and 11 variables absent from the table,
    which nothing in scripts/ read.

Usage:
    check_backward_compat.py              report, exit 0
    check_backward_compat.py --strict     exit non-zero on drift
    check_backward_compat.py --update     rewrite the baseline
"""
import argparse
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASELINE = ROOT / "docs" / "compat-surface.json"

sys.path.insert(0, str(ROOT / "scripts"))

import check_route_coverage as cov  # noqa: E402

# Where each surface is DERIVED FROM. Module level so the tests can point the
# whole gate at a synthetic tree: the real tree's surface rotates, and a test
# asserting against it would just be a second copy of the baseline.
MAIN_GO = ROOT / "cmd" / "fabric-emulator" / "main.go"
ENV_SOURCES = (ROOT / "internal", ROOT / "cmd")
# A DIRECTORY and not a package list, so `pkg/*` is covered the day a second
# package lands rather than the day somebody remembers this line.
PKG_SOURCES = (ROOT / "pkg",)
CONFIG_DOC = ROOT / "docs" / "04-configuration.md"

SURFACES = ("subcommands", "cliFlags", "envVars", "goAPI", "routes")

# `fs.StringVar(&cfg.Addr, "addr", …)` and the pointer-returning form
# `fs.String("addr", …)`, anchored on the flagset variable main.go uses. A
# rename of that variable resolves ZERO flags -- which fails loudly below
# rather than quietly reporting an empty surface. See _require_nonempty.
_FLAG = re.compile(
    r"\bfs\.(?:String|Bool|Int|Int64|Uint|Uint64|Float64|Duration)(?:Var)?\(\s*"
    r'(?:&[^,]+,\s*)?"([A-Za-z0-9_.-]+)"')

# The subcommand switch, found by BRACE-MATCHING its body rather than by a
# line window, so a case added anywhere inside it is picked up.
_SWITCH = re.compile(r"switch\s+args\[0\]\s*\{")
_CASE = re.compile(r"\bcase\s+([^:\n]+):")

# Every way this tree reads a FABRIC_* variable: os directly, or one of
# internal/config's own typed helpers.
_ENV_READ = re.compile(
    r"\b(?:os\.Getenv|os\.LookupEnv|envOr|envDefault|boolEnv|intEnv|durationEnv"
    r"|maxBytesEnv)"
    r'\(\s*"(FABRIC_[A-Z0-9_]+)"\s*[,)]')
# …and every FABRIC_* string literal at all, so a read form this parser does
# NOT know gets NAMED instead of silently dropping a knob out of the surface.
# The reader list above is five function names in one file; this guard is what
# makes renaming one of them a failure rather than a quietly shrinking ledger --
# and it has now done so once for real: `maxRequestBytesEnv` was generalised to
# `maxBytesEnv(key, default)` when the TDS listener needed the same escape-hatch
# shape, and this line is what reported the two knobs it reads as missing from
# the surface rather than letting them drop out of it.
_ENV_LITERAL = re.compile(r'"(FABRIC_[A-Z0-9_]+)"')
_LINE_COMMENT = re.compile(r"//[^\n]*")

# --- the exported Go API of pkg/ ----------------------------------------------
#
# READ OFF THE SOURCE TEXT, not with go/types, for the reason this whole gate
# is offline: `make check` must answer before a push, with nothing but Python
# and no Go toolchain required. The cost of that is a parser that knows only
# the declaration forms this tree uses -- and the cost is PAID BY
# unparsed_exports() below, which NAMES a form it cannot read instead of
# letting the ledger quietly shrink. That is the same bargain _ENV_LITERAL
# above strikes with _ENV_READ, for the same reason.

# The declared NAME of a top-level declaration, in any form, used only to ask
# whether there is an export on this line at all. DELIBERATELY LOOSER than the
# strict matchers below: a line this finds an exported name on, and none of
# them resolves, is exactly the anti-silence case.
_GO_DECL = re.compile(
    r"^(?:func|type|const|var)\s+(?:\(\s*\w*\s*\*?\w+\s*\)\s*)?([A-Za-z_]\w*)")
_GO_METHOD = re.compile(
    r"^func\s+\(\s*\w+\s+\*?([A-Za-z_]\w*)\s*\)\s*([A-Za-z_]\w*)\s*\(")
_GO_FUNC = re.compile(r"^func\s+([A-Za-z_]\w*)\s*\(")
_GO_TYPE = re.compile(r"^type\s+([A-Za-z_]\w*)\s+(.*)$")
_GO_VALUE = re.compile(r"^(?:const|var)\s+([A-Za-z_]\w*)\b")
_GO_GROUP = re.compile(r"^(const|var|type)\s*\(\s*$")
_GO_PACKAGE = re.compile(r"^package\s+(\w+)", re.MULTILINE)
# One entry of a grouped block, and one field of a struct body. A field line
# may name several at once (`schema, table string`).
_GO_ENTRY = re.compile(r"^([A-Za-z_]\w*)\b(.*)$")
_GO_FIELD = re.compile(r"^([A-Za-z_]\w*(?:\s*,\s*[A-Za-z_]\w*)*)\s+\S")
# An exported interface's method set. NOT ledgered on this pass -- `pkg/` has
# no exported interface today -- so this exists to make the first one FAIL
# rather than land silently unrecorded, which is the whole point of the guard.
_GO_IFACE_METHOD = re.compile(r"^([A-Z]\w*)\s*\(")


def _rel(path):
    """Repo-relative when the file is in the repo, absolute otherwise.

    ENV_SOURCES is monkeypatched to a temp directory by the tests, and
    Path.relative_to RAISES rather than falling back, so the reporter would
    crash on exactly the case the tests exist to cover. Forward slashes
    always, so a Windows leg reads the same as a Linux one.
    """
    try:
        rel = str(path.relative_to(ROOT))
    except ValueError:
        rel = str(path)
    return rel.replace("\\", "/")


def _go_sources():
    for base in ENV_SOURCES:
        for path in sorted(base.rglob("*.go")):
            if not path.name.endswith("_test.go"):
                yield path


def subcommands():
    """The bare words `fabric-emulator <word>` dispatches on before flags."""
    text = MAIN_GO.read_text(encoding="utf-8")
    match = _SWITCH.search(text)
    if not match:
        return []
    depth, index = 1, match.end()
    while index < len(text) and depth:
        if text[index] == "{":
            depth += 1
        elif text[index] == "}":
            depth -= 1
        index += 1
    found = set()
    for labels in _CASE.findall(text[match.end():index]):
        found.update(re.findall(r'"([^"]+)"', labels))
    return sorted(found)


def cli_flags():
    """Every flag name the emulator's flagset registers."""
    return sorted(set(_FLAG.findall(MAIN_GO.read_text(encoding="utf-8"))))


def env_vars():
    """Every FABRIC_* variable the emulator READS, wherever it reads it.

    Not scoped to internal/config, deliberately: FABRIC_RECORD_RESPONSES,
    FABRIC_S3_REGION and FABRIC_TDS_TRACE are read in three other packages and
    are knobs a caller sets all the same -- ci.yml sets the first one on eight
    jobs. A surface is where the process reads it, not where the tidy place to
    read it would be.
    """
    found = set()
    for path in _go_sources():
        found.update(_ENV_READ.findall(path.read_text(encoding="utf-8")))
    return sorted(found)


def unrecognised_env_reads():
    """FABRIC_* literals in the source that no known read form explains."""
    surprises = []
    for path in _go_sources():
        text = _LINE_COMMENT.sub("", path.read_text(encoding="utf-8"))
        for name in sorted(set(_ENV_LITERAL.findall(text)) - set(_ENV_READ.findall(text))):
            surprises.append((_rel(path), name))
    return surprises


def _exported(name):
    """Go's whole export rule: an initial capital, and nothing else."""
    return name[:1].isupper()


def _pkg_sources():
    for base in PKG_SOURCES:
        for path in sorted(base.rglob("*.go")):
            if not path.name.endswith("_test.go"):
                yield path


def _braced(lines, start):
    """The brace-matched body opening on lines[start], and the line it ends on.

    BRACE-MATCHED rather than read to the next line beginning with `}`, the
    same technique subcommands() uses on the dispatch switch and for the same
    reason: a nested struct and a one-line `struct{ Msg string }` both get
    their real extent, where a line window reads one of them wrong. This tree
    has one of each.
    """
    text = "\n".join(lines[start:])
    opened = text.index("{")
    depth, index = 1, opened + 1
    while index < len(text) and depth:
        if text[index] == "{":
            depth += 1
        elif text[index] == "}":
            depth -= 1
        index += 1
    return text[opened + 1:index - 1], start + text[:index].count("\n")


def _struct_fields(body):
    """Exported field names in one struct body, and lines read as no field."""
    names, unread = [], []
    depth = 0
    for raw in body.split("\n"):
        line = _LINE_COMMENT.sub("", raw).strip()
        here, depth = depth, depth + line.count("{") - line.count("}")
        # A nested type's own body is not this struct's surface.
        if here or not line:
            continue
        field = _GO_FIELD.match(line)
        if not field:
            # An embedded field, or a shape this parser does not know. Either
            # way it yielded no id, so an EXPORTED one has to be named.
            if _exported(line):
                unread.append(line)
            continue
        names += [n for n in (p.strip() for p in field.group(1).split(","))
                  if _exported(n)]
    return names, unread


def _read_go_api(pkg, text):
    """One file: the exported ids it declares, and the lines left unread."""
    ids, unread = [], []
    lines = text.split("\n")
    index, group = 0, None

    def body_at(line_index, name, rest):
        """Skip a type's body, taking its exported fields on the way past."""
        body, end = _braced(lines, line_index)
        if _exported(name):
            if re.search(r"\bstruct\b", rest):
                fields, missed = _struct_fields(body)
                ids.extend(f"{pkg}.{name}.{field}" for field in fields)
                unread.extend(missed)
            else:
                # An exported interface, or an exported type whose body is
                # neither. Its members are a contract this pass does not
                # record, so they are named rather than dropped.
                unread.extend(
                    member.strip() for member in body.split("\n")
                    if _GO_IFACE_METHOD.match(
                        _LINE_COMMENT.sub("", member).strip()))
        return end + 1

    while index < len(lines):
        line = _LINE_COMMENT.sub("", lines[index])
        index += 1

        if group:
            if line.startswith(")"):
                group = None
                continue
            entry = _GO_ENTRY.match(line.strip())
            if not entry:
                continue
            name, rest = entry.group(1), entry.group(2)
            if _exported(name):
                ids.append(f"{pkg}.{name}")
            # A grouped `type (` entry can carry a body, and it must be
            # skipped whether or not the type is exported: its field lines
            # would otherwise read as further entries of the group.
            if group == "type" and "{" in rest:
                index = body_at(index - 1, name, rest)
            continue

        opening = _GO_GROUP.match(line)
        if opening:
            group = opening.group(1)
            continue

        method = _GO_METHOD.match(line)
        if method:
            receiver, name = method.group(1), method.group(2)
            # An unexported receiver or an unexported method cannot be named
            # from another module, so neither is an id. DECIDED HERE, which is
            # what keeps it out of unparsed_exports below -- the difference
            # between a deliberate exclusion and a parser that gave up.
            if _exported(receiver) and _exported(name):
                ids.append(f"{pkg}.{receiver}.{name}")
            continue

        func = _GO_FUNC.match(line)
        if func:
            if _exported(func.group(1)):
                ids.append(f"{pkg}.{func.group(1)}")
            continue

        kind = _GO_TYPE.match(line)
        if kind:
            name, rest = kind.group(1), kind.group(2)
            if _exported(name):
                ids.append(f"{pkg}.{name}")
            if "{" in rest:
                index = body_at(index - 1, name, rest)
            continue

        value = _GO_VALUE.match(line)
        if value:
            if _exported(value.group(1)):
                ids.append(f"{pkg}.{value.group(1)}")
            continue

        declaration = _GO_DECL.match(line)
        if declaration and _exported(declaration.group(1)):
            unread.append(line.strip())

    return ids, unread


def _go_api_scan():
    """Both answers at once, so the ledger and the guard cannot disagree."""
    ids, surprises = set(), []
    for path in _pkg_sources():
        text = path.read_text(encoding="utf-8")
        clause = _GO_PACKAGE.search(text)
        pkg = clause.group(1) if clause else path.parent.name
        found, unread = _read_go_api(pkg, text)
        ids.update(found)
        surprises += [(_rel(path), line) for line in unread]
    return ids, surprises


def go_api():
    """Every exported identifier of `pkg/`, spelled as an importer writes it.

    `<package>.<Name>` for a package-level func, type, const or var;
    `<package>.<Type>.<Method>` where receiver AND method are both exported;
    `<package>.<Type>.<Field>` for an exported field of an exported struct.
    Unexported receivers, types and fields never enter the ledger, because
    Go's export rule means no other module can bind to one -- so `rfNode`,
    `rfLiteral` and `RowFilter.root` are absent on purpose, not by omission.
    """
    return sorted(_go_api_scan()[0])


def unparsed_exports():
    """`pkg/` declarations whose exported name this parser could not read.

    THE SAME GUARD unrecognised_env_reads() is, one surface over, and it
    exists for the same failure: a refactor that writes an export in a form
    this parser does not know would shrink the ledger instead of failing it,
    and a shrinking ledger reads in review as a clean tree. A generic type, an
    embedded field, an exported interface's method set -- each is named here,
    with the file it is in, rather than dropped.
    """
    return _go_api_scan()[1]


def _shape(template):
    """A path template with its parameter NAMES erased.

    MUST AGREE WITH check_surface_ledger._shape, and a test asserts that it
    does. The reason is the same in both places and it is a REAL near-miss
    here: renaming `{wid}` to `{workspaceId}` changes no URL any client ever
    writes -- the client sends a GUID -- so reporting it as a removal plus an
    addition would be noise that trains people to run --update without
    reading. Go's `{path...}` subtree wildcard folds to the same placeholder,
    which is coarser than the router and coarse in the safe direction.

    Measured when this landed: 1056 registered routes shape to 1056 distinct
    rows, so the erasure costs nothing today -- it only removes the false
    positive.
    """
    return re.sub(r"\{[^}]+\}", "{}", template).rstrip("/")


def routes():
    """Every registered in-scope route, one row per spelling.

    Imported from check_route_coverage rather than re-parsed, exactly as
    check_surface_ledger does: the Go registration forms are genuinely hard to
    read, and two parsers would be two places for the reading to go stale.
    """
    shaped = set()
    for label in cov.registered():
        method, _, template = label.partition(" ")
        shaped.add(f"{method} {_shape(template)}")
    return sorted(shaped)


EXTRACTORS = {
    "subcommands": (subcommands, lambda: _rel(MAIN_GO)),
    "cliFlags": (cli_flags, lambda: _rel(MAIN_GO)),
    "envVars": (env_vars, lambda: ", ".join(_rel(p) for p in ENV_SOURCES)),
    "goAPI": (go_api, lambda: ", ".join(_rel(p) for p in PKG_SOURCES)),
    "routes": (routes, lambda: ", ".join(_rel(p) for p in cov.SOURCES)),
}


def tree_surface():
    """The five surfaces as the tree has them now, keyed by surface name."""
    return {name: EXTRACTORS[name][0]() for name in SURFACES}


def _require_nonempty(surface):
    """Surfaces whose parser resolved nothing, with the file it reads.

    THE FAILURE THIS EXISTS FOR. A refactor that renames `fs`, moves the
    switch or relocates the packages leaves an extractor returning an empty
    list; the baseline comparison then matches nothing against nothing for
    that surface, and the gate goes on passing having stopped measuring. That
    is the shape every checker in this directory was written about, and it is
    worth four lines to make it impossible here.
    """
    return [(name, EXTRACTORS[name][1]()) for name in SURFACES if not surface[name]]


def read_baseline():
    if not BASELINE.is_file():
        return None
    return json.loads(BASELINE.read_text(encoding="utf-8"))


def write_baseline(surface, previous=None):
    """Rewrite the baseline from the tree, PRESERVING the hand-written parts.

    `removed` and `docsUndocumented` are decisions someone wrote down, so
    --update must not be able to erase them -- otherwise regenerating is how a
    breaking change gets laundered into a clean diff. Removed ids are kept OUT
    of the live lists, so the file never records the same name twice with two
    different verdicts.
    """
    previous = previous or {}
    removed = previous.get("removed", [])
    gone = {(entry.get("surface"), entry.get("id")) for entry in removed}
    lists = {name: sorted(i for i in surface[name] if (name, i) not in gone)
             for name in SURFACES}
    BASELINE.write_text(json.dumps({
        "_comment": [
            "The surface an external user BINDS TO: the emulator binary's",
            "subcommands and CLI flags, the FABRIC_* environment knobs it",
            "reads, and every in-scope HTTP route it registers -- plus one",
            "surface served over no wire at all, the exported Go API of",
            "`pkg/`, which is a COMPILE-TIME contract an importing module",
            "binds to: that directory exists so another module CAN import it,",
            "where Go's internal rule would forbid it. Written by",
            "scripts/check_backward_compat.py --update; do not hand-edit the",
            "five lists. `removed` and `docsUndocumented` ARE hand-written and",
            "are preserved across --update.",
            "The gate is asymmetric on purpose. An entry in the tree and not",
            "here is a STALE LEDGER: run --update and let the diff be the",
            "review of what is newly promised. An entry here and not in the",
            "tree is a BREAKING CHANGE and fails until it is written into",
            "`removed` with the release it went away in and why -- --update",
            "cannot launder a removal. An id in `removed` that has come back",
            "fails too, because a stale exemption goes on excusing a surface",
            "nobody is holding.",
            "It says NOTHING about whether a surface still BEHAVES the same --",
            "that is conformance, a different gate on a different input.",
            "docker-compose service names and published ports are deliberately",
            "out of scope.",
        ],
        "counts": {name: len(lists[name]) for name in SURFACES},
        "subcommands": lists["subcommands"],
        "cliFlags": lists["cliFlags"],
        "envVars": lists["envVars"],
        "goAPI": lists["goAPI"],
        "docsUndocumented": previous.get("docsUndocumented", {}),
        "removed": removed,
        "routes": lists["routes"],
    }, indent=2) + "\n", encoding="utf-8")


def removal_problems(baseline):
    """`removed` entries that are not a decision anyone could review."""
    problems = []
    for index, entry in enumerate(baseline.get("removed", [])):
        where = f"removed[{index}] ({entry.get('id', '?')})"
        if entry.get("surface") not in SURFACES:
            problems.append(f"{where}: surface {entry.get('surface')!r} is not one "
                            f"of {list(SURFACES)}")
        for field in ("id", "removedIn", "reason"):
            if not str(entry.get(field, "")).strip():
                problems.append(f"{where}: {field} is empty -- a removal with no "
                                "reason is an omission wearing a decision's clothes")
    return problems


def drift(surface, baseline):
    """Per surface: what vanished, what arrived, and what came back."""
    out = {}
    for name in SURFACES:
        gone = {entry.get("id") for entry in baseline.get("removed", [])
                if entry.get("surface") == name}
        tree, pinned = set(surface[name]), set(baseline.get(name, []))
        out[name] = {
            "breaking": sorted(pinned - tree - gone),
            "stale": sorted(tree - pinned - gone),
            "resurrected": sorted(gone & tree),
        }
    return out


def undocumented_knobs(surface, baseline):
    """Baseline flags and env vars docs/04's table never mentions.

    REPORTED, NOT ENFORCED, on this pass. Accepted omissions live in the
    baseline's `docsUndocumented` with the reason each one is not a knob a user
    is meant to set, so this list is empty today and promoting it into --strict
    is one line.
    """
    if not CONFIG_DOC.is_file():
        return []
    table = "\n".join(line for line in
                      CONFIG_DOC.read_text(encoding="utf-8").splitlines()
                      if line.startswith("|"))
    accepted = baseline.get("docsUndocumented", {})
    missing = [f"-{flag}" for flag in surface["cliFlags"]
               if f"`-{flag}`" not in table and f"-{flag}" not in accepted]
    missing += [name for name in surface["envVars"]
                if f"`{name}`" not in table and name not in accepted]
    return missing


def main_for_test(strict=False, update=False):
    """main() without argparse, so the gate itself can be driven by a test."""
    surface = tree_surface()

    empty = _require_nonempty(surface)
    if empty:
        print("check_backward_compat: a surface parser resolved NOTHING, so the "
              "gate would go on passing having stopped measuring:", file=sys.stderr)
        for name, where in empty:
            print(f"    {name}: 0 entries from {where}", file=sys.stderr)
        print("  -> teach the extractor the form the tree now uses. An empty "
              "surface is never an answer.", file=sys.stderr)
        return 1

    surprises = unrecognised_env_reads()
    if surprises:
        print("check_backward_compat: a FABRIC_* variable is read in a form this "
              "parser does not know, so it would be missing from the surface:",
              file=sys.stderr)
        for rel, name in surprises:
            print(f"    {rel}: {name}", file=sys.stderr)
        print("  -> add the reader to _ENV_READ.", file=sys.stderr)
        return 1

    unread = unparsed_exports()
    if unread:
        print("check_backward_compat: a pkg/ declaration is written in a form "
              "this parser does not know, so its export would be missing from "
              "the surface:", file=sys.stderr)
        for rel, line in unread:
            print(f"    {rel}: {line}", file=sys.stderr)
        print("  -> teach go_api() the form the tree now uses.", file=sys.stderr)
        return 1

    previous = read_baseline()

    if update:
        # --UPDATE IS FOR ADDITIONS ONLY, and this refusal is what makes the
        # docstring's claim true rather than aspirational. Without it, the fix
        # for "you deleted a flag" would be to regenerate: the id leaves the
        # lists, --strict goes green, and the gate is a changelog agreeing with
        # itself. Measured by driving it -- a real flag and a real route were
        # deleted from the tree, and the first version of this regenerated both
        # away without comment.
        #
        # A resurrection is refused here too. Regenerating must not look like
        # the fix for a stale exemption either; the `removed` entry is what has
        # to go.
        blocked = {}
        if previous is not None:
            for name, change in drift(surface, previous).items():
                if change["breaking"] or change["resurrected"]:
                    blocked[name] = change
        if blocked:
            print("check_backward_compat: --update REFUSED. It records additions; "
                  "it cannot launder a removal.", file=sys.stderr)
            for name, change in blocked.items():
                for entry in change["breaking"]:
                    print(f"    gone from the tree: {name} {entry}", file=sys.stderr)
                for entry in change["resurrected"]:
                    print(f"    recorded as removed and back: {name} {entry}",
                          file=sys.stderr)
            print("  -> restore it, or write it into `removed` by hand with the "
                  "release it went away in and why, and run --update again. For a "
                  "resurrection, delete the `removed` entry.", file=sys.stderr)
            return 1
        write_baseline(surface, previous)
        counts = ", ".join(f"{len(surface[n])} {n}" for n in SURFACES)
        print(f"check_backward_compat: baseline rewritten -- {counts}")
        return 0

    if previous is None:
        print(f"check_backward_compat: no baseline at {BASELINE}. "
              f"Create one with --update.", file=sys.stderr)
        return 1

    counts = ", ".join(f"{len(surface[n])} {n}" for n in SURFACES)
    print(f"check_backward_compat: {counts}")

    problems = removal_problems(previous)
    changes = drift(surface, previous)
    failed = bool(problems)

    if problems:
        print("\nthe `removed` ledger is not reviewable:\n")
        for problem in problems:
            print(f"  - {problem}")

    for name in SURFACES:
        change = changes[name]
        if change["breaking"]:
            failed = True
            print(f"\nBREAKING CHANGE -- {len(change['breaking'])} {name} entry(ies) a "
                  f"released binary accepted and this tree does not:\n")
            for entry in change["breaking"]:
                print(f"  - {entry}")
            print("\n  -> restore it, or record it in `removed` with the release it "
                  "went away in and why. --update cannot launder a removal.")
        if change["resurrected"]:
            failed = True
            print(f"\n{len(change['resurrected'])} {name} entry(ies) are recorded as "
                  f"removed and are back in the tree:\n")
            for entry in change["resurrected"]:
                print(f"  - {entry}")
            print("\n  -> drop the `removed` entry and run --update. A stale "
                  "exemption goes on excusing a surface nobody is holding.")
        if change["stale"]:
            failed = True
            print(f"\nSTALE LEDGER -- {len(change['stale'])} {name} entry(ies) in the "
                  f"tree and not in the baseline:\n")
            for entry in change["stale"]:
                print(f"  - {entry}")
            print("\n  -> run --update. The diff is the review of what surface this "
                  "repository has newly promised to keep.")

    if not failed:
        print("  every recorded subcommand, flag, env var, exported Go API id "
              "and route is still here")

    undocumented = undocumented_knobs(surface, previous)
    if undocumented:
        print(f"\nREPORT ONLY -- {len(undocumented)} knob(s) docs/04-configuration.md "
              f"does not mention:\n")
        for knob in undocumented:
            print(f"  - {knob}")
        print("\n  -> document it in the table, or record it in the baseline's "
              "`docsUndocumented` with the reason it is not a user-facing knob. "
              "This section does not fail the build yet.")

    return 1 if (failed and strict) else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--strict", action="store_true",
                        help="exit non-zero when the baseline and the tree disagree")
    parser.add_argument("--update", action="store_true",
                        help="rewrite the baseline from this tree")
    arguments = parser.parse_args()
    return main_for_test(strict=arguments.strict, update=arguments.update)


if __name__ == "__main__":
    sys.exit(main())
