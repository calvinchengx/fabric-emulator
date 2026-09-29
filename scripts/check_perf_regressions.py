#!/usr/bin/env python3
"""Per-request work that is unbounded BY CONSTRUCTION, and the ordering of the
ceilings that bound it.

WHY IT EXISTS. There are no benchmarks in this repository -- `func Benchmark`
returns 0 hits across 552 `*_test.go` files -- and no stored timing baseline
anywhere in the tree. So a performance regression cannot be spotted here by
measurement, today, by anything; and standing up a benchmark corpus plus a
comparison harness is a different and much larger piece of work. What CAN be
decided statically, offline, and without a baseline is the shape that does not
need a stopwatch to be a regression: work whose cost is chosen by the caller and
bounded by nothing.

WHAT WAS MEASURED, at the commit that added this:

  * 70 sites consume an inbound request body across 207 non-test Go files under
    internal/ -- 68 `json.NewDecoder(r.Body)` and 2 bare `io.ReadAll(r.Body)`.
  * `http.MaxBytesReader` appeared 0 times in the entire tree. There was no
    outer ceiling on any request, of any kind, anywhere.
  * 23 `httpx.ReadBounded` call sites across 16 files DID carry a ceiling, one
    of 8 named in internal/httpx/body.go. So the tree was half-bounded: every
    site reading a body as BYTES went through a ceiling, and every site
    streaming it through a DECODER went through none.
  * 0 benchmarks, and 9 `regexp.MustCompile` sites, all 9 at package scope.

The 68 decoder sites are the exposure: a streaming decoder allocates as it
reads, so before the outer bound landed any client could make the emulator
allocate whatever it chose to send, on any of them, and a 69th added tomorrow
would inherit that invisibly. That is closed by ONE bound at the root handler
(internal/server.Server.boundBodies) rather than 68 edits, which is why this
checker's ledger ships nearly empty instead of 70 entries deep -- a ledger with
70 accepted entries would document the problem rather than close it.

WHAT IS FLAGGED. Four kinds, and two of them are about the repair rather than
the original shape, because a bound that exists is not the same as a bound that
still applies:

  * `unbounded-body-read` -- a REQUEST body read as bytes through no ceiling:
    `io.ReadAll(r.Body)`, `io.Copy(_, r.Body)`. Response bodies (`resp.Body`) are
    not this kind; they are bounded on their own terms by the sites that read
    them, and an engine we relayed to is not an untrusted caller choosing a size.
    Two sites held this shape and BOTH were repaired onto httpx.ReadBounded in
    the same change (internal/onelake/principalaccess.go,
    internal/api/dataaccessmode.go), so this ships at zero. Note that
    internal/httpx/guard_test.go does NOT cover it: that test bans the
    `io.ReadAll(io.LimitReader(...))` idiom -- a bound that silently truncates --
    and is blind to the absence of a bound altogether.
  * `missing-outer-bound` -- internal/server/server.go must install
    `http.MaxBytesReader`. This is the 68 decoder sites' only ceiling; if it is
    refactored away, every one of them silently reverts to unbounded and no test
    in the tree would necessarily notice.
  * `ceiling-above-outer-bound` -- an inner ceiling in internal/httpx/body.go
    that is not STRICTLY BELOW `DefaultMaxRequestBody`. This one is subtle and is
    the reason the kind exists: `ReadBounded` detects overflow by probing max+1,
    so an outer limiter set AT an inner ceiling makes the probe itself trip. An
    oversized Blob write would then fail with net/http's generic "request body
    too large" instead of httpx's specific message, and the fit-vs-truncated
    distinction that package exists to provide would be gone at exactly the
    ceiling where it has already mattered once (MaxBlobWrite, which `fab cp`
    crossed -- see docs/34-fab-driven-example.md). A 256 MiB outer bound would
    have shipped that defect while looking correct.
  * `recompiled-regexp` -- `regexp.MustCompile` / `regexp.Compile` evaluated
    inside a function body rather than at package scope. All 9 sites are
    package-level today, so this ships empty; the kind is defined so a future
    per-call compile lands as a decision recorded here rather than an unnoticed
    cost, the same precedent `long-sleep` set in check_vitest_test_flakiness.py.

WHAT IS DELIBERATELY NOT FLAGGED, with the false-positive rate that ruled it
out. The loop-shaped heuristics one reaches for first were probed over this tree
and are too noisy to gate on:

  * `marshal-in-loop` -- 45 hits, nearly all legitimate per-item work (a list
    response marshalling each element is not a defect).
  * `sort-in-loop` -- 4 hits. `string-concat-in-loop` -- 6.
  * `defer-in-loop` -- 1 hit, internal/tds/server.go:100, and it is a FALSE
    POSITIVE: the defer is inside a goroutine closure, not the loop body, so it
    runs per connection exactly as intended. A brace-depth walk cannot tell those
    apart, which is the whole problem with the class.

Gating on any of them would mean a ledger of dozens of accepted entries that
nobody reads, which is how a guard becomes decoration. They are recorded here so
the next person does not re-litigate them from scratch.

WHY A LEDGER RATHER THAN A FLAT BAN. Same contract as every sibling guard here:
`docs/perf-regressions.json` records each accepted site with its kind and the
reason, `--strict` fails only on findings NOT in the ledger, and the ledger is
checked in BOTH directions -- an entry naming a site that is no longer flagged is
as loud as an unrecorded finding, because a stale allowance is how a ban quietly
stops applying and it is the direction a "what's new" reader never thinks to
check.

Usage:
    check_perf_regressions.py            report findings, exit 0
    check_perf_regressions.py --strict   exit non-zero on unrecorded findings
"""
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
LEDGER = ROOT / "docs" / "perf-regressions.json"

# Every directory holding Go we ship. e2e/ is harness code and examples/ is
# client code a reader runs; neither serves a request, so an unbounded read
# there is not this checker's business.
SCAN_ROOTS = ("internal", "cmd", "pkg")

# Same list, and the same reason, as the three flakiness checkers carry:
# `.claude` can hold a git WORKTREE -- a second checkout of this repository
# nested inside it -- and a sweep that walked one would report findings against
# a sibling branch's copy of a file.
SKIP_DIRS = {".git", ".claude", "vendor", "node_modules", "_site", "dist"}

# The file holding the inner ceilings and the outer default, and the handler that
# must install the outer bound. Named as constants because two kinds below are
# assertions ABOUT these files rather than sweeps across the tree.
CEILINGS_FILE = "internal/httpx/body.go"
ROOT_HANDLER_FILE = "internal/server/server.go"
OUTER_CONST = "DefaultMaxRequestBody"

# A request body read as bytes with no ceiling. Anchored on the RECEIVER name --
# `r`, `req`, `request` -- because that is what distinguishes a body a caller
# chose the size of from `resp.Body`, which is an engine we relayed to answering
# us. httpx.ReadBounded(r.Body, ...) does not match: it is not io.ReadAll.
_REQ = r"(?:r|req|request)"
_UNBOUNDED_READ = re.compile(rf"\bio\.ReadAll\(\s*{_REQ}\.Body\s*\)")
_UNBOUNDED_COPY = re.compile(rf"\bio\.Copy\([^,]+,\s*{_REQ}\.Body\s*\)")

_REGEXP = re.compile(r"\bregexp\.(?:MustCompile|Compile)\s*\(")
_FUNC = re.compile(r"^func\b")
# `MaxFoo = 100 << 20` inside a const block, or a standalone
# `const DefaultMaxRequestBody = 320 << 20`. The optional `const ` is not
# cosmetic: without it this matched every ceiling inside body.go's const block
# and silently missed the standalone outer one, which made the ordering check
# report the outer bound as ABSENT rather than comparing it. Found by running
# the checker against the tree it ships with.
_CEILING = re.compile(
    r"^\s*(?:const\s+)?(Max[A-Za-z0-9]*|DefaultMaxRequestBody)\s*=\s*(.+?)\s*$")
_SHIFT = re.compile(r"^(\d+)\s*<<\s*(\d+)$")


def relkey(path, root=None):
    """A path as the ledger spells it: relative to the root, forward slashes.

    Same reasoning as every sibling checker's `relkey` -- a raw
    `str(path.relative_to(ROOT))` is backslash-separated on Windows, and a
    ledger keyed on that separator reads every real site as unrecorded and every
    real entry as stale on that platform alone.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    r = ROOT if root is None else root
    r = r if isinstance(r, pathlib.PurePath) else pathlib.PurePath(r)
    return (p.relative_to(r) if p.is_absolute() else p).as_posix()


def missing_scan_roots(root=None):
    """Scan roots that are not there -- i.e. that this sweep did not look at.

    A renamed or removed root would leave `go_files` walking less than it
    believes and `main` reporting success over a surface it never read. That
    looks identical from the outside to a tree with no findings, and has a very
    different cause.
    """
    base = ROOT if root is None else pathlib.Path(root)
    return [r for r in SCAN_ROOTS if not (base / r).is_dir()]


def go_files(root=None):
    """Every non-test .go file under the scan roots."""
    base = ROOT if root is None else pathlib.Path(root)
    out = []
    for scan_root in SCAN_ROOTS:
        d = base / scan_root
        if not d.is_dir():
            continue
        for path in sorted(d.rglob("*.go")):
            rel = path.relative_to(base)
            if SKIP_DIRS.intersection(rel.parts):
                continue
            # Test files may demonstrate an unbounded read while proving it is
            # bad -- the same carve-out httpx/guard_test.go makes for itself.
            if path.name.endswith("_test.go"):
                continue
            out.append(path)
    return out


def _strip_noise(text):
    """Comments and string literals blanked, newline-for-newline so every line
    number is unaffected.

    Crude on purpose, in the tradition of the Go flakiness checker's
    `_brace_depth`: Go's grammar is not being parsed here, only enough of its
    syntax to tell a call from a sentence about a call. Without this, the
    docstring you are reading -- which names `io.ReadAll(r.Body)` -- would be
    reported as a finding against this very file if it lived in Go.
    """
    text = re.sub(r"/\*.*?\*/", lambda m: "\n" * m.group(0).count("\n"), text,
                  flags=re.DOTALL)
    out = []
    for line in text.split("\n"):
        line = re.sub(r"//.*$", "", line)
        line = re.sub(r'"(?:[^"\\]|\\.)*"', '""', line)
        line = re.sub(r"`[^`]*`", "``", line)
        out.append(line)
    return "\n".join(out)


def _enclosing_func(clean_lines, i):
    """The name of the function containing line i, or `<package>` at file scope.

    Walks BACKWARDS to the nearest `^func` and then forwards from it counting
    braces, so a site after a function has closed is correctly attributed to
    file scope rather than to the function above it. That distinction is the
    whole of the `recompiled-regexp` kind: a package-level
    `var x = regexp.MustCompile(...)` compiles once at init and is not a finding,
    while the same call one line inside a handler compiles on every request.
    """
    for start in range(i, -1, -1):
        if not _FUNC.match(clean_lines[start]):
            continue
        depth = 0
        for j in range(start, len(clean_lines)):
            depth += clean_lines[j].count("{") - clean_lines[j].count("}")
            if j >= start and depth <= 0 and "{" in "".join(clean_lines[start:j + 1]):
                # Function closed at j. Inside only if i is within (start, j].
                return _func_name(clean_lines[start]) if start < i <= j else "<package>"
        return _func_name(clean_lines[start]) if start < i else "<package>"
    return "<package>"


def _func_name(line):
    m = re.match(r"^func\s+(?:\([^)]*\)\s*)?([A-Za-z0-9_]+)", line)
    return m.group(1) if m else "<anonymous>"


def _value_of(expr):
    """`256 << 20` or a plain integer, as an int. None if it is neither.

    Narrow deliberately: a ceiling built from another named constant is not
    resolved, and none is written that way today. An unreadable value must not
    silently pass the ordering check below -- `ceilings_of` reports what it could
    not read, and `main` fails on it, because a ceiling this cannot compare is
    a ceiling the ordering invariant is not being enforced for.
    """
    expr = expr.strip().rstrip(",")
    m = _SHIFT.match(expr)
    if m:
        return int(m.group(1)) << int(m.group(2))
    if re.fullmatch(r"\d+", expr):
        return int(expr)
    return None


def ceilings_of(text):
    """({name: value}, [unreadable names]) from internal/httpx/body.go."""
    values, unreadable = {}, []
    for line in _strip_noise(text).split("\n"):
        m = _CEILING.match(line)
        if not m:
            continue
        name, raw = m.group(1), m.group(2)
        v = _value_of(raw)
        if v is None:
            unreadable.append(name)
        else:
            values[name] = v
    return values, unreadable


def findings_for(path, source=None, root=None):
    """Sweep findings in one file, as {file, symbol, line, kind, snippet} dicts.

    The two assertion kinds (`missing-outer-bound`, `ceiling-above-outer-bound`)
    are NOT produced here -- they are properties of a file's absence or of a
    comparison between two files, which a per-line sweep cannot express. See
    `structural_findings`.
    """
    text = source if source is not None else path.read_text(encoding="utf-8")
    raw_lines = text.split("\n")
    clean_lines = _strip_noise(text).split("\n")
    rel = relkey(path, root)
    out = []
    for i, line in enumerate(clean_lines):
        kind = None
        if _UNBOUNDED_READ.search(line) or _UNBOUNDED_COPY.search(line):
            kind = "unbounded-body-read"
        elif _REGEXP.search(line) and _enclosing_func(clean_lines, i) != "<package>":
            kind = "recompiled-regexp"
        if kind is None:
            continue
        out.append({"file": rel, "symbol": _enclosing_func(clean_lines, i),
                    "line": i + 1, "kind": kind,
                    "snippet": raw_lines[i].strip()})
    return out


def structural_findings(root=None):
    """The two findings that are about the REPAIR rather than the original shape.

    Both are absence-shaped, and absence is what a sweep cannot report: no line
    anywhere says "the outer bound is missing". Without these the checker would
    pass, in perpetuity, on a tree whose one ceiling had been refactored away --
    reporting zero unbounded reads, which would be true and beside the point.
    """
    base = ROOT if root is None else pathlib.Path(root)
    out = []

    handler = base / ROOT_HANDLER_FILE
    if not handler.exists():
        out.append({"file": ROOT_HANDLER_FILE, "symbol": "Handler", "line": 0,
                    "kind": "missing-outer-bound",
                    "snippet": f"{ROOT_HANDLER_FILE} does not exist"})
    elif "http.MaxBytesReader" not in _strip_noise(
            handler.read_text(encoding="utf-8")):
        out.append({"file": ROOT_HANDLER_FILE, "symbol": "Handler", "line": 0,
                    "kind": "missing-outer-bound",
                    "snippet": "no http.MaxBytesReader in the root handler"})

    ceilings = base / CEILINGS_FILE
    if not ceilings.exists():
        out.append({"file": CEILINGS_FILE, "symbol": OUTER_CONST, "line": 0,
                    "kind": "ceiling-above-outer-bound",
                    "snippet": f"{CEILINGS_FILE} does not exist"})
        return out

    values, unreadable = ceilings_of(ceilings.read_text(encoding="utf-8"))
    outer = values.get(OUTER_CONST)
    for name in sorted(unreadable):
        out.append({"file": CEILINGS_FILE, "symbol": name, "line": 0,
                    "kind": "ceiling-above-outer-bound",
                    "snippet": f"{name} has a value this checker cannot read, "
                               "so the ordering is unverified"})
    if outer is None:
        out.append({"file": CEILINGS_FILE, "symbol": OUTER_CONST, "line": 0,
                    "kind": "ceiling-above-outer-bound",
                    "snippet": f"{OUTER_CONST} is missing: the inner ceilings "
                               "have nothing to be ordered against"})
        return out
    for name, v in sorted(values.items()):
        if name == OUTER_CONST:
            continue
        if v >= outer:
            out.append({"file": CEILINGS_FILE, "symbol": name, "line": 0,
                        "kind": "ceiling-above-outer-bound",
                        "snippet": f"{name}={v} is not strictly below "
                                   f"{OUTER_CONST}={outer}; ReadBounded probes "
                                   "max+1, so its specific message would be "
                                   "replaced by net/http's generic refusal"})
    return out


def scan(files=None, root=None):
    """Every finding across the tree -- sweep kinds plus structural ones."""
    out = []
    for path in (files if files is not None else go_files(root)):
        out.extend(findings_for(path, root=root))
    out.extend(structural_findings(root))
    return out


def ledger_key(item):
    """The ledger key for a finding or an accepted entry: file, symbol AND kind.

    Three parts, and the kind is not optional. The same defect
    check_python_test_flakiness.py had and fixed: a key with no kind would let
    an accepted `unbounded-body-read` for a symbol also exempt a
    `recompiled-regexp` added to that symbol later, silently widening what was
    actually reviewed to cover a shape nobody looked at.
    """
    missing = [k for k in ("file", "symbol", "kind") if not item.get(k)]
    if missing:
        raise KeyError(
            f"a perf entry is missing {', '.join(missing)}: {item!r}. Every "
            "entry needs file, symbol and kind -- the kind is what scopes the "
            "exemption to one shape, so an entry without one would exempt the "
            "symbol from every ban.")
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

    # Checked BEFORE the walked-nothing guard below, and the order matters: a
    # root that does not exist and a root that exists holding no Go both leave
    # `files` short, and the two causes deserve different messages. Checked
    # second, this could never be the one that fires.
    gone = missing_scan_roots()
    if gone:
        print("check_perf_regressions: scan root(s) do not exist, so their share "
              f"of the surface was not inspected at all: {', '.join(gone)}\n"
              "  Either the directory moved -- update SCAN_ROOTS -- or it was "
              "removed. A partial sweep must not report success.")
        return 1

    files = go_files()

    # A sweep that walked nothing reports success. Same guard every sibling
    # carries, for the same reason: a check that inspects nothing passes for the
    # wrong reason, and it passes quietly.
    if not files:
        print("check_perf_regressions: walked zero non-test .go files under "
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
        print(f"\ncheck_perf_regressions: {len(files)} non-test Go files, "
              f"{len(found)} finding(s), "
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
            f"{len(unrecorded)} unbounded-by-construction site(s) are neither "
            f"repaired nor recorded in {relkey(LEDGER)}:\n    {listing}\n"
            "  Per-request work whose size the caller chooses needs a ceiling: "
            "read bytes through httpx.ReadBounded, keep the root handler's "
            "http.MaxBytesReader in place, hoist a regexp to package scope, or "
            "record the site with its reason.")
    if stale:
        problems.append(
            f"{len(stale)} ledger entr(y/ies) name a site that is no longer "
            "flagged; a stale allowance is how a ban quietly stops applying:\n"
            "    " + "\n    ".join(stale))

    if problems:
        print("check_perf_regressions: " + "\n\n".join(problems))
        return 1

    print(f"check_perf_regressions: {len(files)} non-test Go files scanned; "
          f"{len(found)} finding(s), all {len(accepted)} recorded in "
          f"{relkey(LEDGER)} with a reason")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
