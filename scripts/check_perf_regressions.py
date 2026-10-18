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

WHAT WAS MEASURED when the TDS half was added, which is the surface everything
above could not see:

  * TWO listeners, not one. cmd/fabric-emulator/main.go serves HTTP on one
    (`http.Serve(ln, srv.Handler())` -- the ONLY serve site in the tree) and the
    SQL/TDS endpoint on a second net.Listener of its own, `srv.TDS.Serve(tln)`.
  * 0 checks on the accumulated length of a TDS message:
    `grep -c 'MaxMessageBytes|maxMessageBytes' internal/tds/*.go` returned 0
    across all 40 files of that package.
  * 8 TDS read sites (3 in server.go, 2 in client.go, 2 in splice.go, plus the
    loop in packet.go itself), ALL of them funnelling through `ReadMessage`. One
    ceiling there therefore covers the whole listener, which is why this is one
    anchored assertion rather than eight.

Everything above anchors on `r.Body`, `http.MaxBytesReader` and
internal/httpx/body.go, so it is HTTP-only BY CONSTRUCTION -- there is no
`r.Body` on a raw TCP listener for an outer bound to wrap. And the TDS side held
the same shape the HTTP side spent a whole change closing: `tds.ReadMessage`
concatenated packets until one set the EOM status bit, with no cap on the total.
Each `ReadPacket` is self-limiting at 64 KiB because the length field is a
uint16, which is exactly why the loop read as bounded and was not: a peer that
never sets EOM chose how much the emulator allocated. Repaired in the same
change by `tds.DefaultMaxMessageBytes` (settable at start-up through
`tds.SetMaxMessageBytes`, wired to FABRIC_MAX_TDS_MESSAGE_BYTES), so this kind
too ships at zero entries rather than as a recorded allowance.

The 68 decoder sites are the exposure: a streaming decoder allocates as it
reads, so before the outer bound landed any client could make the emulator
allocate whatever it chose to send, on any of them, and a 69th added tomorrow
would inherit that invisibly. That is closed by ONE bound at the root handler
(internal/server.Server.boundBodies) rather than 68 edits, which is why this
checker's ledger ships nearly empty instead of 70 entries deep -- a ledger with
70 accepted entries would document the problem rather than close it.

WHAT IS FLAGGED. Six kinds, and four of them are about the repair rather than
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
  * `unbounded-frame-accumulation` -- the whole CHAIN the TDS bound travels
    through internal/tds/packet.go, in four links: `DefaultMaxMessageBytes` is
    declared with a value this can read; the effective ceiling is initialised
    FROM it; the exported `ReadMessage` -- the single entry point all 8 TDS read
    sites go through -- routes it; and the function holding the packet join
    ORDERS something against the bound it was handed, at or before the append
    that would cross it.
    Four links because each is separately satisfiable while the bound does
    nothing, and THIS KIND'S OWN FIRST VERSION proved it. That version checked
    only that `ReadMessage` mentioned the constant. By the time it shipped,
    `ReadMessage` was a one-line delegation to a helper -- so it did mention the
    constant, and deleting the comparison from the HELPER left the ceiling
    declared, derived, passed around, never compared against, and this checker
    reporting the tree healthy. The accumulating function is therefore located
    from the join itself rather than named here: a name stays satisfied by a
    function that merely still exists, while the join is satisfied only by the
    line that grows the buffer. Zero recognised joins is itself a finding.
    What this does NOT claim, so nobody reads more into a green run than is
    there: that the comparison is arithmetically correct -- that the accumulated
    length is what is on the other side of it, and that the bound is inclusive.
    Those are held by internal/tds/maxmessage_test.go, which exercises the
    refusal and its tightness. This holds the half no test can: that the
    comparison is still there, after a refactor nothing would fail on.
  * `unbounded-serve-root` -- every `http.Serve` / `http.ListenAndServe` /
    `http.ListenAndServeTLS` / `&http.Server{...}` site under the scan roots
    must be handed an expression that calls `.Handler()`, which is the single
    place the outer HTTP bound is installed. Exactly one such site exists today
    (cmd/fabric-emulator/main.go, handed `srv.Handler()`), so the kind ships
    empty; its purpose is that a SECOND HTTP listener handed `s.mux`, a bare
    `http.HandlerFunc` or no Handler field at all (which means
    http.DefaultServeMux) lands as a finding rather than as an unnoticed bypass
    of the 68 decoder sites' only ceiling. The check is that the root handler is
    in the chain, not that it is the outermost wrapper -- `middleware(
    srv.Handler())` is fine, because the bound is still installed. Zero
    recognised serve sites is itself a finding: a sweep that covers nothing
    passes vacuously, and a serve call moved behind a helper this regex does not
    know would do exactly that.

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
  * `append-in-loop` -- the generic form of the TDS defect, and the most
    tempting one to add after fixing it. 643 `append(` sites under the scan
    roots, 540 of them inside a loop by the same crude walk. Nearly all
    accumulate a bounded, already-read collection, and a heuristic that cannot
    tell "grow a slice per item of a list I already have" from "grow a slice per
    frame a peer chooses to send" would bury the one case that matters under
    five hundred that do not. So the TDS bound is held by the two anchored kinds
    above -- the two listeners this emulator actually exposes -- rather than by
    loop shape.

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

# The SECOND listener's ceiling. A raw TCP listener has no r.Body, so nothing
# above can reach this surface: the assertion has to name the file directly.
#
# THREE names, not one, and the reason is the defect this kind was first shipped
# with. It checked that `ReadMessage` -- the exported entry -- mentioned the
# ceiling, which it did, as a one-line delegation to a helper. Deleting the
# comparison from the HELPER left the constant declared, passed around, never
# compared against, and this kind reported the tree healthy. So what is asserted
# is not "a function names the constant" but the chain the bound actually travels:
# the default is DECLARED, the effective ceiling DERIVES from it, the entry point
# ROUTES it, and the function holding the join COMPARES against the bound it was
# handed, at or before the append that would cross it.
#
# The accumulating function is deliberately NOT named here. It is located from
# the join itself (`_TDS_JOIN`), because naming it is what went wrong: a name in
# this file is satisfied by a function that still exists, while the join is
# satisfied only by the line that actually grows the buffer.
TDS_PACKET_FILE = "internal/tds/packet.go"
TDS_CEILING = "DefaultMaxMessageBytes"
TDS_EFFECTIVE = "maxMessageBytes"
TDS_ENTRY_FUNC = "ReadMessage"

# Where the zero-serve-sites finding is reported, which is the file that holds
# the one real site. Named so the finding has somewhere to point rather than
# being attributed to the whole tree.
SERVE_ROOT_ANCHOR = "cmd/fabric-emulator/main.go"

# The one handler expression a serve site may be handed: `srv.Handler()` is
# where boundBodies is installed. Matched as a CALL, because a field or variable
# named `handler` proves nothing about the bound.
_HANDLER_CALL = re.compile(r"\.Handler\(\s*\)")
# `http.Serve(` / `http.ListenAndServe(` / `http.ListenAndServeTLS(` and the
# `http.Server{...}` composite literal. ListenAndServeTLS is listed before
# ListenAndServe so the longer name wins the alternation, and `Serve` requires
# the open paren so `http.ServeContent(`, `http.ServeFile(` and `http.ServeMux`
# -- none of which install a handler -- do not match.
_SERVE_SITE = re.compile(
    r"\bhttp\.(?:ListenAndServeTLS|ListenAndServe|Serve)\s*\(|\bhttp\.Server\s*\{")
_HANDLER_FIELD = re.compile(r"\bHandler\s*:\s*(.+)")

# A request body read as bytes with no ceiling. Anchored on the RECEIVER name --
# `r`, `req`, `request` -- because that is what distinguishes a body a caller
# chose the size of from `resp.Body`, which is an engine we relayed to answering
# us. httpx.ReadBounded(r.Body, ...) does not match: it is not io.ReadAll.
_REQ = r"(?:r|req|request)"
_UNBOUNDED_READ = re.compile(rf"\bio\.ReadAll\(\s*{_REQ}\.Body\s*\)")
_UNBOUNDED_COPY = re.compile(rf"\bio\.Copy\([^,]+,\s*{_REQ}\.Body\s*\)")

_REGEXP = re.compile(r"\bregexp\.(?:MustCompile|Compile)\s*\(")
_TDS_CEILING_DECL = re.compile(
    rf"^\s*(?:const\s+)?{TDS_CEILING}\s*=\s*(.+?)\s*$", re.M)
# The packet JOIN -- `data = append(data, p.Data...)`. Anchored on the PAYLOAD
# being appended rather than on `append(` alone, which would also match any
# unrelated slice-building helper this file grows later and demand a ceiling
# check inside it. If a refactor spells the join some other way this matches
# nothing, and zero recognised joins is itself a finding: the same
# covers-nothing-passes-vacuously direction `unbounded-serve-root` takes.
_TDS_JOIN = re.compile(r"\bappend\s*\([^,]+,\s*[A-Za-z0-9_]+\.Data\s*\.\.\.")
# `<` and `>` that ORDER two values. `<=`/`>=` included, `==` excluded (equality
# is not a ceiling check), and shifts blanked before matching because `<<`
# contains `<` and `320 << 20` is not a comparison.
_ORDERING = r"(?:<=|>=|<|>)"
_SHIFTS = re.compile(r"<<|>>")
_INT_TYPE = re.compile(r"u?int(?:8|16|32|64)?")
# `limit := expr` / `limit = expr`, so a bound read into a local is still a
# bound. One name on the left only: a multi-assignment is not how a ceiling is
# fetched, and accepting one would widen what counts as the bound for no gain.
_ASSIGN = re.compile(r"^\s*([A-Za-z0-9_]+)\s*(?::=|=)\s*(.+)$")
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


def _func_span(clean_lines, name):
    """`(first, last)` line indices of `func <name>(...)`, or None if it is gone.

    Brace-counted forward from the `func` line, in the same crude tradition as
    `_enclosing_func`: enough of Go's syntax to tell where a function ends, not
    a parse. The TDS kind needs the SPAN rather than just the text, because it
    has to compare the position of the ceiling comparison against the position
    of the append -- "bounded" and "bounded before the allocation that would
    cross the bound" are different claims, and packet.go makes the second one.
    """
    pat = re.compile(rf"^func\s+{re.escape(name)}\s*\(")
    for start, line in enumerate(clean_lines):
        if not pat.match(line):
            continue
        depth = 0
        for j in range(start, len(clean_lines)):
            depth += clean_lines[j].count("{") - clean_lines[j].count("}")
            if depth <= 0 and "{" in "".join(clean_lines[start:j + 1]):
                return start, j
        return start, len(clean_lines) - 1
    return None


def _func_body(clean_lines, name):
    """The lines of `func <name>(...)`, header included, or None if it is gone."""
    span = _func_span(clean_lines, name)
    return None if span is None else clean_lines[span[0]:span[1] + 1]


def _bound_params(body_text):
    """The integer-parameter names of a Go func -- the bounds it was HANDED.

    `func readMessageBounded(r io.Reader, limit int64)` -> `["limit"]`. Needed
    because the accumulation loop compares against the limit it was given, not
    against the package constant, so a check that only looked for the constant's
    name would report a helper that takes a bound and ignores it as healthy --
    which is exactly how the first version of this kind failed.
    """
    m = re.match(r"^func\s+(?:\([^)]*\)\s*)?[A-Za-z0-9_]+", body_text)
    if not m:
        return []
    i = body_text.find("(", m.end())
    if i < 0:
        return []
    names, pending = [], []
    for piece in _split_args(_balanced_from(body_text, i)):
        parts = piece.split()
        if len(parts) < 2:
            # `a, b int` arrives here as the bare `a`, whose type is the NEXT
            # piece's. Held until a piece names one.
            pending.append(piece)
            continue
        group, pending = [*pending, parts[0]], []
        if _INT_TYPE.fullmatch(" ".join(parts[1:]).strip()):
            names.extend(group)
    return names


def _derived_locals(lines):
    """Local names assigned FROM the ceiling: `limit := maxMessageBytes.Load()`.

    Without this the kind would be enforcing a code LAYOUT rather than a bound.
    A loop that reads the effective ceiling into a local and orders the
    accumulated length against that local is correctly guarded, and flagging it
    because the bound arrived as an assignment rather than as a parameter is the
    kind of false positive that gets a guard argued with instead of fixed --
    `defer-in-loop`'s whole history in this file.
    """
    out = []
    for line in lines:
        m = _ASSIGN.match(line)
        if m and (TDS_CEILING in m.group(2) or TDS_EFFECTIVE in m.group(2)):
            out.append(m.group(1))
    return out


def _ordering_line(lines, names):
    """The first index in `lines` that ORDERS something against one of `names`.

    None when nothing does, which is the finding: a bound that is never on
    either side of a `<`, `<=`, `>` or `>=` is a bound nothing is checked
    against. What this does NOT claim is that the comparison is arithmetically
    right -- that the accumulated length is what is on the other side of it, and
    that the bound is inclusive. Those are held by internal/tds/maxmessage_test.go,
    which exercises the refusal and its tightness. This holds the half a test
    cannot: that the comparison is still THERE, in the function that joins, after
    a refactor no test would fail on.
    """
    for i, line in enumerate(lines):
        text = _SHIFTS.sub("~", line)
        for n in names:
            esc = re.escape(n)
            if re.search(rf"\b{esc}\b\s*{_ORDERING}", text) or \
                    re.search(rf"{_ORDERING}\s*\b{esc}\b", text):
                return i
    return None


def _balanced_from(text, start):
    """The text inside the `(` or `{` at text[start], to its matching close.

    Returns the remainder if it never closes, which is the safe direction: an
    unterminated call reads as "everything after it" and so is checked for a
    handler rather than silently skipped.
    """
    depth = 0
    for i in range(start, len(text)):
        if text[i] in "([{":
            depth += 1
        elif text[i] in ")]}":
            depth -= 1
            if depth == 0:
                return text[start + 1:i]
    return text[start + 1:]


def _split_args(text):
    """Comma-separated pieces at the TOP level of `text`.

    `http.Serve(ln, mw(srv.Handler(), opts))` has two arguments, not three; a
    naive split on commas would read the handler as `opts` and report a correct
    site as unbounded.
    """
    out, depth, cur = [], 0, []
    for ch in text:
        if ch in "([{":
            depth += 1
        elif ch in ")]}":
            depth -= 1
        if ch == "," and depth == 0:
            out.append("".join(cur))
            cur = []
        else:
            cur.append(ch)
    out.append("".join(cur))
    return [a.strip() for a in out if a.strip()]


def serve_sites(path, source=None, root=None):
    """Every HTTP serve site in one file, with the handler it is handed.

    `handler` is "" when the site names none at all -- `http.Serve(ln, nil)`, or
    an `&http.Server{...}` with no Handler field. Both mean
    http.DefaultServeMux, which means no outer bound, so "" must read as a
    finding rather than as "nothing to check".
    """
    text = source if source is not None else path.read_text(encoding="utf-8")
    raw_lines = text.split("\n")
    clean_lines = _strip_noise(text).split("\n")
    rel = relkey(path, root)
    out = []
    for i, line in enumerate(clean_lines):
        m = _SERVE_SITE.search(line)
        if not m:
            continue
        # The call or composite literal may span lines, so the walk starts at
        # the opening delimiter -- always the last character of the match -- and
        # runs over the rest of the file rather than this line alone.
        window = "\n".join(clean_lines[i:])
        inner = _balanced_from(window, m.end() - 1)
        if m.group(0).rstrip().endswith("{"):
            field = _HANDLER_FIELD.search(inner)
            pieces = _split_args(field.group(1)) if field else []
            handler = pieces[0] if pieces else ""
        else:
            args = _split_args(inner)
            handler = args[-1] if args else ""
        out.append({"file": rel, "symbol": _enclosing_func(clean_lines, i),
                    "line": i + 1, "handler": handler,
                    "snippet": raw_lines[i].strip()})
    return out


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


def _tds_findings(base):
    """The second listener's ceiling, checked along the chain the bound travels.

    FOUR links, because every one of them is satisfiable while the bound does
    nothing, and the first version of this kind proved it by checking only the
    third:

      1. DECLARED -- `DefaultMaxMessageBytes` exists with a value this can read.
      2. DERIVED  -- the effective ceiling is initialised FROM that default, so
         lowering the constant lowers the bound. A `maxMessageBytes.Store(1<<20)`
         would leave the constant documenting a number nothing uses.
      3. ROUTED   -- the exported entry every TDS read goes through hands the
         effective ceiling (or the constant) to whatever does the joining. A
         literal here would bound the listener at a number the constant and the
         knob both disagree with.
      4. COMPARED -- and this is the link that was missing: the function holding
         the join must ORDER something against the bound it was handed, at or
         before the append that would cross it. The join lives in a helper, so a
         check pointed at the entry function read a one-line delegation, found
         the constant's name in it, and called a loop with no comparison at all
         healthy.

    Link 4 is located from the join rather than from a function name on purpose.
    A name in this file stays satisfied by a function that merely still exists;
    the join is satisfied only by the line that actually grows the buffer.
    """
    def finding(symbol, snippet, line=0):
        return {"file": TDS_PACKET_FILE, "symbol": symbol, "line": line,
                "kind": "unbounded-frame-accumulation", "snippet": snippet}

    path = base / TDS_PACKET_FILE
    if not path.exists():
        return [finding(TDS_CEILING, f"{TDS_PACKET_FILE} does not exist")]
    clean_lines = _strip_noise(path.read_text(encoding="utf-8")).split("\n")
    clean = "\n".join(clean_lines)

    # 1. DECLARED.
    decl = _TDS_CEILING_DECL.search(clean)
    if not decl:
        return [finding(
            TDS_CEILING,
            f"{TDS_CEILING} is not declared; the read loop joins packets until "
            "one sets EOM, so with no ceiling a peer that never sets it chooses "
            "how much the emulator accumulates")]
    out = []
    if _value_of(decl.group(1)) is None:
        out.append(finding(
            TDS_CEILING,
            f"{TDS_CEILING} has a value this checker cannot read, so the "
            "ceiling is unverified"))

    # 2. DERIVED. One line naming both is enough -- the `init` that stores the
    # default into the atomic -- and anything less specific would be satisfied by
    # the two names merely coexisting in the file.
    if not any(TDS_EFFECTIVE in ln and TDS_CEILING in ln for ln in clean_lines):
        out.append(finding(
            TDS_EFFECTIVE,
            f"no line initialises {TDS_EFFECTIVE} from {TDS_CEILING}; the "
            "effective ceiling no longer derives from the documented default, "
            f"so {TDS_CEILING} documents a number nothing enforces"))

    # 3. ROUTED.
    entry = _func_span(clean_lines, TDS_ENTRY_FUNC)
    entry_body = "" if entry is None else "\n".join(clean_lines[entry[0]:entry[1] + 1])
    if entry is None:
        out.append(finding(
            TDS_ENTRY_FUNC,
            f"{TDS_ENTRY_FUNC} is gone from {TDS_PACKET_FILE}; it was the single "
            "entry point every TDS read went through, so the ceiling now has no "
            "one place that installs it -- re-point this check rather than "
            "deleting it"))
    elif TDS_CEILING not in entry_body and TDS_EFFECTIVE not in entry_body:
        out.append(finding(
            TDS_ENTRY_FUNC,
            f"{TDS_ENTRY_FUNC} passes on neither {TDS_CEILING} nor "
            f"{TDS_EFFECTIVE}; the listener is bounded by some other number, "
            "which the constant and FABRIC_MAX_TDS_MESSAGE_BYTES both disagree "
            "with", entry[0] + 1))

    # 4. COMPARED.
    joins = [i for i, ln in enumerate(clean_lines) if _TDS_JOIN.search(ln)]
    if not joins:
        out.append(finding(
            "<join>",
            "no packet-payload join (`append(..., p.Data...)`) anywhere in "
            f"{TDS_PACKET_FILE}; this kind locates the accumulation from the "
            "join, so zero of them means it is checking nothing -- teach "
            "_TDS_JOIN whatever replaced it"))
    for i in joins:
        name = _enclosing_func(clean_lines, i)
        span = None if name == "<package>" else _func_span(clean_lines, name)
        if span is None:
            out.append(finding(
                name,
                f"the packet join at line {i + 1} is not inside a function this "
                "checker can delimit, so the comparison that should guard it "
                "cannot be located", i + 1))
            continue
        body = clean_lines[span[0]:span[1] + 1]
        bounds = [TDS_CEILING, TDS_EFFECTIVE,
                  *_bound_params("\n".join(body)), *_derived_locals(body)]
        at = _ordering_line(body, bounds)
        if at is None:
            out.append(finding(
                name,
                f"{name} joins packet payloads without ordering anything against "
                f"a ceiling ({', '.join(bounds)}); the bound is declared, "
                "derived and passed in, and then never compared against -- the "
                "shape a presence-only check reports as healthy", i + 1))
        elif span[0] + at > i:
            out.append(finding(
                name,
                f"{name} compares against its bound at line {span[0] + at + 1}, "
                f"AFTER the join at line {i + 1}; the worst case held in memory "
                "is then the ceiling plus whatever the peer had queued, not the "
                "ceiling plus the one packet that tripped it", i + 1))
    return out


def _serve_findings(base, files=None):
    """Every serve site not handed the root handler -- plus zero sites at all.

    The outer HTTP bound lives in exactly one place, so a listener handed
    anything else is a listener with no ceiling on any of the 68 decoder sites
    behind it. That is not visible as a line in internal/server: the bypass is
    written somewhere else entirely, which is why this sweeps rather than
    asserts.
    """
    paths = files if files is not None else go_files(base)
    out, total = [], 0
    for path in paths:
        for site in serve_sites(path, root=base):
            total += 1
            if _HANDLER_CALL.search(site["handler"]):
                continue
            named = site["handler"] or "no handler named, i.e. http.DefaultServeMux"
            out.append({"file": site["file"], "symbol": site["symbol"],
                        "line": site["line"],
                        "kind": "unbounded-serve-root",
                        "snippet": f"serves {named}, not Server.Handler(): the "
                                   "outer request-body bound is installed there "
                                   "and nowhere else"})
    if total == 0:
        out.append({"file": SERVE_ROOT_ANCHOR, "symbol": "<none>", "line": 0,
                    "kind": "unbounded-serve-root",
                    "snippet": "no http.Serve/ListenAndServe/http.Server site "
                               f"anywhere under {', '.join(SCAN_ROOTS)}; a sweep "
                               "that covers nothing passes vacuously, so teach "
                               "_SERVE_SITE about whatever replaced it"})
    return out


def structural_findings(root=None, files=None):
    """The findings that are about the REPAIR rather than the original shape.

    All four are absence-shaped, and absence is what a sweep cannot report: no
    line anywhere says "the outer bound is missing". Without these the checker
    would pass, in perpetuity, on a tree whose one ceiling had been refactored
    away -- reporting zero unbounded reads, which would be true and beside the
    point. `files`, when given, is the sweep's file list, reused so the serve
    sweep does not walk the tree a second time.
    """
    base = ROOT if root is None else pathlib.Path(root)
    out = []
    out.extend(_tds_findings(base))
    out.extend(_serve_findings(base, files))

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
    paths = files if files is not None else go_files(root)
    out = []
    for path in paths:
        out.extend(findings_for(path, root=root))
    out.extend(structural_findings(root, files=paths))
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
            "http.MaxBytesReader in place, hoist a regexp to package scope, "
            f"keep {TDS_CEILING} derived, routed by tds.{TDS_ENTRY_FUNC} and "
            "compared against where the packet join happens, hand every serve "
            "site Server.Handler(), or record the site with its reason.")
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
