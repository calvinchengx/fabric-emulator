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

Measured when this landed: 2 subcommands, 28 CLI flags, 37 FABRIC_* variables,
and 1056 registered in-scope routes (174 after the route ratchet's family
collapse, which this gate deliberately does NOT apply -- see below).

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
    that still parses and now means the opposite passes here. That is
    conformance and the e2e suites: a different gate on a different input.
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
CONFIG_DOC = ROOT / "docs" / "04-configuration.md"

SURFACES = ("subcommands", "cliFlags", "envVars", "routes")

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
    r"\b(?:os\.Getenv|os\.LookupEnv|envOr|envDefault|boolEnv|intEnv|durationEnv)"
    r'\(\s*"(FABRIC_[A-Z0-9_]+)"\s*[,)]')
# …and every FABRIC_* string literal at all, so a read form this parser does
# NOT know gets NAMED instead of silently dropping a knob out of the surface.
# The reader list above is five function names in one file; this guard is what
# makes renaming one of them a failure rather than a quietly shrinking ledger.
_ENV_LITERAL = re.compile(r'"(FABRIC_[A-Z0-9_]+)"')
_LINE_COMMENT = re.compile(r"//[^\n]*")


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
    "routes": (routes, lambda: ", ".join(_rel(p) for p in cov.SOURCES)),
}


def tree_surface():
    """The four surfaces as the tree has them now, keyed by surface name."""
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
            "reads, and every in-scope HTTP route it registers. Written by",
            "scripts/check_backward_compat.py --update; do not hand-edit the",
            "four lists. `removed` and `docsUndocumented` ARE hand-written and",
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
        print("  every recorded subcommand, flag, env var and route is still here")

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
