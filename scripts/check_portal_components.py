#!/usr/bin/env python3
"""Every vendored portal UI component directory is imported by something.

WHY IT EXISTS. The portal's `ui/` directory is VENDORED code: shadcn-svelte
components are copied in by a generator, not resolved from a package, so
nothing in the dependency graph holds an opinion about whether one is still
used. A component that stops being imported keeps compiling, keeps
type-checking, keeps passing vitest and keeps being scanned by Tailwind --
there is no error to notice and no diff to read, because the failure is an
import that is NOT there.

WHAT WAS MEASURED, at the commit that added this. Two of the ten directories
under portal/src/lib/components/ui were unreachable from the application:

  * `select/` -- 12 files wrapping bits-ui's Select primitive, with not one
    reference anywhere outside its own directory.
  * `separator/` -- 2 files, whose ONLY referrer in the entire repository was
    select/select-separator.svelte.

That pairing is the whole reason this check reads the way it does. `separator/`
was not dead on its own terms: it HAD a referrer, and a "is this name mentioned
anywhere" sweep would have reported it healthy right up until `select/` was
deleted -- and healthy afterwards too, because the sweep would have been
reading the dead directory's own file. Dead code holds dead code alive, and it
does it quietly.

So what is asserted is reachability from the APPLICATION, not mention-anywhere:
a referrer that is itself inside `ui/` does not count. Both directories fall
out of that one question together, which is the answer a reader wants.

DIRECTORY GRANULARITY, deliberately, and this is the decision most likely to be
re-litigated. A file-level version of this check would be stricter and WRONG:
card/card-action.svelte, table/table-caption.svelte and table/table-footer.svelte
are imported by their own directory's index.ts barrel and by nothing else,
because a barrel publishes a component's whole surface whether or not today's
pages use every part of it. Flagging those would produce findings nobody should
act on, and a guard whose output is argued with instead of fixed is the
`defer-in-loop` story scripts/check_perf_regressions.py records at length. The
directory is the unit a generator adds and a person deletes, so the directory
is the unit checked.

WHAT THIS DOES NOT CLAIM, so nobody reads more into a green run than is there:
that an imported component is RENDERED, or that every symbol its barrel exports
is reached. A directory imported once by one page passes. That is the honest
limit of a text sweep, and closing it would need a bundler's own module graph
-- which this deliberately does not take, for the same reason
check_vitest_test_flakiness.py scans text rather than importing a TypeScript
parser: these checkers run under $(PY) on the standard library alone, so
`make check` stays runnable with no Node and no installed portal.

Zero component directories, or zero application files, is ITSELF a finding. A
sweep that covers nothing passes vacuously, and it passes quietly -- the same
guard every sibling in this directory carries.

Usage:
    check_portal_components.py            report orphans, exit 0
    check_portal_components.py --strict   exit non-zero on any orphan
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent

# The vendored components, and the tree that must reach them. APP_ROOT
# CONTAINS UI_DIR: the application files are everything under portal/src that
# is not itself a vendored component, which is what makes select/ -> separator/
# fail to count as a referrer.
UI_DIR = "portal/src/lib/components/ui"
APP_ROOT = "portal/src"

# Svelte components, their barrels, and plain TypeScript/JavaScript. Nothing
# else under portal/src can carry an import.
SUFFIXES = (".svelte", ".ts", ".js")

# Same list, and the same reason, as the three flakiness checkers carry:
# `.claude` can hold a git WORKTREE -- a second checkout of this repository
# nested inside it -- and a sweep that walked one would read a sibling
# branch's copy of a file as a referrer here.
SKIP_DIRS = {".git", ".claude", "vendor", "node_modules", "_site", "dist"}


def relkey(path, root=None):
    """A path as a message should spell it: relative to the root, forward slashes.

    Same reasoning as every sibling checker's `relkey` -- a raw
    `str(path.relative_to(ROOT))` is backslash-separated on Windows, so one
    finding would read two ways across the three platforms this suite runs on.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    r = ROOT if root is None else root
    r = r if isinstance(r, pathlib.PurePath) else pathlib.PurePath(r)
    return (p.relative_to(r) if p.is_absolute() else p).as_posix()


def component_dirs(root=None):
    """Every vendored component directory, by name.

    A directory rather than a file, because that is the unit the shadcn
    generator adds and the unit a person deletes. See the module docstring for
    why a file-level sweep would flag barrel sub-components that are not
    defects.
    """
    base = (ROOT if root is None else pathlib.Path(root)) / UI_DIR
    if not base.is_dir():
        return []
    return sorted(d.name for d in base.iterdir()
                  if d.is_dir() and d.name not in SKIP_DIRS)


def app_files(root=None):
    """Every portal source file that is NOT itself a vendored component.

    The exclusion IS this checker rather than a tidiness measure: with `ui/`
    left in, select/select-separator.svelte counts as a referrer for
    separator/, and two directories that were dead together both report
    healthy.
    """
    base = ROOT if root is None else pathlib.Path(root)
    src = base / APP_ROOT
    if not src.is_dir():
        return []
    ui = base / UI_DIR
    out = []
    for path in sorted(src.rglob("*")):
        if not path.is_file() or path.suffix not in SUFFIXES:
            continue
        if SKIP_DIRS.intersection(path.relative_to(base).parts):
            continue
        if ui in path.parents:
            continue
        out.append(path)
    return out


def referrer(name):
    """An import of `components/ui/<name>`, however the path is spelled.

    `$lib/components/ui/card/index`, a bare `components/ui/card` and a relative
    `../ui/card/index.js` all match, because what is asserted is that the
    directory is REACHED -- not the alias a particular file happened to use.

    The trailing lookahead is load-bearing: without it `components/ui/select`
    would also be satisfied by a sibling directory named `select-native`, and a
    guard that can be satisfied by a DIFFERENT component than the one it is
    asking about is worse than no guard.
    """
    return re.compile(rf"components/ui/{re.escape(name)}(?![A-Za-z0-9_-])")


def orphans(root=None, files=None, names=None):
    """Component directories no application file imports, as a sorted list."""
    paths = app_files(root) if files is None else files
    texts = [p.read_text(encoding="utf-8", errors="replace") for p in paths]
    out = []
    for name in (component_dirs(root) if names is None else names):
        pat = referrer(name)
        if not any(pat.search(t) for t in texts):
            out.append(name)
    return out


def main(argv):
    strict = "--strict" in argv[1:]

    names = component_dirs()
    files = app_files()

    # Both vacuity guards BEFORE the sweep, and separately, because they have
    # different causes and deserve different messages. A moved `ui/` and a
    # moved `portal/src` both leave this reporting zero orphans, which is
    # indistinguishable from a healthy tree.
    if not names:
        print("check_portal_components: no component directories under "
              f"{UI_DIR} - either the portal moved (update UI_DIR) or this "
              "check now inspects nothing, which passes vacuously")
        return 1
    if not files:
        print("check_portal_components: no .svelte/.ts/.js files under "
              f"{APP_ROOT} outside {UI_DIR} - a sweep with no application "
              "files would report every component as an orphan, so this is a "
              "broken scan rather than a finding")
        return 1

    found = orphans(files=files, names=names)

    if not strict:
        for name in names:
            mark = "ORPHAN" if name in found else "used"
            print(f"{mark:8} {UI_DIR}/{name}/")
        print(f"\ncheck_portal_components: {len(names)} component "
              f"director(y/ies), {len(files)} application file(s), "
              f"{len(found)} orphan(ed)")
        return 0

    if found:
        listing = "\n    ".join(f"{UI_DIR}/{name}/" for name in found)
        print("check_portal_components: "
              f"{len(found)} vendored component director(y/ies) are imported "
              f"by no file outside {UI_DIR}:\n    {listing}\n"
              "  Vendored UI code has nothing in the dependency graph holding "
              "an opinion about it, so an unimported component compiles, "
              "type-checks and ships forever. Either import it or delete the "
              "directory. A referrer INSIDE ui/ does not count: select/ and "
              "separator/ were dead together, and separator/ had a referrer "
              "the whole time.")
        return 1

    print(f"check_portal_components: {len(names)} component director(y/ies) "
          f"under {UI_DIR}, every one imported by at least one of "
          f"{len(files)} application file(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
