#!/usr/bin/env python3
"""Every `setTimeout`-based wait in the portal's vitest suite is bounded by a
fake clock, or is recorded as accepted with the reason.

WHY THIS EXISTS. docs/60-test-flakiness.md closed this gap for the Go suite
(scripts/check_test_flakiness.py) and the Python one
(scripts/check_python_test_flakiness.py), and named what was left in its own
words: "the portal's vitest suite ... out of scope as a separate toolchain."
This is that third checker. A test that waits a fixed real-clock interval and
then reads a verdict does not assert what it appears to: on a fast machine the
component has already reacted and the assertion is real, and on a loaded CI
runner it has not been scheduled yet and the assertion passes for an unrelated
reason. That is invisible in review, because a bare `setTimeout` reads the same
whether the suite is actually racing the wall clock or not.

WHAT THIS SUITE ACTUALLY LOOKS LIKE, measured rather than assumed: one
real-clock `setTimeout` across 24 `*.test.ts` files
(Flow.test.ts: "counts a dropped notice that does not say how many"), and nine
places across five files where `vi.useFakeTimers()` drives the clock instead
(advanced explicitly with `vi.advanceTimersByTimeAsync`, never raced against the
real one). The one real-clock site asserted a NEGATIVE -- the dropped-count chip
must not appear -- which is exactly the shape a fixed sleep cannot prove: it
only shows the chip was absent AT THE INSTANT checked, not for the whole window.
It was rewritten onto `staysAbsent` (src/testing.ts), which polls the probe
across the window and fails the moment it stops being empty, the same repair
the Go sweep made onto `StaysFalse` and the Python one onto `stays_empty`. So
this checker, like both siblings at the commit that added them, guards a line
that is currently HELD -- see python/tests/test_check_vitest_test_flakiness.py
for the violations it is driven against instead.

WHAT IS FLAGGED. Every `setTimeout(` call inside a `*.test.ts` file that is not
under `vi.useFakeTimers()` control at that point in the file:

  * `bare-sleep` -- the shape this ledger exists to keep EMPTY. `staysAbsent`
    covers the one legitimate use this suite has (asserting an absence over a
    window); anything else wanting a fixed real-clock wait belongs on the fake
    clock instead (`vi.useFakeTimers()` + `vi.advanceTimersByTimeAsync`), which
    this checker does not flag at all -- advancing a virtual clock is
    deterministic and races nothing.
  * `long-sleep` -- a `bare-sleep` of a second or more. None exist today; the
    kind is defined so a slow one lands as a decision recorded here rather than
    an unnoticed cost, exactly as the Go and Python ledgers use theirs.

FAKE-TIMER STATE IS TRACKED IN FILE ORDER, TOP TO BOTTOM -- `vi.useFakeTimers()`
turns it on, `vi.useRealTimers()` turns it off, real timers are the default at
the top of every file (vite.config.ts sets no global fake-timer config). That is
a straight-line reading of a file that is not itself straight-line control flow,
the same simplification the Go checker's brace-counter makes about loop extent.
It is exactly right for every file in this tree today, where each block that
switches to fake timers switches back before the next real-clock wait, and it
would misread a file that toggled the clock from a shared helper called by
several tests rather than inline -- there is no such helper here, and this
docstring is where a future one would need to be weighed against the checker
gaining that ability.

WHY A LEDGER RATHER THAN A FLAT BAN. The same reason as both siblings:
`docs/vitest-test-flakiness.json` records each accepted site with its bucket and
reason, in `--strict` mode the checker fails only on sites NOT in the ledger,
and the ledger is checked in BOTH directions -- an entry naming a site no longer
flagged is as loud as an unrecorded one, because a stale allowance is how a ban
quietly stops applying and it is the direction a "what's new" reader would never
think to check.

Usage:
    check_vitest_test_flakiness.py            report findings, exit 0
    check_vitest_test_flakiness.py --strict    exit non-zero on unrecorded findings
"""
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
LEDGER = ROOT / "docs" / "vitest-test-flakiness.json"

# The vitest suite lives here alone (vite.config.ts: `include: ['src/**/*.test.ts']`).
# e2e/ and the Playwright smoke suite (`portal/smoke/`) are a different
# toolchain against a real browser and carry no `vi.useFakeTimers` at all.
SCAN_ROOTS = ("portal/src",)

# Directories with no vitest suite of ours in them, for the same reason the Go
# and Python checkers carry this list: `.claude` can hold a git WORKTREE, a
# second checkout of this repository nested inside it, and a sweep that walked
# one would report findings against a sibling branch's copy of a file.
SKIP_DIRS = {"node_modules", ".git", ".claude", "_site", "dist", "build", "coverage", ".svelte-kit"}

_SLEEP = re.compile(r"\bsetTimeout\s*\(")
_FAKE_ON = re.compile(r"\bvi\.useFakeTimers\s*\(")
_FAKE_OFF = re.compile(r"\bvi\.useRealTimers\s*\(")
# `it('title', ...)` / `test('title', ...)` / `it.skip('title', ...)`, the
# callback body opening on the same line. Every one of this tree's 239 already
# does; requiring the trailing `{` (mirroring the Go checker's `_FOR` anchor)
# is what keeps a brace-less arrow body (`it('x', () => expect(...))`, which
# this tree has none of) from being read as a block whose extent runs until
# some unrelated later line happens to zero the brace count out.
#
# The title group is `(?:\\.|(?!\1)[^\\])*`, NOT the more obvious
# `(?:\\.|(?!\1).)*` -- CodeQL flagged the latter as exponential-backtracking
# on a title with many backslashes: `.` and `\\.` both match a lone backslash,
# so a run of them can be split between the two alternatives in exponentially
# many ways before the engine gives up. Excluding `\\` from the plain-char
# branch makes the two alternatives disjoint on their first character, which
# is what removes the ambiguity rather than merely making it less likely.
_IT = re.compile(r"^\s*(?:it|test)(?:\.\w+)?\s*\((['\"`])((?:\\.|(?!\1)[^\\])*)\1.*\{\s*$")
# A `setTimeout(fn, 1500)` / `setTimeout(fn, 2 * 1000)`-shaped second argument.
# Deliberately narrow -- a duration built from a named constant is not matched
# and is not currently in this tree, so it reads as a plain `bare-sleep`
# instead of silently passing as a value this cannot see.
_DURATION = re.compile(r"setTimeout\(\s*[^,()]*,\s*(\d+)\s*\)")


def relkey(path, root=None):
    """A path as the ledger spells it: relative to the root, forward slashes.

    Same reasoning as the Go and Python checkers' `relkey` -- a raw
    `str(path.relative_to(ROOT))` is backslash-separated on Windows, and a
    ledger keyed on that separator reads every real site as unrecorded and
    every real entry as stale on that platform alone.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    r = ROOT if root is None else root
    r = r if isinstance(r, pathlib.PurePath) else pathlib.PurePath(r)
    return (p.relative_to(r) if p.is_absolute() else p).as_posix()


def missing_scan_roots(root=None):
    """Scan roots that are not there -- i.e. that this sweep did not look at.

    `portal/src` is the only root, so this is a single-item guard rather than
    the Python checker's per-root subtraction -- but the failure it catches is
    the same one: a renamed or removed root would leave `vitest_test_files`
    walking nothing and `main` reporting the vacuous-success case below for a
    reason that looks identical from the outside (zero files) but has a
    different, more silent cause (the directory moved, rather than the suite
    having none).
    """
    base = ROOT if root is None else pathlib.Path(root)
    return [r for r in SCAN_ROOTS if not (base / r).is_dir()]


def vitest_test_files(root=None):
    """Every `*.test.ts` under the scan roots that is ours."""
    base = ROOT if root is None else pathlib.Path(root)
    out = []
    for scan_root in SCAN_ROOTS:
        d = base / scan_root
        if not d.is_dir():
            continue
        for path in sorted(d.rglob("*.test.ts")):
            rel = path.relative_to(base)
            if SKIP_DIRS.intersection(rel.parts):
                continue
            out.append(path)
    return out


def _strip_comments(text):
    """Block and line comments blanked out, newline-for-newline so every line
    number after them is unaffected. Strings are left INTACT -- this pass feeds
    `its_of`'s title match, and a comment can defeat its `\\{\\s*$` anchor
    (`it('x', () => { // note` does not end the line at `{`) while a string
    never legitimately follows the callback's opening brace. Crude on purpose,
    in the Go checker's `_brace_depth` tradition: TypeScript's grammar is not
    being parsed here, only enough of its syntax to find a title and a block.
    """
    text = re.sub(r"/\*.*?\*/", lambda m: "\n" * m.group(0).count("\n"),
                  text, flags=re.DOTALL)
    return "\n".join(re.sub(r"//.*$", "", line) for line in text.split("\n"))


def _strip_strings(text):
    """String and template literals blanked out, comments assumed already gone.

    A SEPARATE pass from `_strip_comments`, and deliberately run only where the
    title text does not matter: `its_of` needs the comment strip (so a trailing
    `// note` after the block's opening `{` does not defeat the end-of-line
    anchor) but NOT this one, because blanking `'flakes on load'` would blank
    the very title `its_of` exists to read. Everything else -- brace-depth
    counting and the `setTimeout`/`vi.useFakeTimers` detection below -- wants
    both passes, so a match inside a quoted snippet or a mock JSON payload is
    not mistaken for the real thing.
    """
    out = []
    for line in text.split("\n"):
        line = re.sub(r'"(?:[^"\\]|\\.)*"', '""', line)
        line = re.sub(r"'(?:[^'\\]|\\.)*'", "''", line)
        line = re.sub(r"`(?:[^`\\]|\\.)*`", "``", line)
        out.append(line)
    return "\n".join(out)


def _brace_depth(line):
    return line.count("{") - line.count("}")


def its_of(title_lines, clean_lines):
    """[(start, end exclusive, title)] for every `it`/`test` block.

    Matched against `title_lines` -- comments stripped, strings intact -- so
    the captured title is the real one; the block's EXTENT is then measured
    from `clean_lines` -- comments AND strings stripped -- so a brace inside a
    later mock payload on either side of the boundary cannot shift it.

    Nested loops of these do not occur in this suite (an `it` inside an `it` is
    not a shape vitest gives meaning to), so unlike the Go checker's loop
    extents this list needs no nesting story -- each site falls inside at most
    one.
    """
    out = []
    for i, line in enumerate(title_lines):
        match = _IT.match(line)
        if not match:
            continue
        depth, end = 0, len(clean_lines)
        for j in range(i, len(clean_lines)):
            depth += _brace_depth(clean_lines[j])
            if depth <= 0 and j > i:
                end = j + 1
                break
        out.append((i, end, match.group(2)))
    return out


def findings_for(path, source=None):
    """Findings in one file, as {file, line, symbol, kind, snippet} dicts."""
    text = source if source is not None else path.read_text(encoding="utf-8")
    raw_lines = text.split("\n")
    title_lines = _strip_comments(text).split("\n")
    clean_lines = _strip_strings("\n".join(title_lines)).split("\n")
    its = its_of(title_lines, clean_lines)
    rel = relkey(path)

    def symbol_at(i):
        # The line is a `setTimeout(` call and describe blocks are not
        # `it`/`test` blocks, so at most one entry in `its` can contain it.
        for start, end, title in its:
            if start <= i < end:
                return title
        return "<module>"

    out = []
    fake = False
    for i, line in enumerate(clean_lines):
        if _FAKE_ON.search(line):
            fake = True
            continue
        if _FAKE_OFF.search(line):
            fake = False
            continue
        if fake or not _SLEEP.search(line):
            continue
        match = _DURATION.search(line)
        ms = int(match.group(1)) if match else None
        kind = "long-sleep" if ms is not None and ms >= 1000 else "bare-sleep"
        out.append({"file": rel, "line": i + 1, "symbol": symbol_at(i),
                     "kind": kind, "snippet": raw_lines[i].strip()})
    return out


def scan(files=None):
    """Every finding across the tree."""
    out = []
    for path in (files if files is not None else vitest_test_files()):
        out.extend(findings_for(path))
    return out


def ledger_key(item):
    """The ledger key for a finding or an accepted entry: file, symbol AND kind.

    Same three-part key as the Python checker's `ledger_key`, and the same
    reason: a key with no kind would let an accepted `bare-sleep` for a symbol
    also exempt a `long-sleep` added to that symbol later, silently widening
    what was actually reviewed.
    """
    missing = [k for k in ("file", "symbol", "kind") if not item.get(k)]
    if missing:
        raise KeyError(
            f"a flakiness entry is missing {', '.join(missing)}: {item!r}. "
            "Every entry needs file, symbol and kind -- the kind is what scopes "
            "the exemption to one shape, so an entry without one would exempt "
            "the symbol from every ban.")
    return f"{item['file']}:{item['symbol']}:{item['kind']}"


def load_ledger(ledger=None):
    """The accepted sites, keyed by `ledger_key`.

    Keyed on the symbol rather than the line number deliberately: a line number
    goes stale on any edit above it, and a ledger that must be renumbered to
    stay valid is a ledger people delete entries from.
    """
    path = LEDGER if ledger is None else ledger
    if not path.exists():
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    return {ledger_key(e): e for e in data.get("accepted", [])}


def main(argv):
    strict = "--strict" in argv[1:]

    # Checked BEFORE the "walked zero files" guard below, not after: with a
    # single scan root, a root that does not exist and a root that exists but
    # holds no *.test.ts both leave `files` empty, and the two causes deserve
    # different messages -- one names a moved or deleted directory, the other
    # says the suite itself vanished. Only this ordering can ever say the first
    # one; checked second, it would be unreachable dead code.
    gone = missing_scan_roots()
    if gone:
        print("check_vitest_test_flakiness: scan root(s) do not exist, so their "
              f"share of the surface was not inspected at all: {', '.join(gone)}\n"
              "  Either the directory moved -- update SCAN_ROOTS -- or it was "
              "removed. A partial sweep must not report success.")
        return 1

    files = vitest_test_files()

    # A sweep that walked nothing reports success. Same guard both siblings
    # carry, for the same reason: a check that inspects nothing passes for the
    # wrong reason, and it passes quietly.
    if not files:
        print("check_vitest_test_flakiness: walked zero *.test.ts files under "
              f"{', '.join(SCAN_ROOTS)} - a check that inspects nothing passes "
              "vacuously")
        return 1

    found = scan(files)
    accepted = load_ledger()

    unrecorded, matched = [], set()
    for f in found:
        key = ledger_key(f)
        if key in accepted:
            matched.add(key)
        else:
            unrecorded.append(f)

    stale = sorted(set(accepted) - matched)

    if not strict:
        for f in found:
            mark = "accepted" if ledger_key(f) in accepted else "NEW"
            print(f"{mark:9} {f['file']}:{f['line']} {f['symbol']} "
                  f"[{f['kind']}] {f['snippet']}")
        for key in stale:
            print(f"{'STALE':9} {key} - recorded, but no longer flagged")
        print(f"\ncheck_vitest_test_flakiness: {len(files)} test files, "
              f"{len(found)} timing-coupled site(s), "
              f"{len(found) - len(unrecorded)} accepted, "
              f"{len(unrecorded)} not recorded, "
              f"{len(stale)} ledger entr(y/ies) stale")
        return 0

    problems = []
    if unrecorded:
        listing = "\n    ".join(
            f"{f['file']}:{f['line']} {f['symbol']} [{f['kind']}] {f['snippet']}"
            for f in unrecorded)
        problems.append(
            f"{len(unrecorded)} timing-coupled site(s) are neither on the fake "
            f"clock nor recorded in {relkey(LEDGER)}:\n    {listing}\n"
            "  A real-clock setTimeout before an assertion makes the verdict a "
            "function of machine load. Drive the clock instead "
            "(vi.useFakeTimers + vi.advanceTimersByTimeAsync), assert an "
            "absence across an explicit window (src/testing.ts staysAbsent), "
            "or record the site with its reason.")
    if stale:
        problems.append(
            f"{len(stale)} ledger entr(y/ies) name a site that is no longer "
            "flagged; a stale allowance is how a ban quietly stops applying:\n"
            "    " + "\n    ".join(stale))

    if problems:
        print("check_vitest_test_flakiness: " + "\n\n".join(problems))
        return 1

    print(f"check_vitest_test_flakiness: {len(files)} test files scanned; "
          f"{len(found)} timing-coupled site(s), all {len(accepted)} recorded "
          f"in {relkey(LEDGER)} with a reason")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
