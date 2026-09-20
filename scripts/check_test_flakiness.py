#!/usr/bin/env python3
"""Every timing-coupled construct in the Go suite is bounded, or is recorded as accepted.

WHY THIS EXISTS. A test that sleeps a fixed interval and then reads a verdict
does not assert what it appears to assert. Its outcome is a function of machine
load: on a fast machine the thing under test has finished and the assertion is
real; on a loaded CI runner it has not been scheduled yet and the assertion
passes for a reason unrelated to the code. That is the worst kind of green —
locally true, globally meaningless — and it is invisible in review, because a
`time.Sleep` looks the same whether it sits inside a bounded polling loop (the
correct shape, and what almost all of this suite already does) or alone before
an assertion.

WHAT THIS SUITE ACTUALLY LOOKED LIKE when the checker was written, measured
rather than assumed: 29 sleeps across 532 `*_test.go` files, of which 26 were
already inside bounded loops. The three that were not all asserted a NEGATIVE —
"no engine ran this job", "nothing was delivered after Close" — which is exactly
the shape where a fixed sleep is load-dependent, and all three were rewritten
onto `internal/testsupport.StaysFalse`. So this checker guards a line that is
currently HELD, which is the awkward case: a check that passes today passes
whether or not it works. Its own test (python/tests/test_check_test_flakiness.py)
therefore drives it with violations it must catch, in both directions.

WHAT IS FLAGGED.

  1. AN UNBOUNDED SLEEP — a `time.Sleep` that is not lexically inside a loop
     carrying a bound. Two bounds count, because this repo uses both:
       * a DEADLINE (`time.Now().Add(...)` / `time.After(...)` consulted in the
         loop), used by the polling helpers in internal/api and internal/store;
       * an ITERATION COUNT (`for i := 0; i < 60; i++`), used by the TDS suites
         retrying against a SQL Server that is still booting.
     Both make the failure case bounded and the success case immediate, which is
     the property that matters. Neither is preferred over the other here.

  2. AN UNBOUNDED POLLING LOOP — `for { ... }` or `for cond { ... }` that sleeps
     and carries NO deadline and no iteration bound. A spin with no ceiling does
     not fail when the thing it waits for never happens; it hangs, and a hung
     job reads as "still pending" to anything watching. That is the same failure
     `timeout-minutes` exists for one layer up.

  3. A LONG SLEEP — a single sleep of a second or more, even inside a bounded
     loop. These are not wrong and none are asked to change: the seven in the
     TDS suites back off against a SQL Server that is genuinely still booting,
     and a shorter interval would only poll a socket that is not listening. They
     are surfaced because their AGGREGATE cost is real and invisible at any one
     call site — seven retry loops at sixty seconds of worst case each, inside a
     CI job budgeted at twenty-five minutes. Recording them makes an eighth a
     deliberate decision rather than an unnoticed one. This is the category that
     the ledger exists to hold; the two above are the categories it exists to
     keep empty.

WHY A LEDGER RATHER THAN A FLAT BAN. Some unbounded sleeps are legitimate and
some are merely not worth rewriting today, and a checker with no escape hatch
gets switched off rather than satisfied. `docs/test-flakiness.json` records each
accepted site with its bucket and the reason it is accepted — the same
convention as docs/witnesses.json and docs/surface-ledger.json. In `--strict`
mode the checker fails only on sites that are NOT in the ledger, so an accepted
site costs nothing and a newly introduced one fails the build.

The ledger is checked in BOTH directions. An entry naming a site that no longer
exists is also a failure: a stale allowance is how a ban quietly stops applying,
and it is the direction a "flag what is new" checker would otherwise never look.

Usage:
    check_test_flakiness.py            report findings, exit 0
    check_test_flakiness.py --strict   exit non-zero on unrecorded findings
"""
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
LEDGER = ROOT / "docs" / "test-flakiness.json"

# Directories with no Go of ours in them. vendor/ is other people's code and
# node_modules is not Go at all; walking either would report findings nobody
# here can act on.
#
# `.claude` is in the list for a sharper reason than tidiness: it can hold git
# WORKTREES, i.e. a second checkout of this same repository nested inside it. A
# sweep that walked one would report findings against a sibling branch's copy of
# a file — with a path that looks local and a line number that does not match
# anything in this tree — and the first draft of this checker did exactly that.
# golangci-lint has been caught by the same nesting here before.
SKIP_DIRS = {"vendor", "node_modules", ".git", ".claude", "_site"}

_SLEEP = re.compile(r"\btime\.Sleep\(")
# A loop header. Go has exactly one loop keyword, so this is the whole set:
# `for {`, `for cond {`, `for i := 0; i < n; i++ {`, `for range x {`.
#
# Matched against the line with its trailing comment REMOVED. That is not a
# nicety: `for i := 0; i < 60; i++ { // SQL Server may still be starting` is a
# real line in internal/server/tds_strict_test.go, and anchoring on `{$`
# against the raw line missed it — so a properly bounded retry loop was
# reported as an unbounded sleep. A checker whose false positives land on
# correct code is one that gets argued with rather than fixed.
_FOR = re.compile(r"^\s*for\b(?P<header>.*?)\{\s*$")
_LINE_COMMENT = re.compile(r"//.*$")
# The two bound shapes this repo uses. A deadline is consulted rather than
# merely declared, so the loop BODY is searched for it as well as the header.
_DEADLINE = re.compile(r"time\.Now\(\)|time\.After\(|<-\s*\w*[Dd]eadline|context\.WithTimeout|\.Done\(\)")
# `for i := 0; i < 60; i++` — a three-clause header is bounded by construction,
# since the counter must terminate. `for {` and `for cond {` are not.
_COUNTED = re.compile(r";.*;")
# `for range n` / `for i := range n` — bounded by the range expression.
_RANGE = re.compile(r"\brange\b")
# A sleep whose interval is a second or more, in the spellings this tree uses:
# `time.Second`, `2 * time.Second`, `1500 * time.Millisecond`.
_LONG_SLEEP = re.compile(
    r"time\.Sleep\(\s*(?:(?P<n>\d+)\s*\*\s*)?time\.(?P<unit>Second|Minute|Millisecond)\b")


def is_long_sleep(line):
    """True if the line sleeps for >= 1 second."""
    match = _LONG_SLEEP.search(line)
    if not match:
        return False
    count = int(match.group("n") or 1)
    unit_ms = {"Millisecond": 1, "Second": 1000, "Minute": 60000}[match.group("unit")]
    return count * unit_ms >= 1000


def relkey(path, root=None):
    """A path as the ledger spells it: relative to the root, forward slashes.

    THE SEPARATOR IS NOT COSMETIC. The ledger is a checked-in JSON file, so its
    keys are written once and read on three platforms, while
    `str(path.relative_to(ROOT))` yields `internal\\server\\x_test.go` on Windows
    and `internal/server/x_test.go` everywhere else. That mismatch does not make
    the checker miss things -- it makes it report EVERYTHING, twice over: every
    site reads as unrecorded (no ledger key matches it) and every ledger entry
    reads as stale (nothing matched). Both halves of a both-directions check
    fire at once, on one platform only, which is precisely the shape that
    survives review on a green Linux run. It failed the Windows leg of
    make-targets.yml and the Windows leg of the pytest job, and nothing in the
    checker's own suite could see it.

    An already-pure path is passed THROUGH rather than rebuilt, because
    `pathlib.PurePath(PureWindowsPath(...))` re-parses with the HOST's flavour
    and silently discards the Windows one -- `C:/repo/x` then has no root on
    POSIX, reads as relative, and the root is never stripped. That is the same
    class of mistake as the bug above, one layer in, and the test below caught
    it on the first run.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    root = ROOT if root is None else root
    r = root if isinstance(root, pathlib.PurePath) else pathlib.PurePath(root)
    return (p.relative_to(r) if p.is_absolute() else p).as_posix()


def go_test_files():
    """Every *_test.go under ROOT that is ours."""
    out = []
    for path in sorted(ROOT.rglob("*_test.go")):
        rel = path.relative_to(ROOT)
        if SKIP_DIRS.intersection(rel.parts):
            continue
        out.append(path)
    return out


def _brace_depth(line):
    """Net brace change for a line, ignoring braces inside string literals.

    Crude on purpose — Go's grammar is not being parsed here, only its block
    nesting, and the cases that would fool this (a brace inside a rune literal,
    a comment) do not change a loop's extent in any file in this tree. The
    checker's own test pins the shapes that matter.
    """
    stripped = re.sub(r'"(?:[^"\\]|\\.)*"', '""', line)
    stripped = re.sub(r"`[^`]*`", "``", stripped)
    stripped = re.sub(r"//.*$", "", stripped)
    return stripped.count("{") - stripped.count("}")


def loops_of(lines):
    """[(start index, end index exclusive, header, bounded?)] for every for-loop.

    Nested loops are all reported, so a sleep inside an inner unbounded loop
    that sits inside a bounded outer one still counts as bounded — the outer
    bound is what stops the test hanging, which is the property being checked.
    """
    out = []
    for i, line in enumerate(lines):
        match = _FOR.match(_LINE_COMMENT.sub("", line).rstrip())
        if not match:
            continue
        depth, end = 0, len(lines)
        for j in range(i, len(lines)):
            depth += _brace_depth(lines[j])
            if depth <= 0 and j > i:
                end = j + 1
                break
        header = match.group("header")
        body = "\n".join(lines[i:end])
        bounded = bool(
            _COUNTED.search(header)
            or _RANGE.search(header)
            or _DEADLINE.search(body)
        )
        out.append((i, end, header.strip(), bounded))
    return out


def findings_for(path, text=None):
    """Findings in one file, as {file, line, symbol, kind, snippet} dicts."""
    source = text if text is not None else path.read_text(encoding="utf-8")
    lines = source.split("\n")
    loops = loops_of(lines)
    rel = relkey(path)

    # Enclosing func, so a finding names the test rather than only a line.
    funcs = [(i, m.group(1))
             for i, line in enumerate(lines)
             for m in [re.match(r"^func\s+(?:\([^)]*\)\s*)?(\w+)", line)] if m]

    def symbol_at(i):
        name = "?"
        for start, fn in funcs:
            if start <= i:
                name = fn
            else:
                break
        return name

    out = []
    for i, line in enumerate(lines):
        if not _SLEEP.search(line) or line.lstrip().startswith("//"):
            continue
        enclosing = [lp for lp in loops if lp[0] <= i < lp[1]]
        if not enclosing:
            out.append({"file": rel, "line": i + 1, "symbol": symbol_at(i),
                        "kind": "unbounded-sleep", "snippet": line.strip()})
        elif not any(lp[3] for lp in enclosing):
            out.append({"file": rel, "line": i + 1, "symbol": symbol_at(i),
                        "kind": "unbounded-poll", "snippet": line.strip()})
        elif is_long_sleep(line):
            out.append({"file": rel, "line": i + 1, "symbol": symbol_at(i),
                        "kind": "long-sleep", "snippet": line.strip()})
    return out


def scan():
    """Every finding across the tree."""
    out = []
    for path in go_test_files():
        out.extend(findings_for(path))
    return out


def load_ledger():
    """The accepted sites, keyed file:symbol.

    Keyed on the SYMBOL rather than the line number deliberately: a line number
    goes stale on any edit above it, and a ledger that must be renumbered to
    stay valid is a ledger people delete entries from.
    """
    if not LEDGER.exists():
        return {}
    data = json.loads(LEDGER.read_text(encoding="utf-8"))
    return {f"{e['file']}:{e['symbol']}": e for e in data.get("accepted", [])}


def main(argv):
    strict = "--strict" in argv[1:]
    found = scan()
    accepted = load_ledger()
    files = go_test_files()

    # A scan that walked nothing reports success. Same guard the shipped-
    # notebook sweep in internal/api carries, for the same reason.
    if not files:
        print("check_test_flakiness: walked zero *_test.go files — a check that "
              "inspects nothing passes vacuously")
        return 1

    unrecorded, matched = [], set()
    for f in found:
        key = f"{f['file']}:{f['symbol']}"
        if key in accepted:
            matched.add(key)
        else:
            unrecorded.append(f)

    stale = sorted(set(accepted) - matched)

    if not strict:
        for f in found:
            mark = "accepted" if f"{f['file']}:{f['symbol']}" in accepted else "NEW"
            print(f"{mark:9} {f['file']}:{f['line']} {f['symbol']} [{f['kind']}] {f['snippet']}")
        print(f"\ncheck_test_flakiness: {len(files)} test files, {len(found)} timing-coupled "
              f"site(s), {len(found) - len(unrecorded)} accepted, {len(unrecorded)} not recorded")
        return 0

    problems = []
    if unrecorded:
        lines = "\n    ".join(
            f"{f['file']}:{f['line']} {f['symbol']} [{f['kind']}] {f['snippet']}"
            for f in unrecorded)
        problems.append(
            f"{len(unrecorded)} timing-coupled site(s) are neither bounded nor recorded in "
            f"{relkey(LEDGER)}:\n    {lines}\n"
            "  An unbounded sleep before an assertion makes the verdict a function of machine "
            "load. Poll inside a deadline (internal/testsupport.WaitFor), assert a negative "
            "across an explicit window (StaysFalse), or record the site with its reason.")
    if stale:
        problems.append(
            f"{len(stale)} ledger entr(y/ies) name a site that is no longer flagged; a stale "
            f"allowance is how a ban quietly stops applying:\n    " + "\n    ".join(stale))

    if problems:
        print("check_test_flakiness: " + "\n\n".join(problems))
        return 1

    print(f"check_test_flakiness: {len(files)} test files scanned; "
          f"{len(found)} timing-coupled site(s), all {len(accepted)} recorded in "
          f"{relkey(LEDGER)} with a reason")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
