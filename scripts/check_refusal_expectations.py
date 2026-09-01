#!/usr/bin/env python3
"""Contract 8's expectations, held against a source that is not our own typing.

WHY THIS EXISTS. Contract 8 grades the emulator against `REFUSAL_EXPECTATIONS`
-- the error a caller must be able to branch on when an operation is refused.
That map is a hand transcription of somebody else's contract, and it is the
only copy. A transcription with one source cannot be wrong out loud: if
Microsoft renames a code or drops a refusal, the map keeps asserting the old
one and the probe keeps passing. **Its failure direction is a false GREEN**,
which is the one nobody notices -- and contract 8 exists precisely to catch the
permissive direction, so a stale expectation blinds the probe built to see.

This is contract 2's shape applied to contract 8. There, `notebookutils`
members are transcribed from Learn AND checked against the `dummy-notebookutils`
wheel Microsoft publishes, with the divergence COMPUTED rather than declared;
holding two sources together turned "we transcribed carefully" into 15 real
gaps. The same move is available here and costs no new dependency.

WHAT BACKS EACH CASE, and the honest answer differs per case:

  * the ADLS Gen2 wire code is enumerated by Microsoft's own SDK.
    `StorageErrorCode` carries 165 codes including `DirectoryNotEmpty`. It is
    read from `third_party/azure-storage-error-codes/` rather than imported:
    every guard here runs under $(PY), which is standard library only, and the
    first CI job without uv on PATH failed on this check rather than on what it
    checks. Vendoring is also what makes the pin auditable -- PROVENANCE.md
    carries the version and the hash of the bytes parsed.
    Azurite would have been the better witness -- it is Microsoft's storage
    implementation -- but it serves Blob/Queue/Table and NOT ADLS Gen2, which
    e2e/azurite-shortcut's own compose says in its header. A DFS-specific
    refusal is exactly what it cannot answer.

  * the POSIX errno is enumerated by CPython, by running the operation.
    `fs.rm`'s docstring says the mapping to `os.rmdir`'s errno is the point, so
    the standard library is the authority for what that errno IS.

  * and one case has NO second source, which is stated rather than papered
    over. See SINGLE_SOURCED.

A NEW CASE WITH NO DECLARED SOURCE IS A FAILURE, not an omission -- the same
rule contract 8 applies to its own cases. Otherwise this check silently grades
whatever happened to be listed when it was written.

Usage:
    check_refusal_expectations.py     exit non-zero if a case disagrees or is unaccounted for
"""
import ast
import errno
import os
import pathlib
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
LIVE = ROOT / "e2e" / "conformance" / "live.py"
VENDORED = ROOT / "third_party" / "azure-storage-error-codes" / "models.py"

# Cases whose expectation this check cannot corroborate, WITH THE REASON. An
# entry here is a decision someone wrote down, not a gap nobody saw.
SINGLE_SOURCED = {
    "mv-onto-existing-without-overwrite":
        "no independent source. POSIX `os.rename` REPLACES the destination "
        "silently, so CPython contradicts this expectation rather than "
        "corroborating it -- refusing here is the shim's own contract, matching "
        "what notebookutils does rather than what the C library does. "
        "Microsoft's stub wheel cannot help either: its bodies are empty, so it "
        "enumerates the surface and never the behaviour.",
}


def expectations() -> dict[str, str]:
    """REFUSAL_EXPECTATIONS, parsed rather than imported.

    live.py imports pyspark and carries module-level session state; importing
    it to read one dict would make this check depend on a Spark install.
    """
    tree = ast.parse(LIVE.read_text(encoding="utf-8"))
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(
                getattr(t, "id", "") == "REFUSAL_EXPECTATIONS" for t in node.targets):
            return ast.literal_eval(node.value)
    raise SystemExit(f"REFUSAL_EXPECTATIONS not found in {LIVE}")


def storage_error_codes() -> set[str]:
    """The codes Microsoft enumerates, parsed from the vendored SDK file.

    `ast` rather than `import`: see the module docstring and PROVENANCE.md.
    The enum is `class StorageErrorCode(str, Enum)` with `NAME = "Value"`
    members, and it is the VALUES that go on the wire.
    """
    if not VENDORED.is_file():
        raise SystemExit(
            f"{VENDORED} is missing — contract 8's second source is vendored, "
            "not installed; see its PROVENANCE.md")
    tree = ast.parse(VENDORED.read_text(encoding="utf-8"))
    for node in ast.walk(tree):
        if isinstance(node, ast.ClassDef) and node.name == "StorageErrorCode":
            return {
                stmt.value.value
                for stmt in node.body
                if isinstance(stmt, ast.Assign)
                and isinstance(stmt.value, ast.Constant)
                and isinstance(stmt.value.value, str)
            }
    return set()


def posix_rmdir_errno() -> str:
    """What CPython ACTUALLY raises removing a non-empty directory."""
    with tempfile.TemporaryDirectory() as tmp:
        d = pathlib.Path(tmp) / "full"
        d.mkdir()
        (d / "child").write_text("x", encoding="utf-8")
        try:
            os.rmdir(d)
        except OSError as exc:
            return f"{type(exc).__name__}/{errno.errorcode.get(exc.errno, exc.errno)}"
    return "no error"


def main() -> int:
    exp = expectations()
    problems, notes = [], []

    # 1. The wire code, against Microsoft's enumeration.
    wire = "dfs-delete-non-empty-directory"
    if wire in exp:
        codes = storage_error_codes()
        if not codes:
            problems.append(
                f"{wire}: no codes parsed from {VENDORED.name} — the second "
                "source is unreadable, so the expectation is unchecked rather "
                "than confirmed")
        else:
            code = exp[wire].split()[-1]
            if code not in codes:
                problems.append(
                    f"{wire}: expects {code!r}, which is not among the "
                    f"{len(codes)} codes Microsoft's StorageErrorCode enumerates "
                    "— either the expectation is stale or the code was renamed")
            else:
                notes.append(f"{wire}: {code} confirmed against StorageErrorCode "
                             f"({len(codes)} codes)")

    # 2. The errno, against CPython.
    rm = "rm-non-empty-without-recurse"
    if rm in exp:
        if os.name != "posix":
            notes.append(f"{rm}: POSIX corroboration skipped on {os.name}")
        else:
            got = posix_rmdir_errno()
            if got != exp[rm]:
                problems.append(
                    f"{rm}: expects {exp[rm]!r} but CPython raises {got!r} "
                    "removing a non-empty directory")
            else:
                notes.append(f"{rm}: {got} confirmed against CPython")

    # 3. Nothing may be unaccounted for.
    checked = {wire, rm} | set(SINGLE_SOURCED)
    orphans = sorted(set(exp) - checked)
    if orphans:
        problems.append(
            f"no source declared for {orphans} — add a second source, or record "
            "it in SINGLE_SOURCED with the reason. A case nobody corroborates "
            "and nobody flags is the shape this check exists to prevent")

    for n in notes:
        print(f"  {n}")
    for name, why in sorted(SINGLE_SOURCED.items()):
        if name in exp:
            print(f"  {name}: SINGLE SOURCED — {why[:80]}…")
    if problems:
        print("check_refusal_expectations: " + "\n  ".join(["", *problems]),
              file=sys.stderr)
        return 1
    print(f"check_refusal_expectations: {len(exp)} expectations, "
          f"{len(exp) - len([s for s in SINGLE_SOURCED if s in exp])} corroborated "
          "by a source outside this repository")
    return 0


if __name__ == "__main__":
    sys.exit(main())
