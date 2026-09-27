#!/usr/bin/env python3
"""Every timing-coupled construct in the Python test surface is bounded, or is recorded.

THE SIBLING OF `check_test_flakiness.py`, ONE LANGUAGE OVER. That checker holds
the Go suite; docs/60-test-flakiness.md recorded the gap it left open in the
first line of its own "what is not covered" section -- "the pytest, e2e and
portal suites. Out of scope, as stated above." This closes the larger part of
that gap on the same architecture, with the same contract, the same three
finding kinds and a ledger checked in both directions, so the two read as one
policy rather than two opinions.

WHY IT IS THE SAME POLICY. A test that sleeps a fixed interval and then reads a
verdict does not assert what it appears to assert. Its outcome is a function of
machine load: on a fast machine the thing under test has finished and the
assertion is real; on a loaded CI runner it has not been scheduled yet and the
assertion passes for a reason unrelated to the code. That is the worst kind of
green -- locally true, globally meaningless -- and it is invisible in review,
because `time.sleep` looks identical whether it sits inside a bounded polling
loop (the correct shape, and what almost all of this surface already does) or
alone before an assertion.

WHAT THIS SURFACE ACTUALLY LOOKED LIKE when the checker was written, measured
rather than assumed: 104 `time.sleep` call sites across 232 files (101 in `e2e/`,
2 in `python/tests/`, 1 in `python/fabric-target/tests/`), of which 59 sleep for
a second or more and 5 were genuinely unbounded. Nothing inspected any of them.
Two of the five were real defects and were fixed rather than recorded -- see
docs/60-test-flakiness.md -- and two are accepted with reasons.

WHY `ast` RATHER THAN THE GO CHECKER'S REGEX. That checker must count braces to
find a loop's extent, because it has no Go parser available; it learned the cost
of that the hard way, reporting a correctly bounded `for i := 0; i < 60; i++ {`
as unbounded because the line carried a trailing comment and the header pattern
anchored on `{$`. Python ships its own parser, so a loop's extent here is exact
and that WHOLE CLASS of false positive cannot occur. The bound shapes below are
therefore decided on the syntax tree, not on what a line happens to look like.

WHAT IS FLAGGED. The same three kinds the Go checker flags, deliberately:

  1. AN UNBOUNDED SLEEP -- a `time.sleep` that is not inside any loop. This is
     the bare-sleep-then-assert shape, and it is worst when the assertion is a
     NEGATIVE ("no run started"): sleeping 2s and finding nothing is consistent
     with two different worlds -- there is no trigger, or there is one that has
     not fired yet -- and only the first is what the test claims.

  2. AN UNBOUNDED POLL -- a sleep inside a loop that carries no deadline and no
     iteration bound. A spin with no ceiling does not fail when the awaited
     thing never happens; it hangs, and a hung job reads as "still pending" to
     anything watching it.

  3. A LONG SLEEP -- a single sleep of a second or more, even when correctly
     bounded. These are not wrong and none is asked to change. They are
     surfaced because their AGGREGATE cost is real and invisible at any one
     call site: 59 of them across a fleet of e2e suites, each inside a CI job
     budgeted at twenty-five minutes. Recording them makes a sixtieth a
     deliberate decision rather than an unnoticed one.

Kinds 1 and 2 are what the ledger exists to keep EMPTY; kind 3 is what it
exists to HOLD. That split is the Go checker's and is kept verbatim.

THE THREE BOUND SHAPES, all three verified against real files in this tree:

  (a) a `for` over a finite iterable -- `for _ in range(60)` -- is bounded by
      construction, since the iterable must exhaust. The known-infinite
      generators are excluded by name, because `for _ in itertools.count()` is
      a `for` loop that never ends.

  (b) a `while` whose TEST consults a clock (`while time.time() < end`) or a
      counter incremented in the body (`while tries < 60`). A bare comparison
      does NOT count: `while len(rows) < 3` spins forever if the rows never
      arrive, which is precisely kind 2.

  (c) a `while True` whose BODY enforces a deadline, either as
      `assert time.time() < end` or as `if time.monotonic() >= end: raise`.
      MANDATORY, not a refinement. `e2e/fabric-cicd/driver.py` has two of these
      (the LRO poll and the pipeline-job poll) and both are correct code that a
      header-only check reports as violations. The Go checker learned the same
      lesson and searches its loop BODY for a deadline rather than only the
      header. Getting this wrong makes the checker something people argue with
      rather than fix -- and it was not hypothetical here: the first draft
      recognised only the `assert` form and promptly flagged `e2e/waiting.py`'s
      own `wait_for`, the sanctioned helper, which raises at its deadline
      instead of asserting because an `assert` is stripped under `python -O`.

WHAT SHAPE (c) REQUIRES, AND WHY THAT LINE IS WHERE IT IS. The guard must CALL a
clock in its own test -- `time.time()`, `time.monotonic()` -- and must actually
leave the loop (`assert`, `raise`, `break`, `return`). Both halves are load
bearing, and `e2e/sail/driver.py`'s watchdog is why: it is an intentionally
infinite daemon heartbeat whose body reads

    if elapsed > STEP_BUDGET: ...; os._exit(1)

which fails both tests -- `elapsed` is a local computed earlier, so the test
calls no clock, and `os._exit` is neither a raise nor a break. So it stays
flagged and is RECORDED with the reason it is intentional, which is the right
outcome: an intentionally infinite loop should be declared, not silently
auto-detected as though it were bounded.

THE LEDGER KEY IS `file:symbol`, WITH A `<module>` FALLBACK. This is a real
divergence from the Go side and the reason is structural: the e2e drivers are
top-level scripts, so `e2e/adls-sdk/driver.py` has module-level statements with
no enclosing function at all, and Go has no such position. One `<module>` entry
therefore covers every module-level site in that file -- the same many-to-one
the Go ledger already carries for `tds_reflect_test.go`, which has two.

The symbol is the OUTERMOST enclosing function, not the innermost. A nested
helper that sleeps is named by the test that contains it, which is both more
useful in a report and what the Go checker already does for free (Go's nested
functions are anonymous closures, so its scan finds the enclosing `func Test...`).

Usage:
    check_python_test_flakiness.py            report findings, exit 0
    check_python_test_flakiness.py --strict   exit non-zero on unrecorded findings
"""
import ast
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
LEDGER = ROOT / "docs" / "python-test-flakiness.json"

# The two pytest `testpaths` from pyproject.toml, plus every e2e harness. These
# are the TEST surface; the shipped `fabric_target` package and the
# `notebookutils` shim are product code whose retries are a runtime concern
# rather than a flaky-test one.
SCAN_ROOTS = ("python/tests", "python/fabric-target/tests", "e2e")

# Directories with no test Python of ours in them. The reasoning is the Go
# checker's, carried over because it applies unchanged:
#
# `.claude` is in the list for a sharper reason than tidiness: it can hold git
# WORKTREES, i.e. a second checkout of this same repository nested inside it. A
# sweep that walked one would report findings against a sibling branch's copy of
# a file -- with a path that looks local and a line number that does not match
# anything in this tree. golangci-lint has been caught by the same nesting here.
#
# `build` is the Python-specific member of that same family, and it is not
# hypothetical: `python/fabric-target/build/lib/fabric_target/` is a CHECKED-IN
# setuptools staging copy of a package that also exists at
# `python/fabric-target/fabric_target/`. A sweep that walked it would report the
# same site twice under two paths, only one of which anyone edits.
SKIP_DIRS = {"node_modules", ".git", ".claude", "_site", "__pycache__",
             "third_party", "website", "build", "target", ".venv", "venv"}

# Sleep, in the spelling this surface uses: `time.sleep(...)`. Matched on the
# attribute rather than on a resolved import, because every site in the tree
# writes the qualified form.
_SLEEP_ATTRS = {"sleep"}

# Reading the clock. A loop consulting any of these has a real deadline
# available to it; one consulting none of them does not.
_CLOCK_ATTRS = {"time", "monotonic", "perf_counter", "monotonic_ns", "time_ns",
                "now", "utcnow"}

# Generators that make a `for` loop infinite, so shape (a) does not apply.
_INFINITE_ITER = {"count", "cycle", "repeat"}


def relkey(path, root=None):
    """A path as the ledger spells it: relative to the root, forward slashes.

    THE SEPARATOR IS NOT COSMETIC, and this is not defensive boilerplate -- the
    Go sibling shipped exactly this bug and it was expensive. The ledger is a
    checked-in JSON file, so its keys are written once and read on three
    platforms, while `str(path.relative_to(ROOT))` yields
    `e2e\\adls-sdk\\driver.py` on Windows and `e2e/adls-sdk/driver.py`
    everywhere else. That mismatch does not make the checker miss things -- it
    makes it report EVERYTHING, twice over: every site reads as unrecorded (no
    ledger key matches it) and every ledger entry reads as stale (nothing
    matched). Both halves of a both-directions check fire at once, on one
    platform only, which is precisely the shape that survives review behind a
    green Linux run. On the Go side it took the Windows leg of make-targets.yml
    and the Windows leg of the pytest job red while every POSIX leg stayed
    green, and nothing in that checker's own suite could see it.

    The Windows pytest leg runs THIS checker's test too, so the guard is written
    against `PureWindowsPath` explicitly rather than left to that leg to
    discover.

    An already-pure path is passed THROUGH rather than rebuilt, because
    `pathlib.PurePath(PureWindowsPath(...))` re-parses with the HOST's flavour
    and silently discards the Windows one -- `C:/repo/x` then has no root on
    POSIX, reads as relative, and the root is never stripped. That is the same
    class of mistake as the bug above, one layer in.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    root = ROOT if root is None else root
    r = root if isinstance(root, pathlib.PurePath) else pathlib.PurePath(root)
    return (p.relative_to(r) if p.is_absolute() else p).as_posix()


def python_test_files(root=None):
    """Every *.py under the scan roots that is ours."""
    base = ROOT if root is None else pathlib.Path(root)
    out = []
    for scan_root in SCAN_ROOTS:
        for path in sorted((base / scan_root).rglob("*.py")):
            rel = path.relative_to(base)
            if SKIP_DIRS.intersection(rel.parts):
                continue
            out.append(path)
    return out


def _is_sleep(node):
    """True if `node` is a `time.sleep(...)` call."""
    return (isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr in _SLEEP_ATTRS)


def _calls_clock(node):
    """True if the subtree reads a clock -- `time.time()`, `time.monotonic()`..."""
    for child in ast.walk(node):
        if (isinstance(child, ast.Call)
                and isinstance(child.func, ast.Attribute)
                and child.func.attr in _CLOCK_ATTRS):
            return True
    return False


def sleep_seconds(node):
    """The sleep interval in seconds, or None when it is not a literal.

    `time.sleep(min(float(resp.headers.get("Retry-After", "1")), 60))` is a real
    line in this repo: its interval is decided by a server at run time, so no
    static answer exists and the site is not claimed to be a long sleep. Silence
    here means "not known to be >= 1s", never "known to be short".
    """
    if not node.args:
        return None
    arg = node.args[0]
    if isinstance(arg, ast.Constant) and isinstance(arg.value, (int, float)):
        return float(arg.value)
    # `time.sleep(0.5 * 4)`: a literal times a literal.
    if (isinstance(arg, ast.BinOp) and isinstance(arg.op, ast.Mult)
            and isinstance(arg.left, ast.Constant)
            and isinstance(arg.right, ast.Constant)
            and isinstance(arg.left.value, (int, float))
            and isinstance(arg.right.value, (int, float))):
        return float(arg.left.value) * float(arg.right.value)
    return None


def _counters_incremented_in(loop):
    """Names the loop body augments -- `tries += 1` -- i.e. its counters."""
    out = set()
    for child in ast.walk(loop):
        if isinstance(child, ast.AugAssign) and isinstance(child.target, ast.Name):
            out.add(child.target.id)
    return out


def is_bounded(loop):
    """True if this loop carries a ceiling. The three shapes are in the docstring.

    Nested loops are each asked independently, and a sleep counts as bounded
    when ANY enclosing loop is -- an unbounded inner loop inside a bounded outer
    one still cannot hang the test, which is the property being checked.
    """
    # (a) a `for` over a finite iterable, bounded by construction -- unless the
    # iterable is one of the generators that never exhausts, because
    # `for _ in itertools.count()` is a `for` loop that never ends. The
    # exemption is therefore keyed on the ITERABLE, not on the keyword.
    if isinstance(loop, (ast.For, ast.AsyncFor)):
        iterated = loop.iter
        return not (isinstance(iterated, ast.Call)
                    and isinstance(iterated.func, ast.Attribute)
                    and iterated.func.attr in _INFINITE_ITER)

    # (b) a `while` whose TEST consults a clock, or a counter the body bumps.
    #
    # A bare comparison is deliberately NOT enough. `while len(rows) < 3` is a
    # Compare too, and it spins forever when the rows never arrive -- which is
    # exactly the unbounded poll this checker is for, not an exemption from it.
    if _calls_clock(loop.test):
        return True
    counters = _counters_incremented_in(loop)
    for child in ast.walk(loop.test):
        if isinstance(child, ast.Name) and child.id in counters:
            return True

    # (c) the BODY enforces a deadline. Two spellings, both real in this tree:
    #
    #     assert time.time() < end                 e2e/fabric-cicd/driver.py
    #     if time.monotonic() >= deadline: raise   e2e/waiting.py
    #
    # Both require a clock CALL in the guard's own test AND a statement that
    # leaves the loop. See the module docstring for why that pair of conditions
    # is what keeps the sail watchdog flagged rather than silently blessed.
    for child in ast.walk(loop):
        if isinstance(child, ast.Assert) and _calls_clock(child.test):
            return True
        if isinstance(child, ast.If) and _calls_clock(child.test):
            for inner in ast.walk(child):
                if isinstance(inner, (ast.Raise, ast.Break, ast.Return)):
                    return True
    return False


def findings_for(path, source):
    """Findings in one file, as {file, line, symbol, kind, snippet} dicts."""
    rel = relkey(path)
    lines = source.split("\n")
    tree = ast.parse(source)
    out = []

    def visit(node, loops, funcs):
        for child in ast.iter_child_nodes(node):
            child_loops, child_funcs = loops, funcs
            if isinstance(child, (ast.For, ast.AsyncFor, ast.While)):
                child_loops = (*loops, child)
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                child_funcs = (*funcs, child.name)

            if _is_sleep(child):
                # The OUTERMOST enclosing function names the test rather than a
                # nested helper; `<module>` for a top-level e2e driver
                # statement, a position Go has no equivalent of.
                symbol = funcs[0] if funcs else "<module>"
                seconds = sleep_seconds(child)
                kind = None
                if not loops:
                    kind = "unbounded-sleep"
                elif not any(is_bounded(loop) for loop in loops):
                    kind = "unbounded-poll"
                elif seconds is not None and seconds >= 1:
                    kind = "long-sleep"
                if kind:
                    out.append({
                        "file": rel,
                        "line": child.lineno,
                        "symbol": symbol,
                        "kind": kind,
                        "snippet": (lines[child.lineno - 1].strip()
                                    if child.lineno - 1 < len(lines) else ""),
                    })

            # `visit`, not `ast.walk`: the loop and function stacks have to be
            # carried down, which is the whole reason the extents are exact.
            visit(child, child_loops, child_funcs)

    visit(tree, (), ())
    return sorted(out, key=lambda f: f["line"])


def scan(files=None):
    """Every finding across the tree."""
    out = []
    for path in (python_test_files() if files is None else files):
        out.extend(findings_for(path, path.read_text(encoding="utf-8")))
    return out


def load_ledger(ledger=None):
    """The accepted sites, keyed file:symbol.

    Keyed on the SYMBOL rather than the line number deliberately: a line number
    goes stale on any edit above it, and a ledger that must be renumbered to
    stay valid is a ledger people delete entries from.
    """
    path = LEDGER if ledger is None else ledger
    if not path.exists():
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    return {f"{e['file']}:{e['symbol']}": e for e in data.get("accepted", [])}


def main(argv):
    strict = "--strict" in argv[1:]
    files = python_test_files()

    # A sweep that walked nothing reports success. The same guard the Go sibling
    # carries, for the same reason: a check that inspects nothing passes for the
    # wrong reason, and it passes quietly.
    if not files:
        print("check_python_test_flakiness: walked zero Python files under "
              f"{', '.join(SCAN_ROOTS)} - a check that inspects nothing passes "
              "vacuously")
        return 1

    found = scan(files)
    accepted = load_ledger()

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
            print(f"{mark:9} {f['file']}:{f['line']} {f['symbol']} "
                  f"[{f['kind']}] {f['snippet']}")
        print(f"\ncheck_python_test_flakiness: {len(files)} Python files, "
              f"{len(found)} timing-coupled site(s), "
              f"{len(found) - len(unrecorded)} accepted, "
              f"{len(unrecorded)} not recorded")
        return 0

    problems = []
    if unrecorded:
        listing = "\n    ".join(
            f"{f['file']}:{f['line']} {f['symbol']} [{f['kind']}] {f['snippet']}"
            for f in unrecorded)
        problems.append(
            f"{len(unrecorded)} timing-coupled site(s) are neither bounded nor recorded "
            f"in {relkey(LEDGER)}:\n    {listing}\n"
            "  An unbounded sleep before an assertion makes the verdict a function of "
            "machine load. Poll inside a deadline (e2e/waiting.py wait_for), assert a "
            "negative across an explicit window (stays_empty), or record the site with "
            "its reason.")
    if stale:
        problems.append(
            f"{len(stale)} ledger entr(y/ies) name a site that is no longer flagged; a "
            "stale allowance is how a ban quietly stops applying:\n    "
            + "\n    ".join(stale))

    if problems:
        print("check_python_test_flakiness: " + "\n\n".join(problems))
        return 1

    print(f"check_python_test_flakiness: {len(files)} Python files scanned; "
          f"{len(found)} timing-coupled site(s), all {len(accepted)} recorded in "
          f"{relkey(LEDGER)} with a reason")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
