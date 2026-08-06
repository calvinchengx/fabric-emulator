#!/usr/bin/env python3
"""Prove that declaring an event kind in Go breaks the portal's build.

scripts/gen_event_kinds.py emits `EventKind` as a union so that a `switch` in
the portal which forgot a kind cannot compile. That is a claim about a compiler,
and a claim about a compiler that nothing exercises is a claim nobody checked —
a `default:` arm, a stray `as any`, or `strict` quietly switched off would each
make the guarantee evaporate with every test still green.

So this MUTATES the contract and requires the build to fail. Two mutations, for
the two ways a kind can go unhandled:

  1. a kind in AllKinds *and* ViewKinds, which the log must know how to render
     — caught by `describe()`, whose default arm narrows to `never`;
  2. a kind in AllKinds *only*, which is neither a view kind nor `dropped`
     — caught by `ingest()`, which has nowhere to put it.

Each mutation is bracketed by a clean run, so a check that fails for some
unrelated reason cannot pass for the right one.

Usage:
    check_kind_exhaustiveness.py          run every mutation
    check_kind_exhaustiveness.py -v       show the type errors it provoked
"""
import pathlib
import re
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
BUS = ROOT / "internal" / "store" / "bus.go"
GEN = ROOT / "scripts" / "gen_event_kinds.py"
GENERATED = ROOT / "portal" / "src" / "eventKinds.ts"

# The kind that does not exist. Named so that a leaked mutation is obvious in a
# diff rather than looking like a feature someone half-landed.
GHOST = "ghost"
KIND = f"Kind{GHOST.capitalize()}"
CONST_LINE = (
    f'\t{KIND} = "{GHOST}" '
    f"// NOT REAL: check_kind_exhaustiveness.py inserted this\n"
)

VERBOSE = "-v" in sys.argv


def mutate(src: str, *, viewable: bool) -> str:
    """Add a kind Go declares, in AllKinds and optionally in ViewKinds."""
    out, n = re.subn(
        r'(\tKindDropped\s+= "dropped".*\n)', r"\1" + CONST_LINE, src, count=1
    )
    if n != 1:
        sys.exit("check_kind_exhaustiveness: no KindDropped constant to insert after")
    for name in ("AllKinds",) + (("ViewKinds",) if viewable else ()):
        out, n = re.subn(
            rf"(var {name} = \[\]string\{{\n)",
            rf"\1\t{KIND},\n",
            out,
            count=1,
        )
        if n != 1:
            sys.exit(f"check_kind_exhaustiveness: no `var {name} = []string{{` to extend")
    return out


def generate() -> None:
    subprocess.run([sys.executable, str(GEN)], cwd=ROOT, check=True,
                   stdout=subprocess.DEVNULL)


def type_check() -> tuple[bool, str]:
    """(did it pass, what it said). Types only — CSS warnings are not the gate."""
    r = subprocess.run(
        ["pnpm", "--filter", "fabric-emulator-portal", "check"],
        cwd=ROOT, capture_output=True, text=True,
    )
    return r.returncode == 0, r.stdout + r.stderr


def main() -> int:
    if not shutil.which("pnpm"):
        sys.exit("check_kind_exhaustiveness: needs pnpm and an installed portal "
                 "(`pnpm install`) — it drives svelte-check to prove a compile error")

    original = BUS.read_text(encoding="utf-8")
    generated = GENERATED.read_text(encoding="utf-8")
    failures = []
    try:
        ok, said = type_check()
        if not ok:
            print(said)
            sys.exit("check_kind_exhaustiveness: the portal does not type-check "
                     "BEFORE any mutation, so nothing here could prove anything")
        print("baseline: portal type-checks clean")

        for viewable, whose in ((True, "describe()"), (False, "ingest()")):
            where = "AllKinds and ViewKinds" if viewable else "AllKinds only"
            BUS.write_text(mutate(original, viewable=viewable), encoding="utf-8")
            generate()
            ok, said = type_check()
            if VERBOSE:
                print(said)
            if ok:
                failures.append(
                    f"a kind declared in {where} did NOT break the build — "
                    f"{whose} is no longer exhaustive over EventKind, so the next "
                    f"kind will reach the portal and render as nothing"
                )
                print(f"FAIL  {where}: build still passed")
            elif "Flow.svelte" not in said:
                failures.append(
                    f"a kind declared in {where} broke the build, but not in "
                    f"Flow.svelte — the failure this proves is not the one it "
                    f"claims to prove:\n{said}"
                )
                print(f"FAIL  {where}: failed elsewhere")
            else:
                print(f"ok    {where}: Flow.svelte stops compiling ({whose})")
    finally:
        BUS.write_text(original, encoding="utf-8")
        GENERATED.write_text(generated, encoding="utf-8")

    ok, said = type_check()
    if not ok:
        print(said)
        sys.exit("check_kind_exhaustiveness: the tree does not type-check after "
                 "restoring it — the mutation was not fully undone")

    sys.stdout.flush()  # so the verdicts above precede the reasons below
    for f in failures:
        print(f"\n{f}", file=sys.stderr)
    if failures:
        return 1
    print("restored: portal type-checks clean")
    return 0


if __name__ == "__main__":
    sys.exit(main())
