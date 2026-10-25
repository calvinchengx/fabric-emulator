#!/usr/bin/env python3
"""Every wall-clock read truncated to a DAY in test or e2e-harness code is
recorded, with the reason a run crossing UTC midnight cannot change its verdict.

WHY A FOURTH CHECKER. docs/60-test-flakiness.md's three existing flakiness
gates -- scripts/check_test_flakiness.py (Go), check_python_test_flakiness.py
and check_vitest_test_flakiness.py -- all ask the same question in three
languages: is this WAIT bounded? Their shared blind spot is named in §6's own
first bullet: "flakiness with no timing tell ... nothing here covers
within-test nondeterminism that the race detector cannot see." This is one
sharply-defined member of that class, and it was present in this tree twice.

THE DEFECT, measured rather than hypothesised. A test performed operations
whose audit events were stamped from the store's clock, THEN read the wall
clock a second time to compute "today", then asserted those events appear in
that day's window (internal/server/activityevents_test.go, and byte-for-byte
the same shape one surface over in e2e/az-rest/driver.py). The two reads are
independent, so a run that crosses UTC midnight between them queries a day the
events are not in and fails on a missing CreateWorkspace. There is no sleep to
flag, no shared memory for `-race` to instrument, and no cross-test ordering
for `-shuffle=on` to perturb -- the whole failure is two readings of one clock
landing on either side of a boundary. It fires in a sub-second window once per
day, which is exactly the "locally true, globally meaningless" shape the three
siblings were built for, on an axis none of them looks at.

WHAT IS FLAGGED -- ONE KIND, `day-granular-now`: a wall-clock read truncated to
day granularity in a test file or an e2e harness.

  * Go -- `time.Now()[.UTC()].Format("<layout>")` where the layout carries a
    date and NO time component, AND the two-statement spelling of the same
    thing: a local bound to a `time.Now()` chain, formatted later. The
    one-expression pattern alone was this checker's first version, and writing
    the oracle for internal/testsupport.UTCDay immediately produced the missed
    shape -- `before := time.Now().UTC()` then `before.Format("2006-01-02")`
    is the identical defect across two lines and reported GREEN. Same lesson
    R8 in check_security_footguns.py records about a bare name bound to
    `spark.sql`: a rule anchored on one expression shape walks past the
    assignment that spells it in two. A layout with a time in it (`"2006-01-02T15:04:05Z"`)
    is not day-granular: nothing about it is truncated, so there is no boundary
    to straddle. Only a STRING LITERAL layout is read; `time.DateOnly` and any
    other named constant are deliberately not resolved, the same narrowing the
    vitest checker's `_DURATION` makes, so an unseen shape reads as "no finding"
    rather than as a guess.
  * Python -- `datetime.now(...).strftime("<fmt>")`, `datetime.utcnow().strftime(...)`
    and bare `date.today()`, on the same date-without-a-time rule, with the
    same two-statement clause: a name this file assigns from a clock read,
    `strftime`-d later.

WHAT IS NOT IN SCOPE, stated so the silence is not mistaken for a sweep.
Production code is not scanned: a day-granular read in the emulator is a
product decision with its own tests, not a flaky assertion. The Go half scans
`*_test.go` only, which is also what exempts the repair itself --
`internal/testsupport/clock.go` reads the clock once on every caller's behalf,
exactly as `internal/testsupport/wait.go` is exempt from the Go sleep checker
and for the same reason: a helper is not a test, and a caller is trusting it
INSTEAD of writing its own read. That exemption is this checker's one real
blind spot and it is held elsewhere rather than here --
internal/testsupport/clock_test.go pins UTCDaysSince in both straddle
directions, because a regex can see that a day was read and cannot see whether
the straddle was handled.

TWO DIFFERENT PARSERS, for the reason both sibling docstrings give. Python
ships `ast`, so function extents and call shapes here are exact; Go does not,
so its half is a regex scan over comment-stripped source with an upward walk to
the enclosing `func`. The Go comment strip is hand-walked over Go's four
lexical states rather than regexed, because `"https://learn.microsoft.com/..."`
contains `//` -- the mistake check_comment_drift.py records making.

HOW THIS LEDGER DIFFERS FROM ITS THREE SIBLINGS, which is worth stating because
it is a real divergence and not an oversight. There, a repaired site STOPS
MATCHING: a bare sleep becomes a bounded poll and the pattern no longer fires.
Here a repaired read is still a read -- the repair is in what happens to the
value downstream, which no static scan of this kind can see. So the two
repaired sites do not appear below because they no longer read the clock
THEMSELVES (both now call a named straddle-aware helper), and the entries that
remain are reads whose verdict genuinely cannot turn on which day they name.
`docs/clock-test-flakiness.json` carries a bucket on each entry saying which
kind of safe it is.

Usage:
    check_clock_test_flakiness.py             report findings, exit 0
    check_clock_test_flakiness.py --strict    exit non-zero on unrecorded findings
"""
import ast
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
LEDGER = ROOT / "docs" / "clock-test-flakiness.json"

# The Go test surface, mirroring scripts/check_test_flakiness.py's roots.
GO_SCAN_ROOTS = ("internal", "pkg", "cmd")
# The Python surface, mirroring scripts/check_python_test_flakiness.py's: the
# two pytest `testpaths` plus every e2e harness.
PY_SCAN_ROOTS = ("python/tests", "python/fabric-target/tests", "e2e")
SCAN_ROOTS = GO_SCAN_ROOTS + PY_SCAN_ROOTS

# `.claude` can hold a git WORKTREE -- a second checkout of this repository
# nested inside it -- and a sweep that walked one would report findings against
# a sibling branch's copy of a file, at a line number matching nothing here.
# Both siblings carry this list for that measured reason. `build` is here
# because python/fabric-target/build/lib/ is a CHECKED-IN setuptools staging
# copy of a package that also exists one directory up, so walking it reports
# every site twice under two paths, only one of which anyone edits.
SKIP_DIRS = {"node_modules", ".git", ".claude", "_site", "__pycache__",
             "build", "dist", "vendor", "third_party", ".venv"}

KIND = "day-granular-now"

# Go reference-layout tokens that carry a TIME. A layout holding any of these is
# not truncated to a day, so there is no midnight boundary for it to straddle.
# `Z07`/`-07`/`+07` are the zone forms, which only appear on a timestamp layout.
_GO_TIME_TOKENS = ("15", "03", "04", "05", "PM", "pm", "Z07", "-07", "+07",
                   ".000", ".999", "\u00b5s", "15:04")
# ...and the tokens that make it a DATE at all. A pure time layout
# (`"15:04:05"`) is not a day read either.
_GO_DATE_TOKENS = ("2006", "06", "Jan", "January", "01", "02", "_2", "Mon")

# Python strftime directives, same split.
_PY_TIME_DIRECTIVES = ("%H", "%M", "%S", "%I", "%p", "%f", "%T", "%X", "%c",
                       "%Z", "%z", "%s")
_PY_DATE_DIRECTIVES = ("%Y", "%y", "%m", "%d", "%j", "%F", "%x", "%b", "%B",
                       "%a", "%A", "%G", "%U", "%W", "%V")

# `time.Now()` / `time.Now().UTC()` / `time.Now().In(z)` followed by
# `.Format("literal")`. The layout group is `[^"\\]*` -- no escape handling --
# because a reference layout containing a backslash is not a thing, and a
# narrow pattern that misses an exotic spelling is the documented trade above.
_GO_NOW_FORMAT = re.compile(
    r'\btime\.Now\(\)(?:\.(?:UTC|Local|In)\([^()]*\))?\.Format\(\s*"([^"\\]*)"\s*\)')
# The enclosing declaration, for the symbol half of the ledger key. The
# receiver group is skipped so a method and a plain func both yield the name.
_GO_FUNC = re.compile(r'^func\s+(?:\([^)]*\)\s*)?(\w+)')
# `now := time.Now()` / `var now = time.Now().UTC()` -- a name this FILE binds
# to a clock read. Resolved per file, never by name alone: a `now` that came
# from a FAKE clock is the correct shape and must not be flagged.
_GO_NOW_BIND = re.compile(r'\b(\w+)\s*(?::=|=)\s*time\.Now\(\)')
# ...and that name formatted later, which is the two-statement spelling.
_GO_ALIAS_FORMAT = r'\b%s\.Format\(\s*"([^"\\]*)"\s*\)'


def relkey(path, root=None):
    """A path as the ledger spells it: relative to the root, forward slashes.

    `str(path.relative_to(ROOT))` is BACKSLASH-separated on Windows, and a
    ledger keyed on that separator reads every real site as unrecorded and
    every real entry as stale on that platform alone -- both directions of the
    both-directions check firing at once, which is the one defect that escaped
    scripts/check_test_flakiness.py's own test suite and took two CI legs red.
    `python/tests/test_check_clock_test_flakiness.py` drives
    `pathlib.PureWindowsPath` through here explicitly so reintroducing `str()`
    fails on darwin too.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    r = ROOT if root is None else root
    r = r if isinstance(r, pathlib.PurePath) else pathlib.PurePath(r)
    if p.is_absolute() and p.is_relative_to(r):
        return p.relative_to(r).as_posix()
    # Outside the root: spelled whole rather than raised on. A path this
    # checker is pointed at from elsewhere -- a ledger monkeypatched into a
    # tmp_path by its own test suite -- must not take the checker down with a
    # ValueError, because a checker that crashes reads as broken tooling rather
    # than as the finding it is.
    return p.as_posix()


def missing_scan_roots(root=None):
    """Scan roots that are not there -- i.e. that this sweep did not look at.

    The vacuity guard below is all-or-nothing and this sweep is not: `rglob` on
    a missing path yields nothing and raises nothing, so a renamed root
    subtracts its whole share in SILENCE while the remaining roots keep the
    file count comfortably non-zero. `e2e` holds one of the two measured
    defects; rename it and the checker would walk the Go and pytest roots and
    print success.
    """
    base = ROOT if root is None else pathlib.Path(root)
    return [r for r in SCAN_ROOTS if not (base / r).is_dir()]


def _walk(base, roots, pattern):
    out = []
    for scan_root in roots:
        d = base / scan_root
        if not d.is_dir():
            continue
        for path in sorted(d.rglob(pattern)):
            rel = path.relative_to(base)
            if SKIP_DIRS.intersection(rel.parts):
                continue
            out.append(path)
    return out


def scanned_files(root=None):
    """Every file this checker inspects: Go test files, then Python sources."""
    base = ROOT if root is None else pathlib.Path(root)
    return (_walk(base, GO_SCAN_ROOTS, "*_test.go")
            + _walk(base, PY_SCAN_ROOTS, "*.py"))


def _strip_go_comments(text):
    """Go comments blanked out, newline-for-newline, strings left INTACT.

    Strings must survive: the layout this checker reads IS a string literal.
    Hand-walked over Go's four lexical states rather than regexed, because
    `"https://learn.microsoft.com/x"` contains `//` and the regex form reads
    the second half of every documentation URL in the tree as a comment --
    scripts/check_comment_drift.py records paying for that.
    """
    out = []
    i, n = 0, len(text)
    state = None  # None | '"' | '`' | "'" | '//' | '/*'
    while i < n:
        c = text[i]
        two = text[i:i + 2]
        if state is None:
            if two == "//":
                state, i = "//", i + 2
                continue
            if two == "/*":
                state, i = "/*", i + 2
                continue
            if c in '"`\'':
                state = c
            out.append(c)
            i += 1
            continue
        if state in ("//", "/*"):
            if state == "//" and c == "\n":
                state = None
                out.append(c)
            elif state == "/*" and two == "*/":
                state, i = None, i + 2
                continue
            elif c == "\n":
                out.append(c)
            i += 1
            continue
        # inside a string: `\` escapes the next byte, except in a raw string
        if state != "`" and c == "\\" and i + 1 < n:
            out.append(text[i:i + 2])
            i += 2
            continue
        if c == state:
            state = None
        out.append(c)
        i += 1
    return "".join(out)


def go_layout_is_day_granular(layout):
    """True when a Go reference layout names a date and carries no time.

    Both halves are required. A layout with a time component is not truncated,
    so there is no boundary to straddle; a layout with no date at all
    (`"15:04:05"`) is not a day read either, and flagging it would be the kind
    of false positive that gets a checker argued with rather than fixed.
    """
    if any(tok in layout for tok in _GO_TIME_TOKENS):
        return False
    return any(tok in layout for tok in _GO_DATE_TOKENS)


def py_format_is_day_granular(fmt):
    """The same rule for a `strftime` format string."""
    if any(d in fmt for d in _PY_TIME_DIRECTIVES):
        return False
    return any(d in fmt for d in _PY_DATE_DIRECTIVES)


def _go_symbol_at(lines, i):
    """The enclosing declaration's name, or `<module>` for a package-level read.

    Walks UP to the nearest `func` at column zero, which is where every
    declaration in this tree sits. `<module>` covers a day read in a package
    `var` initialiser -- a position Go allows and this tree has none of.
    """
    for j in range(i, -1, -1):
        match = _GO_FUNC.match(lines[j])
        if match:
            return match.group(1)
    return "<module>"


def go_now_names(text):
    """Names this file binds to a `time.Now()` chain.

    Per file rather than a fixed name list, for the reason
    check_python_test_flakiness.py's `bare_sleep_names` gives about a local
    helper called `sleep`: a `now` that came from a FAKE clock
    (`st.Clock.Now()`, which is most of this tree) is the CORRECT shape, and
    flagging it would make the checker something people argue with.
    """
    return {m.group(1) for m in _GO_NOW_BIND.finditer(text)}


def _go_findings(rel, text):
    raw_lines = text.split("\n")
    stripped = _strip_go_comments(text)
    lines = stripped.split("\n")
    patterns = [_GO_NOW_FORMAT]
    for name in sorted(go_now_names(stripped)):
        patterns.append(re.compile(_GO_ALIAS_FORMAT % re.escape(name)))
    out, seen = [], set()
    for i, line in enumerate(lines):
        for pattern in patterns:
            for match in pattern.finditer(line):
                if not go_layout_is_day_granular(match.group(1)):
                    continue
                # One line can satisfy both the direct and the alias pattern
                # (`now := time.Now(); _ = now.Format(...)` on one line); the
                # ledger key would collapse them anyway, but a duplicate
                # finding would inflate the reported count.
                if (i, match.start()) in seen:
                    continue
                seen.add((i, match.start()))
                out.append({"file": rel, "line": i + 1,
                            "symbol": _go_symbol_at(lines, i), "kind": KIND,
                            "snippet": raw_lines[i].strip()})
    return sorted(out, key=lambda f: f["line"])


def _is_now_read(node):
    """True if `node` is a wall-clock read: `datetime.now(...)`/`.utcnow()`.

    Matched on the ATTRIBUTE rather than the full dotted path, so
    `dt.datetime.now(UTC)` and a `from datetime import datetime` both land --
    the two spellings this tree actually uses. A bare `now()` is NOT matched:
    unlike `time.sleep`, `now` is a name a great many objects legitimately
    have, and a fixture's own `now()` on a FAKE clock is the correct shape
    rather than a finding.

    IT LOOKS THROUGH ARITHMETIC, which the first version did not, and the
    omission reads as safe until the offset is in DAYS. `(datetime.now(UTC) -
    timedelta(days=1)).strftime("%Y-%m-%d")` is "yesterday": a day-granular
    wall-clock read with exactly the same boundary as "today", and the first
    version reported it green. Found by mutation-testing this module -- the
    near-miss asserting that a full TIMESTAMP layout is not flagged was written
    with an offset receiver, so it passed for the wrong reason and left the
    timestamp discrimination itself unasserted. Disabling that discrimination
    now reds the suite; before, it did not.
    """
    if isinstance(node, ast.BinOp) and isinstance(node.op, (ast.Add, ast.Sub)):
        return _is_now_read(node.left) or _is_now_read(node.right)
    return (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
            and node.func.attr in ("now", "utcnow"))


def _is_date_today(node):
    """True if `node` is `date.today()` / `datetime.date.today()`.

    The receiver must be spelled `date` or `datetime`, so an unrelated
    `schedule.today()` is not a finding. `date.today()` has no format argument
    to inspect: it IS day-granular by construction, which is what makes it
    worth a second shape rather than a special case of the first.
    """
    if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
            and node.func.attr == "today"):
        return False
    recv = node.func.value
    name = (recv.attr if isinstance(recv, ast.Attribute)
            else recv.id if isinstance(recv, ast.Name) else None)
    return name in ("date", "datetime")


def py_now_names(tree):
    """Names this module assigns from a wall-clock read.

    The Python half of the alias clause above, and narrowed the same way: only
    a name bound from `datetime.now(...)` / `.utcnow()` counts, so a value that
    reached a variable from a fixture or a response body is not a clock read
    however it is later formatted.
    """
    out = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Assign) and _is_now_read(node.value):
            for target in node.targets:
                if isinstance(target, ast.Name):
                    out.add(target.id)
        elif (isinstance(node, ast.AnnAssign) and node.value is not None
              and _is_now_read(node.value) and isinstance(node.target, ast.Name)):
            out.add(node.target.id)
    return out


def _py_symbol_index(tree):
    """[(start, end, name)] for every OUTERMOST function in the module.

    Outermost, not innermost, for the reason check_python_test_flakiness.py
    gives: a read inside a nested helper is named by the test that contains it,
    which is both more useful to a reader and more stable than the helper's
    name.
    """
    out = []
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            out.append((node.lineno, node.end_lineno, node.name))
        elif isinstance(node, ast.ClassDef):
            for sub in node.body:
                if isinstance(sub, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    out.append((sub.lineno, sub.end_lineno, sub.name))
    return out


def _py_findings(rel, text):
    try:
        tree = ast.parse(text)
    except SyntaxError:
        # A file this interpreter cannot parse is not silently skipped: it is
        # a file that was NOT inspected, and saying so is the whole point of
        # missing_scan_roots one function up.
        return [{"file": rel, "line": 1, "symbol": "<module>", "kind": KIND,
                 "snippet": "<unparseable: this file was not inspected>"}]
    raw_lines = text.split("\n")
    symbols = _py_symbol_index(tree)
    now_names = py_now_names(tree)

    def symbol_at(lineno):
        for start, end, name in symbols:
            if start <= lineno <= end:
                return name
        return "<module>"

    out = []
    for node in ast.walk(tree):
        hit = False
        if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                and node.func.attr == "strftime" and node.args
                and isinstance(node.args[0], ast.Constant)
                and isinstance(node.args[0].value, str)
                and py_format_is_day_granular(node.args[0].value)):
            # The receiver has to be a clock read. `published.strftime("%Y-%m-%d")`
            # on a value that came from a fixture is not a wall-clock read and
            # has no boundary to straddle.
            recv = node.func.value
            hit = (_is_now_read(recv)
                   or (isinstance(recv, ast.Name) and recv.id in now_names))
        elif _is_date_today(node):
            hit = True
        if not hit:
            continue
        line = node.lineno
        out.append({"file": rel, "line": line, "symbol": symbol_at(line),
                    "kind": KIND,
                    "snippet": raw_lines[line - 1].strip() if line - 1 < len(raw_lines) else ""})
    return sorted(out, key=lambda f: f["line"])


def findings_for(path, source=None):
    """Findings in one file, as {file, line, symbol, kind, snippet} dicts.

    Dispatches on the suffix rather than on which walk produced the path, so a
    test can hand this a `PureWindowsPath` and a string of source with no
    filesystem involved at all.
    """
    text = source if source is not None else pathlib.Path(path).read_text(encoding="utf-8")
    rel = relkey(path)
    name = pathlib.PurePath(path).name
    if name.endswith("_test.go"):
        return _go_findings(rel, text)
    if name.endswith(".py"):
        return _py_findings(rel, text)
    return []


def scan(files=None):
    """Every finding across the tree."""
    out = []
    for path in (files if files is not None else scanned_files()):
        out.extend(findings_for(path))
    return out


def ledger_key(item):
    """The ledger key for a finding or an accepted entry: file, symbol AND kind.

    ONE function, read by findings and entries ALIKE, which is the fix for the
    defect check_python_test_flakiness.py records: the key was spelled inline in
    three places with the kind missing from two, so a finding matched an entry
    on `file:symbol` alone and 42 of 44 recorded symbols were exempt from the
    bans the ledger existed to enforce. This checker defines only one kind
    today, so a kind-less key would be harmless RIGHT NOW -- and that is
    precisely when it gets written, so it is spelled once and required by name.
    """
    missing = [k for k in ("file", "symbol", "kind") if not item.get(k)]
    if missing:
        raise KeyError(
            f"a clock-flakiness entry is missing {', '.join(missing)}: {item!r}. "
            "Every entry needs file, symbol and kind -- the kind is what scopes "
            "the exemption to one shape, so an entry without one would exempt "
            "the symbol from every ban this checker ever grows.")
    return f"{item['file']}:{item['symbol']}:{item['kind']}"


def load_ledger(ledger=None):
    """The accepted sites, keyed by `ledger_key`.

    Keyed on the enclosing symbol rather than the line number: a line number
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

    gone = missing_scan_roots()
    if gone:
        print("check_clock_test_flakiness: scan root(s) do not exist, so their "
              f"share of the surface was not inspected at all: {', '.join(gone)}\n"
              "  Either the directory moved -- update SCAN_ROOTS -- or it was "
              "removed. A partial sweep must not report success.")
        return 1

    files = scanned_files()
    if not files:
        print("check_clock_test_flakiness: walked zero files under "
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
        # The stale direction is printed too. Report mode is the output someone
        # actually reads, and showing only what is new would hide the half of
        # the contract a "what changed" reader would never think to ask about.
        for key in stale:
            print(f"{'STALE':9} {key} - recorded, but no longer flagged")
        print(f"\ncheck_clock_test_flakiness: {len(files)} file(s), "
              f"{len(found)} day-granular clock read(s), "
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
            f"{len(unrecorded)} day-granular clock read(s) are not recorded in "
            f"{relkey(LEDGER)}:\n    {listing}\n"
            "  A day read AFTER the operations whose data it must contain makes "
            "the verdict a function of whether the run crossed UTC midnight -- "
            "invisible to -race and -shuffle=on, and it fires once a day. "
            "Capture the day ONCE before those operations and widen with "
            "internal/testsupport.UTCDaysSince (Go) or the driver's own "
            "activity_days (Python), or record the site with the reason its "
            "verdict cannot turn on which day it names.")
    if stale:
        problems.append(
            f"{len(stale)} ledger entr(y/ies) name a site that is no longer "
            "flagged; a stale allowance is how a ban quietly stops applying:\n"
            "    " + "\n    ".join(stale))

    if problems:
        print("check_clock_test_flakiness: " + "\n\n".join(problems))
        return 1

    print(f"check_clock_test_flakiness: {len(files)} file(s) scanned; "
          f"{len(found)} day-granular clock read(s), all {len(accepted)} "
          f"recorded in {relkey(LEDGER)} with a reason")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
