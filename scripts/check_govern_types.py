#!/usr/bin/env python3
"""Every Delta type the emulator can emit must survive the catalog ingest.

OpenMetadata refuses a table outright — 400, the whole table, not a warning on
the column — when a `char`/`varchar`/`binary`/`varbinary` column carries no
`dataLength`. That is a shape of failure this repo has already paid for once:
when `internal/warehouse` learned to report a Delta `binary` column honestly
instead of collapsing it to `string`, the governance job started failing with

    For column data types char, varchar, binary, varbinary dataLength must not
    be null

and the whole table vanished from the catalog. Nothing connected the two: the
change was to the SQL type map, the failure appeared in an OpenMetadata
container, and the only witness was a containerized e2e that needs the full
OpenMetadata stack (postgres, opensearch, the server) to run at all.

So this asserts the same property in a second, cheap place. It imports the real
ingest and drives it over every type in its own map, which is what makes it
survive a new type being added: a Delta type the emulator can emit but this
cannot map is exactly the case that breaks the e2e an hour later.

It does not replace the e2e — it cannot see anything about how OpenMetadata
actually behaves, only that the payload satisfies the constraint OpenMetadata
documents. It exists so that constraint is checked without containers.

Usage:
    check_govern_types.py     exit non-zero on the first column OM would refuse
"""
import importlib
import importlib.util
import pathlib
import sys
import types

ROOT = pathlib.Path(__file__).resolve().parent.parent
INGEST = ROOT / "scripts" / "govern_ingest.py"


def _stub_missing(name, build):
    """Install a stand-in for `name` ONLY when it is genuinely absent.

    govern_ingest imports requests/urllib3/yaml at module scope for the HTTP
    work, none of which the column mapping touches. Those live in the
    `governance` dependency group, so importing the module unconditionally made
    `make check` fail with ModuleNotFoundError on any machine without that group
    — which is how this guard, added to catch a governance break, became one.

    Stubbing only what is missing means the real modules are used wherever they
    exist, and the stub provides exactly the names used at import time: anything
    else raises AttributeError rather than silently doing nothing.
    """
    try:
        importlib.import_module(name)
    except ImportError:
        sys.modules[name] = build()


def load_ingest():
    _stub_missing("requests", lambda: types.ModuleType("requests"))
    _stub_missing("yaml", lambda: types.ModuleType("yaml"))

    def _urllib3():
        mod = types.ModuleType("urllib3")
        exc = types.ModuleType("urllib3.exceptions")
        exc.InsecureRequestWarning = type("InsecureRequestWarning", (Warning,), {})
        mod.exceptions = exc
        mod.disable_warnings = lambda *a, **k: None
        sys.modules["urllib3.exceptions"] = exc
        return mod

    _stub_missing("urllib3", _urllib3)

    spec = importlib.util.spec_from_file_location("govern_ingest", INGEST)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main() -> int:
    gi = load_ingest()
    problems = []

    # Every Delta primitive the ingest claims to understand, plus the
    # parameterised spellings the emulator actually writes.
    cases = [*gi.TYPE_MAP, "decimal(19,4)", "decimal(38,0)"]
    for delta_type in cases:
        col = gi.om_column("c", delta_type)
        dt = col["dataType"]
        if dt in gi.LENGTH_REQUIRED and col.get("dataLength") is None:
            problems.append(
                f"{delta_type!r} -> {dt} with no dataLength; OpenMetadata "
                f"rejects the whole table for this")
        if (dt == "DECIMAL" and "(" in delta_type
                and (col.get("precision") is None or col.get("scale") is None)):
                problems.append(
                    f"{delta_type!r} -> DECIMAL without precision/scale; the "
                    f"scale is the thing a decimal column is for")

    # A nested type still has to produce something legal for its children.
    nested = gi.om_column("s", {"type": "struct", "fields": [
        {"name": "b", "type": "binary", "nullable": True}]})
    for child in nested.get("children", []):
        if child["dataType"] in gi.LENGTH_REQUIRED and child.get("dataLength") is None:
            problems.append(
                f"struct child {child['name']!r} -> {child['dataType']} with no "
                f"dataLength")

    print(f"catalog column mapping: {len(cases)} Delta types checked")
    if problems:
        print("\nColumns OpenMetadata would refuse:")
        for p in problems:
            print(f"  {p}")
        print("\nFAIL: fix scripts/govern_ingest.py — a column OpenMetadata "
              "refuses costs the whole table, and the only other witness is a "
              "containerized e2e.")
        return 1
    print("every mapped type produces a column OpenMetadata accepts")
    return 0


if __name__ == "__main__":
    sys.exit(main())
