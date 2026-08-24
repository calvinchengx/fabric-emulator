#!/usr/bin/env python3
"""Axis A, derived: our `notebookutils` shim against Microsoft's OWN stubs.

`docs/56` calls Axis A — what `notebookutils.*` exposes, and with which
parameters — "enumerable, therefore gradeable". It is graded, against
`e2e/conformance/notebookutils-reference.json`: 44 members transcribed from
Learn pages by hand, each carrying its source URL and read-date, checked live
by contract 2 on both backends.

This adds the SECOND source, and it is the one transcription cannot supply:
`third_party/notebookutils-stubs/` pins the `dummy-notebookutils` wheel
Microsoft publishes so notebook code can be developed off-cluster. Every
function, every parameter name, empty bodies, MIT.

WHAT HOLDING BOTH FOUND. **The two Microsoft sources disagree**, and until now
nothing in this repo could see it. A sample of what the run prints today — the
count is not written here, because a number in a docstring is a claim nothing
checks, which is the defect this file exists to remove:

    fs.ls          stub `dir`          docs `path`
    fs.exists      stub `file`         docs `path`
    fs.unmount     stub `extraOptions` docs `extraConfigs`
    notebook.run   stub `workspaceId`  docs `workspace`
    credentials.getToken   stub `(audience, name)`   docs `(audience)`

The shim follows the documentation, which is the right call — the stub is
Synapse-lineage and the pages are Fabric's own — but "right call" was not a
decision anyone made, because the disagreement was invisible. Now it is
computed on every run, and the classification is DERIVED rather than declared:
if ours matches the docs and the stub differs, that is a stale stub and this
says so without a human writing an entry.

WHAT ONLY THE STUB KNOWS. It also carries members the per-module pages did not
yield to transcription: `help()` on every module (which Fabric's own fs page
documents in its opening lines), `runtime.getCurrentWorkspaceId`, `udf.run`,
`lakehouse.getDefinition`. Those are real gaps, listed in PLANNED, and they are
the answer to "what does a second source buy over a careful reading".

WHAT IT IS NOT. The stub is broader than Fabric: `conf`, `connections`, `data`
and `fabricClient` are absent from Fabric's module list, and Fabric's page says
`fabricClient` and `PBIClient` "aren't supported yet". A module classified in
neither list FAILS rather than being guessed at, which would either manufacture
~20 phantom gaps or hide a real one.

Usage:
    check_notebookutils_surface.py            report
    check_notebookutils_surface.py --strict   exit non-zero on any problem
"""
import argparse
import ast
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
STUBS = ROOT / "third_party" / "notebookutils-stubs" / "notebookutils"
SHIM = ROOT / "python" / "notebookutils"
DOCS = ROOT / "e2e" / "conformance" / "notebookutils-reference.json"

# Fabric's documented modules, from
# https://learn.microsoft.com/en-us/fabric/data-engineering/notebook-utilities
# (read 2026-08-24). `notebook` appears twice on that page, as "run and
# orchestration" and "management"; it is one namespace.
FABRIC_MODULES = {
    "fs", "notebook", "credentials", "lakehouse", "runtime", "session",
    "udf", "variableLibrary",
}

# In the stub, absent from Fabric's list. Each needs the reason, because
# "not implemented" and "not part of the product" look identical in a diff.
NOT_FABRIC_MODULES = {
    "conf": "Synapse-era Spark conf helper; absent from Fabric's module list.",
    "connections": "Synapse linked-service connections; Fabric uses Connections "
                   "on the control plane, not a notebook module.",
    "data": "Synapse-era data helper; absent from Fabric's module list.",
    "fabricClient": "Fabric's own page says under Known issues that fabricClient "
                    "and PBIClient 'aren't supported yet'. Implementing it would "
                    "be surface Fabric does not have.",
}

# Members the stub has, the documentation does not, and we deliberately do not
# implement. Synapse inheritance, mostly: one package ships for both products.
WAIVED = {
    "credentials.getConnectionStringOrCreds": "linked services are Synapse, not Fabric",
    "credentials.getFullConnectionString": "linked services are Synapse, not Fabric",
    "credentials.getPropertiesAll": "linked services are Synapse, not Fabric",
    "credentials.getSPTokenWithCert": "certificate-based SP auth against a Synapse linked "
                                      "service or AKV URI; no Fabric equivalent",
    "credentials.getSPTokenWithCertLS": "as getSPTokenWithCert, via a linked service",
    "credentials.putSecretWithLS": "linked services are Synapse, not Fabric",
    "credentials.configureAzureBlobStorageSASBased": "configures a Spark session from a "
                                                     "Synapse linked service",
    "credentials.configureADLS2TokenBased": "configures a Spark session from a Synapse "
                                            "linked service",
    "credentials.configureADLS2SASBased": "configures a Spark session from a Synapse "
                                          "linked service",
    "credentials.configureADLS2AzureKeyVaultBased": "configures a Spark session from a "
                                                    "Synapse linked service",
    "credentials.tridentHelp": "internal help text for the Fabric (Trident) build of the "
                               "real package; not a capability",
    "credentials.synapseHelp": "internal help text for the Synapse build",
    "fs.mountToDriverNode": "mounts onto the Spark DRIVER's local filesystem. Sail has no "
                            "driver node in that sense, and docs/37 records the single "
                            "mount point this emulator does model",
    "fs.unmountFromDriverNode": "as mountToDriverNode",
    "notebook.updateNBSEndpoint": "repoints the package at another notebook-service "
                                  "endpoint; ours is set by NOTEBOOKUTILS_FABRIC_URL",
}

# Gaps that ARE surface Microsoft ships. Each is visible in every run rather
# than living in a diff nobody runs, and removing an entry is how one is closed.
PLANNED = {
    "fs.help": "the help() family — Fabric's own fs page opens by documenting "
               "`notebookutils.fs.help()` as the discovery mechanism, and we have it on "
               "no module. The transcription did not yield it because it is prose on the "
               "page rather than a row in the method table",
    "credentials.help": "see fs.help",
    "lakehouse.help": "see fs.help",
    "notebook.help": "see fs.help",
    "runtime.help": "see fs.help",
    "session.help": "see fs.help",
    "udf.help": "see fs.help",
    "fs.refreshMounts": "mount lifecycle beyond the single mount point docs/37 models",
    "fs.nbResPath": "notebook resources (`builtin/`), which Axis C also lists as absent",
    "lakehouse.getDefinition": "definition round-trip through the shim; the REST surface "
                               "behind it exists",
    "lakehouse.updateDefinition": "as getDefinition",
    "runtime.getCurrentWorkspaceId": "runtime context member",
    "udf.run": "invoking a User Data Function; the UDF item type has no engine here",
    "udf.getHelpString": "help text for udf",
    "variableLibrary.getHelpString": "help text for variableLibrary",
}

# Parameters we accept that Fabric's documentation does not list. Forgiving is
# still divergent: a notebook written here can pass an argument that does not
# exist upstream, and it fails in Fabric rather than locally.
EXTRA_PARAMS = {
    # These two are Microsoft's OWN signature, from the stub, and narrower in
    # the docs. Keeping them costs nothing and drops a caller written against
    # the real package; the direction of risk is the opposite of notebook.run's.
    "credentials.getSecret": "linkedService is in Microsoft's stub and not on Fabric's "
                             "credentials page. Synapse-era, accepted and ignored, so "
                             "code written against the real package still imports",
    "session.stop": "detach is in Microsoft's stub and not on Fabric's session page",
    "notebook.run": "spark_environment and attach_lakehouse are emulator levers for "
                    "binding a run to an Environment item or a default lakehouse without "
                    "a portal. Documented in docs/14; not in Fabric's signature",
}


class Unreadable(Exception):
    """The vendored stubs or the shim are not the shape this reads."""


def members(path: pathlib.Path) -> dict[str, list[str]]:
    """Public module-level functions and their positional parameter names."""
    tree = ast.parse(path.read_text(encoding="utf-8"))
    out = {}
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and not node.name.startswith("_"):
            args = node.args
            out[node.name] = [a.arg for a in args.posonlyargs + args.args]
    return out


def stub_modules() -> dict[str, dict[str, list[str]]]:
    if not STUBS.is_dir():
        raise Unreadable(
            f"{STUBS} is missing. Run scripts/vendor_notebookutils_stubs.py; without the "
            "pin this check has no oracle and would pass over nothing.")
    found = {p.stem: members(p) for p in sorted(STUBS.glob("*.py")) if p.stem != "__init__"}
    if not found:
        raise Unreadable(
            f"{STUBS} contains no stub module. The wheel layout changed and this check "
            "is now vacuous.")
    return found


def documented() -> dict[str, dict[str, list[str]]]:
    """Fabric's own signatures, as transcribed with a source per entry."""
    if not DOCS.is_file():
        raise Unreadable(f"{DOCS} is missing; the documentation source is half the check")
    raw = json.loads(DOCS.read_text(encoding="utf-8"))["modules"]
    # `kind: property` entries are not callables. `runtime.context` is the one
    # today: a dict a notebook READS, so it has no signature to compare and
    # comparing it as a function reported our shim as missing documented
    # surface it implements as a module attribute.
    return {name.split(".", 1)[1]:
            {m: e["params"] for m, e in members.items() if e.get("kind") != "property"}
            for name, members in raw.items()}


def problems() -> tuple[list[str], list[str]]:
    """(failures, notes). Notes are declared divergences, reported not fatal."""
    stubs = stub_modules()
    docs = documented()
    failures: list[str] = []
    notes: list[str] = []

    unclassified = sorted(set(stubs) - FABRIC_MODULES - set(NOT_FABRIC_MODULES))
    for module in unclassified:
        failures.append(
            f"notebookutils.{module} is in the pinned stubs and in neither FABRIC_MODULES "
            "nor NOT_FABRIC_MODULES. Decide which, with the reason: guessing would either "
            "manufacture phantom gaps or hide a real one.")
    for module in sorted(FABRIC_MODULES - set(stubs)):
        failures.append(
            f"FABRIC_MODULES lists {module}, which the pinned stubs do not carry. Either "
            "the pin moved or the list was wrong.")
    for module in sorted(set(NOT_FABRIC_MODULES) - set(stubs)):
        failures.append(
            f"NOT_FABRIC_MODULES excludes {module}, which the pinned stubs no longer "
            "carry. Drop the entry; it is describing the past.")

    seen_waived: set[str] = set()
    seen_planned: set[str] = set()
    seen_extra: set[str] = set()

    for module in sorted(FABRIC_MODULES & set(stubs)):
        ours_path = SHIM / f"{module}.py"
        if not ours_path.exists():
            failures.append(
                f"notebookutils.{module} is a documented Fabric module and our shim has "
                "no such file")
            continue
        theirs, ours, doc = stubs[module], members(ours_path), docs.get(module, {})
        for name in sorted(set(theirs) | set(doc)):
            qualified = f"{module}.{name}"
            stub_sig, doc_sig, our_sig = theirs.get(name), doc.get(name), ours.get(name)

            if our_sig is None:
                if qualified in WAIVED and doc_sig is None:
                    seen_waived.add(qualified)
                elif qualified in PLANNED:
                    seen_planned.add(qualified)
                    shape = ", ".join(doc_sig if doc_sig is not None else stub_sig or [])
                    notes.append(f"gap      {qualified}({shape}) — {PLANNED[qualified]}")
                elif doc_sig is not None:
                    failures.append(
                        f"{qualified}({', '.join(doc_sig)}) is DOCUMENTED Fabric surface "
                        "and absent from our shim")
                else:
                    failures.append(
                        f"{qualified}({', '.join(stub_sig or [])}) is in Microsoft's stub "
                        "and not in ours. Declare it in WAIVED (not Fabric surface) or "
                        "PLANNED (a real gap), with the reason.")
                continue

            # The documentation wins where it speaks: it is Fabric's, the stub
            # is one package for Fabric and Synapse both.
            if doc_sig is not None:
                extra = our_sig[len(doc_sig):]
                if our_sig[:len(doc_sig)] != doc_sig:
                    failures.append(
                        f"{qualified} does not match Fabric's DOCUMENTED signature: "
                        f"docs({', '.join(doc_sig)}) ours({', '.join(our_sig)})")
                elif extra:
                    if qualified in EXTRA_PARAMS:
                        seen_extra.add(qualified)
                        notes.append(
                            f"extra    {qualified} accepts {', '.join(extra)} beyond "
                            f"Fabric's signature — {EXTRA_PARAMS[qualified]}")
                    else:
                        failures.append(
                            f"{qualified} accepts {', '.join(extra)}, which Fabric's "
                            "signature does not. Declare it in EXTRA_PARAMS with the "
                            "reason, or drop it: a notebook written here would fail there.")
                # DERIVED, not declared: ours agrees with the docs and the stub
                # does not, so the stub is stale. No human writes this down.
                if stub_sig is not None and stub_sig != doc_sig:
                    notes.append(
                        f"stub     {qualified}: stub({', '.join(stub_sig)}) "
                        f"docs({', '.join(doc_sig)}) — we follow the documentation")
                continue

            # Undocumented but shipped by Microsoft and present in ours: fine,
            # and worth saying, because the transcription did not reach it.
            if stub_sig is not None and our_sig != stub_sig:
                failures.append(
                    f"{qualified} is undocumented surface we implement, and our signature "
                    f"differs from the stub: stub({', '.join(stub_sig)}) "
                    f"ours({', '.join(our_sig)}). With no doc page to arbitrate, the stub "
                    "is the only source there is.")

    for qualified in sorted(set(WAIVED) - seen_waived):
        failures.append(f"WAIVED lists {qualified}, which is no longer a gap. Drop it.")
    for qualified in sorted(set(PLANNED) - seen_planned):
        failures.append(f"PLANNED lists {qualified}, which is no longer a gap. Drop it.")
    for qualified in sorted(set(EXTRA_PARAMS) - seen_extra):
        failures.append(
            f"EXTRA_PARAMS lists {qualified}, which no longer takes anything beyond the "
            "documented signature. Drop it.")
    return failures, notes


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--strict", action="store_true")
    args = parser.parse_args(argv)
    try:
        failures, notes = problems()
    except Unreadable as exc:
        print(f"notebookutils-surface: {exc}")
        return 1

    stubs = stub_modules()
    checked = sorted(FABRIC_MODULES & set(stubs))
    total = sum(len(stubs[m]) for m in checked)
    print("notebookutils surface (pinned stubs, Fabric-documented modules only):")
    print(f"  modules checked   : {len(checked)} of {len(stubs)} in the stub "
          f"({len(NOT_FABRIC_MODULES)} are not Fabric surface)")
    print(f"  members compared  : {total}")
    print(f"  waived (Synapse)  : {len(WAIVED)}")
    print(f"  known gaps        : {len(PLANNED)}")
    print(f"  extra parameters  : {len(EXTRA_PARAMS)}")

    if notes:
        print("\nDivergences. `stub` lines are the two Microsoft sources disagreeing "
              "and are derived, not declared:")
        for note in notes:
            print(f"  {note}")
    if failures:
        print("\nUndeclared:")
        for failure in failures:
            print(f"  {failure}")
        if args.strict:
            print("\nFAIL: the shim's surface and Microsoft's have diverged in a way "
                  "nothing declared. Every difference is allowed; none may be silent.")
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
