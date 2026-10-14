"""Tests for the performance-regression checker.

THE SAME AWKWARD CASE its three flakiness siblings document: every invariant this
checker guards is HELD as of the commit that added it. The two bare
`io.ReadAll(r.Body)` sites were repaired onto `httpx.ReadBounded` in the same
change, all nine `regexp.MustCompile` sites are package-level, the outer bound is
installed, and it is ordered correctly above every inner ceiling. Running the
checker against the real tree therefore proves only that it did not crash on it.

So every test here drives it against a violation it MUST catch, and against a
correct-looking near-miss it must NOT catch. The near-misses are the valuable
half: a checker that flagged `resp.Body` reads, or package-level regexps, would
report dozens of findings on a healthy tree and get argued with rather than
fixed — which is how the Go flakiness checker's own false-positive history went.

Writing these found one real defect in the checker, recorded at
`test_the_outer_bound_is_found_when_declared_as_a_standalone_const`: the ceiling
regex anchored at `^\\s*<name>` and so matched every ceiling inside body.go's
`const (...)` block while missing the standalone `const DefaultMaxRequestBody =`,
which made the ordering check report the outer bound as ABSENT instead of
comparing against it.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_perf_regressions as c  # noqa: E402

# A body.go that is correct: eight inner ceilings, all strictly below the outer
# one, with the outer declared the way the real file declares it.
GOOD_BODY_GO = """package httpx

const (
\tMaxDFSAppend = 100 << 20
\tMaxBlobWrite = 256 << 20
\tMaxControlBody = 1 << 20
)

const DefaultMaxRequestBody = 320 << 20
"""

GOOD_SERVER_GO = """package server

func (s *Server) boundBodies(next http.Handler) http.Handler {
\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
\t\tr.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MaxRequestBytes)
\t\tnext.ServeHTTP(w, r)
\t})
}
"""

# A packet.go that is correct, in all four of the links the kind checks: the
# default DECLARED with a readable value, the effective ceiling DERIVED from it,
# the entry point ROUTING it, and the function that joins payloads COMPARING
# against the bound it was handed, before the append. Written out in full rather
# than abbreviated, because the abbreviation is what hid the defect — see
# `test_the_join_moved_into_a_helper_without_the_comparison_is_flagged`.
GOOD_PACKET_GO = """package tds

const DefaultMaxMessageBytes = 320 << 20

var maxMessageBytes atomic.Int64

func init() { maxMessageBytes.Store(DefaultMaxMessageBytes) }

func ReadMessage(r io.Reader) (typ byte, data []byte, err error) {
\treturn readMessageBounded(r, maxMessageBytes.Load())
}

func readMessageBounded(r io.Reader, limit int64) (typ byte, data []byte, err error) {
\tfor {
\t\tp, err := ReadPacket(r)
\t\tif err != nil {
\t\t\treturn 0, nil, err
\t\t}
\t\tif total := int64(len(data)) + int64(len(p.Data)); total > limit {
\t\t\treturn 0, nil, fmt.Errorf("too big")
\t\t}
\t\tdata = append(data, p.Data...)
\t\tif p.EOM() {
\t\t\treturn typ, data, nil
\t\t}
\t}
}
"""

# A main.go that is correct: the one serve site, handed the root handler.
GOOD_MAIN_GO = """package main

func run() error {
\treturn http.Serve(ln, srv.Handler())
}
"""


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """Write a fake Go tree and point the checker's module constants at it.

    `files` maps repo-relative paths to source text. The structural kinds read
    fixed paths (internal/httpx/body.go, internal/server/server.go,
    internal/tds/packet.go) and the serve sweep fails on a tree with no serve
    site at all, so a healthy set of all four is written unless the caller
    overrides them — otherwise every test would trip those kinds and drown the
    one it is about.
    """
    def build(files, ledger=None):
        root = tmp_path / "repo"
        base = {
            c.CEILINGS_FILE: GOOD_BODY_GO,
            c.ROOT_HANDLER_FILE: GOOD_SERVER_GO,
            c.TDS_PACKET_FILE: GOOD_PACKET_GO,
            c.SERVE_ROOT_ANCHOR: GOOD_MAIN_GO,
        }
        base.update(files)
        for rel, text in base.items():
            p = root / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(text, encoding="utf-8")
        # Every scan root must exist, or `missing_scan_roots` fires first and
        # the test reads as a pass for the wrong reason.
        for r in c.SCAN_ROOTS:
            (root / r).mkdir(parents=True, exist_ok=True)
        led = root / "docs" / "perf-regressions.json"
        led.parent.mkdir(parents=True, exist_ok=True)
        led.write_text(json.dumps(ledger if ledger is not None else {"accepted": []}),
                       encoding="utf-8")
        monkeypatch.setattr(c, "ROOT", root)
        monkeypatch.setattr(c, "LEDGER", led)
        return root
    return build


def keys(findings):
    return {c.ledger_key(f) for f in findings}


def kinds(findings):
    return sorted(f["kind"] for f in findings)


# --------------------------------------------------------------------------
# unbounded-body-read
# --------------------------------------------------------------------------

def test_a_bare_request_body_read_is_flagged(tree):
    tree({"internal/api/h.go": """package api

func handle(w http.ResponseWriter, r *http.Request) {
\tbody, _ := io.ReadAll(r.Body)
\t_ = body
}
"""})
    found = c.scan()
    assert kinds(found) == ["unbounded-body-read"]
    assert "internal/api/h.go:handle:unbounded-body-read" in keys(found)


def test_io_copy_from_a_request_body_is_flagged(tree):
    """The other unbounded shape: streamed somewhere else rather than buffered.

    Still caller-chosen work with no ceiling, so it is the same kind — the
    destination does not make the size bounded.
    """
    tree({"internal/api/h.go": """package api

func handle(w http.ResponseWriter, r *http.Request) {
\t_, _ = io.Copy(dst, r.Body)
}
"""})
    assert kinds(c.scan()) == ["unbounded-body-read"]


def test_a_bounded_read_is_not_flagged(tree):
    """The repair must read as clean, or the checker argues with its own fix."""
    tree({"internal/api/h.go": """package api

func handle(w http.ResponseWriter, r *http.Request) {
\tbody, ok := httpx.ReadBounded(r.Body, httpx.MaxControlBody)
\t_, _ = body, ok
}
"""})
    assert c.scan() == []


def test_a_response_body_read_is_not_flagged(tree):
    """THE NEAR-MISS THAT MATTERS MOST. There are 3 `io.ReadAll(resp.Body)` sites
    in the real tree and 6 `json.NewDecoder(resp.Body)` ones. A checker keying on
    `.Body` rather than on the RECEIVER would report all nine as unbounded
    per-request work, which they are not: an engine the emulator relayed to is
    not an untrusted caller choosing a size, and those reads are bounded on their
    own terms where it matters (mlflow.go and kql.go both use MaxProxyBody).
    """
    tree({"internal/api/h.go": """package api

func relay() {
\tbody, err := io.ReadAll(resp.Body)
\t_, _ = body, err
}
"""})
    assert c.scan() == []


def test_a_test_file_is_not_scanned(tree):
    """Test files may demonstrate the banned shape while proving it is bad — the
    same carve-out internal/httpx/guard_test.go makes for itself."""
    tree({"internal/api/h_test.go": """package api

func TestX(t *testing.T) {
\tbody, _ := io.ReadAll(r.Body)
\t_ = body
}
"""})
    assert c.scan() == []


def test_a_comment_or_string_mentioning_the_shape_is_not_flagged(tree):
    """Without the comment/string strip, this file's own prose would be a
    finding. The checker's docstring names `io.ReadAll(r.Body)` repeatedly."""
    tree({"internal/api/h.go": '''package api

// Never write io.ReadAll(r.Body) here.
func handle() {
\tmsg := "io.ReadAll(r.Body) is banned"
\t_ = msg
}
'''})
    assert c.scan() == []


# --------------------------------------------------------------------------
# recompiled-regexp
# --------------------------------------------------------------------------

def test_a_package_level_regexp_is_not_flagged(tree):
    """All 9 real sites are this shape: compiled once at init, not per call."""
    tree({"internal/api/h.go": """package api

var pat = regexp.MustCompile(`^a+$`)

func handle() {
\t_ = pat
}
"""})
    assert c.scan() == []


def test_a_function_scoped_regexp_is_flagged(tree):
    tree({"internal/api/h.go": """package api

func handle() {
\tpat := regexp.MustCompile(`^a+$`)
\t_ = pat
}
"""})
    found = c.scan()
    assert kinds(found) == ["recompiled-regexp"]
    assert "internal/api/h.go:handle:recompiled-regexp" in keys(found)


def test_a_regexp_after_a_function_closes_is_package_scope(tree):
    """The brace walk must not attribute a later package-level var to the
    function above it — that would flag a correct site and name the wrong
    symbol while doing it."""
    tree({"internal/api/h.go": """package api

func handle() {
\t_ = 1
}

var pat = regexp.MustCompile(`^a+$`)
"""})
    assert c.scan() == []


# --------------------------------------------------------------------------
# missing-outer-bound
# --------------------------------------------------------------------------

def test_a_root_handler_without_maxbytesreader_is_flagged(tree):
    """The 68 decoder sites' only ceiling. If it is refactored away they all
    silently revert to unbounded, and no per-line sweep would report it —
    absence is not a line anywhere."""
    tree({}, )
    (c.ROOT / c.ROOT_HANDLER_FILE).write_text(
        "package server\n\nfunc (s *Server) Handler() http.Handler {\n\treturn s.mux\n}\n",
        encoding="utf-8")
    found = c.scan()
    assert "missing-outer-bound" in kinds(found)


def test_a_commented_out_maxbytesreader_does_not_satisfy_the_check(tree):
    """A bound in a comment is not a bound. The strip runs before this test so a
    commented-out call cannot hold the invariant open."""
    tree({})
    (c.ROOT / c.ROOT_HANDLER_FILE).write_text(
        "package server\n\n// r.Body = http.MaxBytesReader(w, r.Body, max)\n"
        "func (s *Server) Handler() http.Handler { return s.mux }\n",
        encoding="utf-8")
    assert "missing-outer-bound" in kinds(c.scan())


# --------------------------------------------------------------------------
# ceiling-above-outer-bound — the defect an earlier design would have shipped
# --------------------------------------------------------------------------

def test_an_inner_ceiling_equal_to_the_outer_bound_is_flagged(tree):
    """EQUAL, not just greater, and that is the whole point of this kind.

    ReadBounded probes max+1, so an outer limiter set AT an inner ceiling makes
    the probe itself trip: the oversized write is still refused, but with
    net/http's generic message instead of httpx's specific fit-vs-truncated one.
    A 256 MiB outer bound would have shipped that while looking correct.
    """
    tree({}, )
    (c.ROOT / c.CEILINGS_FILE).write_text(
        "package httpx\n\nconst (\n\tMaxBlobWrite = 256 << 20\n)\n\n"
        "const DefaultMaxRequestBody = 256 << 20\n", encoding="utf-8")
    found = c.scan()
    assert "ceiling-above-outer-bound" in kinds(found)
    assert "internal/httpx/body.go:MaxBlobWrite:ceiling-above-outer-bound" in keys(found)


def test_an_inner_ceiling_above_the_outer_bound_is_flagged(tree):
    tree({})
    (c.ROOT / c.CEILINGS_FILE).write_text(
        "package httpx\n\nconst (\n\tMaxBlobWrite = 512 << 20\n)\n\n"
        "const DefaultMaxRequestBody = 320 << 20\n", encoding="utf-8")
    assert "ceiling-above-outer-bound" in kinds(c.scan())


def test_the_outer_bound_is_found_when_declared_as_a_standalone_const(tree):
    """THE REGRESSION TEST FOR A REAL DEFECT IN THIS CHECKER.

    The ceiling regex first anchored at `^\\s*<name>`, which matches a name
    indented inside a `const (...)` block and NOT `const DefaultMaxRequestBody =`
    at column zero. Every inner ceiling was read and the outer one was not, so
    the checker reported it as missing — a finding, on a correct tree, naming the
    opposite of the problem. Found by running the checker against the tree it
    ships with, which is the only way it would have been found.
    """
    tree({})
    values, unreadable = c.ceilings_of(GOOD_BODY_GO)
    assert unreadable == []
    assert values["DefaultMaxRequestBody"] == 320 << 20
    assert values["MaxBlobWrite"] == 256 << 20
    assert c.scan() == []


def test_a_ceiling_this_cannot_read_is_reported_rather_than_passed(tree):
    """An unreadable value must not silently satisfy the ordering check.

    A ceiling built from another named constant is not resolved by `_value_of`.
    Treating that as "fine" would mean the invariant is unenforced for exactly
    the ceiling nobody can see, which is the quiet direction.
    """
    tree({})
    (c.ROOT / c.CEILINGS_FILE).write_text(
        "package httpx\n\nconst (\n\tMaxBlobWrite = someOtherConst * 2\n)\n\n"
        "const DefaultMaxRequestBody = 320 << 20\n", encoding="utf-8")
    found = c.scan()
    assert "ceiling-above-outer-bound" in kinds(found)
    assert any("cannot read" in f["snippet"] for f in found)


def test_a_missing_outer_bound_constant_is_flagged(tree):
    tree({})
    (c.ROOT / c.CEILINGS_FILE).write_text(
        "package httpx\n\nconst (\n\tMaxBlobWrite = 256 << 20\n)\n", encoding="utf-8")
    found = c.scan()
    assert "ceiling-above-outer-bound" in kinds(found)
    assert any("is missing" in f["snippet"] for f in found)


# --------------------------------------------------------------------------
# unbounded-frame-accumulation — the SECOND listener, which every kind above
# is blind to by construction: a raw TCP listener has no r.Body to wrap
# --------------------------------------------------------------------------

def test_a_missing_message_ceiling_is_flagged(tree):
    """The shape as it stood: packets joined until EOM, with no cap on the
    total. Each ReadPacket is self-limiting at 64 KiB (the length field is a
    uint16), which is exactly why the loop around it read as bounded."""
    tree({c.TDS_PACKET_FILE: """package tds

func ReadMessage(r io.Reader) (typ byte, data []byte, err error) {
\tfor {
\t\tp, err := ReadPacket(r)
\t\tif err != nil {
\t\t\treturn 0, nil, err
\t\t}
\t\tdata = append(data, p.Data...)
\t\tif p.EOM() {
\t\t\treturn typ, data, nil
\t\t}
\t}
}
"""})
    found = c.scan()
    assert kinds(found) == ["unbounded-frame-accumulation"]
    assert (f"{c.TDS_PACKET_FILE}:{c.TDS_CEILING}:unbounded-frame-accumulation"
            in keys(found))
    assert any("never sets it" in f["snippet"] for f in found)


def test_a_missing_packet_file_is_flagged(tree):
    """A moved or deleted file must not read as "nothing to check" — the same
    direction `missing_scan_roots` guards for the sweep."""
    tree({})
    (c.ROOT / c.TDS_PACKET_FILE).unlink()
    found = c.scan()
    assert "unbounded-frame-accumulation" in kinds(found)
    assert any("does not exist" in f["snippet"] for f in found)


def test_the_join_moved_into_a_helper_without_the_comparison_is_flagged(tree):
    """THE REGRESSION TEST FOR THIS KIND'S OWN FIRST VERSION, and the reason it
    now locates the accumulation from the join rather than from a function name.

    The first version asked whether `ReadMessage` mentioned the ceiling. By the
    time it shipped, `ReadMessage` was a one-line delegation to a helper — so it
    mentioned the ceiling, the check passed, and deleting the comparison from the
    HELPER changed nothing it could see. This fixture is that tree: the constant
    declared, derived and passed in, and then never compared against. Everything
    a presence grep looks for is present.
    """
    tree({c.TDS_PACKET_FILE: """package tds

const DefaultMaxMessageBytes = 320 << 20

var maxMessageBytes atomic.Int64

func init() { maxMessageBytes.Store(DefaultMaxMessageBytes) }

func ReadMessage(r io.Reader) (typ byte, data []byte, err error) {
\treturn readMessageBounded(r, maxMessageBytes.Load())
}

func readMessageBounded(r io.Reader, limit int64) (typ byte, data []byte, err error) {
\tfor {
\t\tp, err := ReadPacket(r)
\t\tif err != nil {
\t\t\treturn 0, nil, err
\t\t}
\t\tdata = append(data, p.Data...)
\t\tif p.EOM() {
\t\t\treturn typ, data, nil
\t\t}
\t}
}
"""})
    found = c.scan()
    assert kinds(found) == ["unbounded-frame-accumulation"]
    # Attributed to the HELPER, not to the entry point: the line that needs the
    # comparison back is the one that joins.
    assert (f"{c.TDS_PACKET_FILE}:readMessageBounded:unbounded-frame-accumulation"
            in keys(found))
    assert any("never compared against" in f["snippet"] for f in found)


def test_a_comparison_the_join_happens_before_is_flagged(tree):
    """"Bounded" and "bounded before the allocation that would cross the bound"
    are different claims, and packet.go's comment makes the second one.

    Checking after the append means the worst case held in memory is the ceiling
    plus whatever the peer had queued, not the ceiling plus the one packet that
    tripped it — and a test that only asserts "an endless stream is eventually
    refused" passes either way.
    """
    tree({c.TDS_PACKET_FILE: """package tds

const DefaultMaxMessageBytes = 320 << 20

var maxMessageBytes atomic.Int64

func init() { maxMessageBytes.Store(DefaultMaxMessageBytes) }

func ReadMessage(r io.Reader) (typ byte, data []byte, err error) {
\treturn readMessageBounded(r, maxMessageBytes.Load())
}

func readMessageBounded(r io.Reader, limit int64) (typ byte, data []byte, err error) {
\tfor {
\t\tp, err := ReadPacket(r)
\t\tdata = append(data, p.Data...)
\t\tif int64(len(data)) > limit {
\t\t\treturn 0, nil, fmt.Errorf("too big")
\t\t}
\t\tif p.EOM() {
\t\t\treturn typ, data, nil
\t\t}
\t}
}
"""})
    found = c.scan()
    assert kinds(found) == ["unbounded-frame-accumulation"]
    assert any("AFTER the join" in f["snippet"] for f in found)


def test_an_equality_check_is_not_a_ceiling_check(tree):
    """`total == limit` is not a bound: it refuses one exact size and admits
    every larger one. Only the ORDERING operators count, which is why the
    comparison regex excludes `==` rather than looking for the bound's name near
    any operator at all."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace("total > limit",
                                                    "total == limit")})
    found = c.scan()
    assert kinds(found) == ["unbounded-frame-accumulation"]
    assert any("never compared against" in f["snippet"] for f in found)


def test_a_shift_is_not_mistaken_for_a_comparison(tree):
    """The near-miss on the other side: `<<` contains `<`. A helper that shifts
    its bound and never compares it would read as guarded by a regex that looked
    for the name next to a `<`, so shifts are blanked before matching."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace(
        "if total := int64(len(data)) + int64(len(p.Data)); total > limit {",
        "if words := limit << 3; words == 0 {")})
    found = c.scan()
    assert kinds(found) == ["unbounded-frame-accumulation"]
    assert any("never compared against" in f["snippet"] for f in found)


def test_the_bound_may_be_a_grouped_int_parameter(tree):
    """The near-miss that would make this kind noisy: `func f(a, b int64)` gives
    `b` the type and leaves `a` bare, so a parameter-type walk that required each
    piece to name its own type would miss half the bounds a helper could be
    handed and flag a correctly-guarded loop."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace(
        "func readMessageBounded(r io.Reader, limit int64)",
        "func readMessageBounded(r io.Reader, limit, other int64)")})
    assert c.scan() == []


def test_the_join_inside_the_entry_point_itself_is_accepted(tree):
    """A helper is not required. If the join moves back into ReadMessage and the
    comparison comes with it, the invariant holds and this must stay silent —
    otherwise the kind would be enforcing a code layout rather than a bound."""
    tree({c.TDS_PACKET_FILE: """package tds

const DefaultMaxMessageBytes = 320 << 20

var maxMessageBytes atomic.Int64

func init() { maxMessageBytes.Store(DefaultMaxMessageBytes) }

func ReadMessage(r io.Reader) (typ byte, data []byte, err error) {
\tlimit := maxMessageBytes.Load()
\tfor {
\t\tp, err := ReadPacket(r)
\t\tif err != nil {
\t\t\treturn 0, nil, err
\t\t}
\t\tif int64(len(data)+len(p.Data)) > limit {
\t\t\treturn 0, nil, fmt.Errorf("too big")
\t\t}
\t\tdata = append(data, p.Data...)
\t\tif p.EOM() {
\t\t\treturn typ, data, nil
\t\t}
\t}
}
"""})
    assert c.scan() == []


def test_an_effective_ceiling_that_no_longer_derives_from_the_default_is_flagged(tree):
    """Link 2. The constant can stay in the file, correct and documented, while
    the ceiling actually in force is some other number — and then lowering the
    constant lowers nothing. `grep DefaultMaxMessageBytes` says yes; the bound
    the listener enforces is 1 MiB."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace(
        "func init() { maxMessageBytes.Store(DefaultMaxMessageBytes) }",
        "func init() { maxMessageBytes.Store(1 << 20) }")})
    found = c.scan()
    assert kinds(found) == ["unbounded-frame-accumulation"]
    assert (f"{c.TDS_PACKET_FILE}:{c.TDS_EFFECTIVE}:unbounded-frame-accumulation"
            in keys(found))
    assert any("no longer derives" in f["snippet"] for f in found)


def test_an_entry_point_passing_a_literal_is_flagged(tree):
    """Link 3. A number spelled at the call site is a bound that neither the
    documented constant nor FABRIC_MAX_TDS_MESSAGE_BYTES can move, so the knob
    silently stops working while the loop still looks guarded."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace(
        "readMessageBounded(r, maxMessageBytes.Load())",
        "readMessageBounded(r, 1<<20)")})
    found = c.scan()
    assert kinds(found) == ["unbounded-frame-accumulation"]
    assert (f"{c.TDS_PACKET_FILE}:{c.TDS_ENTRY_FUNC}:unbounded-frame-accumulation"
            in keys(found))
    assert any("passes on neither" in f["snippet"] for f in found)


def test_a_ceiling_named_only_in_a_comment_does_not_consult_it(tree):
    """A bound in a comment is not a bound — the same thing
    `test_a_commented_out_maxbytesreader_does_not_satisfy_the_check` pins for
    the HTTP half. The strip runs first, so prose cannot hold this open."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace(
        "if total := int64(len(data)) + int64(len(p.Data)); total > limit {",
        "if false { // TODO: compare the total against limit here.")})
    found = c.scan()
    assert kinds(found) == ["unbounded-frame-accumulation"]
    assert any("never compared against" in f["snippet"] for f in found)


def test_a_vanished_entry_point_is_flagged_rather_than_assumed_fine(tree):
    """A renamed entry point means the guard is pointing at nothing. That wants
    re-pointing, and the only way anyone learns it needs re-pointing is if the
    checker says so."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace("func ReadMessage(",
                                                    "func ReadMsg(")})
    found = c.scan()
    assert "unbounded-frame-accumulation" in kinds(found)
    assert any("re-point this check" in f["snippet"] for f in found)


def test_a_join_this_cannot_locate_is_flagged_rather_than_passing_vacuously(tree):
    """Zero recognised joins is itself a finding, the same
    covers-nothing-passes-vacuously direction `unbounded-serve-root` takes for
    zero serve sites. A kind that locates its subject by pattern passes silently
    and permanently once the pattern stops matching."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace(
        "data = append(data, p.Data...)", "data = joinInto(data, p)")})
    found = c.scan()
    assert kinds(found) == ["unbounded-frame-accumulation"]
    assert f"{c.TDS_PACKET_FILE}:<join>:unbounded-frame-accumulation" in keys(found)
    assert any("teach _TDS_JOIN" in f["snippet"] for f in found)


def test_a_message_ceiling_this_cannot_read_is_reported_rather_than_passed(tree):
    """Same reasoning as the httpx ceilings: a value the checker cannot read is
    a ceiling whose size nobody is checking, and silence there is the quiet
    direction."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace(
        "const DefaultMaxMessageBytes = 320 << 20",
        "const DefaultMaxMessageBytes = someOtherConst * 2")})
    found = c.scan()
    assert "unbounded-frame-accumulation" in kinds(found)
    assert any("cannot read" in f["snippet"] for f in found)


def test_a_declared_ceiling_inside_a_const_block_is_accepted(tree):
    """The declaration form the real file does not use today but may tomorrow.
    The HTTP half shipped a defect of exactly this shape — a regex that read one
    declaration style and silently missed the other."""
    tree({c.TDS_PACKET_FILE: GOOD_PACKET_GO.replace(
        "const DefaultMaxMessageBytes = 320 << 20",
        "const (\n\theaderLen = 8\n\tDefaultMaxMessageBytes = 320 << 20\n)")})
    assert c.scan() == []


def test_the_real_tree_satisfies_every_link_of_the_chain(tree):
    """The real packet.go, read by the real checker, with nothing stubbed. The
    fixtures above all derive from GOOD_PACKET_GO, so without this nothing here
    would notice the fixture drifting away from the file it stands in for."""
    assert c._tds_findings(pathlib.Path(__file__).resolve().parents[2]) == []


# --------------------------------------------------------------------------
# unbounded-serve-root — a second HTTP listener handed anything but the root
# handler is a listener with no ceiling on any of the 68 decoder sites
# --------------------------------------------------------------------------

def test_the_real_serve_form_is_not_flagged(tree):
    """`http.Serve(ln, srv.Handler())` — the one site in the tree. Asserted
    explicitly rather than left implicit in the fixture, because this kind
    sweeping the whole tree means a false positive here is a false positive
    everywhere."""
    tree({})
    assert c.scan() == []
    sites = c.serve_sites(c.ROOT / c.SERVE_ROOT_ANCHOR, root=c.ROOT)
    assert [s["handler"] for s in sites] == ["srv.Handler()"]


def test_a_serve_site_handed_a_mux_is_flagged(tree):
    """The bypass this kind exists for: a second listener, serving the same
    routes, with the outer body bound nowhere in the chain."""
    tree({c.SERVE_ROOT_ANCHOR: """package main

func run() error {
\treturn http.Serve(ln, srv.mux)
}
"""})
    found = c.scan()
    assert kinds(found) == ["unbounded-serve-root"]
    assert f"{c.SERVE_ROOT_ANCHOR}:run:unbounded-serve-root" in keys(found)


def test_a_bare_handlerfunc_is_flagged(tree):
    tree({c.SERVE_ROOT_ANCHOR: """package main

func run() error {
\treturn http.ListenAndServe(":0", http.HandlerFunc(handle))
}
"""})
    assert kinds(c.scan()) == ["unbounded-serve-root"]


def test_the_tls_form_takes_its_handler_from_the_last_argument(tree):
    """ListenAndServeTLS has four arguments, not two. A check that read the
    second would report `certFile` as the handler and flag a correct site."""
    tree({c.SERVE_ROOT_ANCHOR: """package main

func run() error {
\treturn http.ListenAndServeTLS(":0", "c.pem", "k.pem", srv.Handler())
}
"""})
    assert c.scan() == []


def test_an_http_server_literal_is_checked_too(tree):
    """The form that does not look like a call at all, and the one a reader
    adding TLS or timeouts reaches for first."""
    tree({c.SERVE_ROOT_ANCHOR: """package main

func run() error {
\ts := &http.Server{
\t\tAddr:    ":0",
\t\tHandler: srv.mux,
\t}
\treturn s.ListenAndServe()
}
"""})
    found = c.scan()
    assert kinds(found) == ["unbounded-serve-root"]
    assert any("srv.mux" in f["snippet"] or "not Server.Handler()" in f["snippet"]
               for f in found)


def test_an_http_server_literal_with_the_root_handler_is_not_flagged(tree):
    """Multi-line, with the Handler field several lines below the match — the
    reason the walk runs past the end of the matched line."""
    tree({c.SERVE_ROOT_ANCHOR: """package main

func run() error {
\ts := &http.Server{
\t\tAddr:              ":0",
\t\tReadHeaderTimeout: 5,
\t\tHandler:           srv.Handler(),
\t}
\treturn s.ListenAndServe()
}
"""})
    assert c.scan() == []


def test_an_http_server_literal_with_no_handler_field_is_flagged(tree):
    """No Handler field means http.DefaultServeMux, which means no bound. The
    absence has to read as a finding rather than as nothing to check."""
    tree({c.SERVE_ROOT_ANCHOR: """package main

func run() error {
\ts := &http.Server{Addr: ":0"}
\treturn s.ListenAndServe()
}
"""})
    found = c.scan()
    assert kinds(found) == ["unbounded-serve-root"]
    assert any("DefaultServeMux" in f["snippet"] for f in found)


def test_a_nil_handler_is_flagged(tree):
    tree({c.SERVE_ROOT_ANCHOR: """package main

func run() error {
\treturn http.ListenAndServe(":0", nil)
}
"""})
    assert kinds(c.scan()) == ["unbounded-serve-root"]


def test_a_root_handler_wrapped_in_middleware_is_not_flagged(tree):
    """THE NEAR-MISS THAT MATTERS. The invariant is that the root handler is in
    the chain, not that it is the outermost wrapper — the bound is installed
    inside it either way. Flagging this would make the kind argue with any
    future tracing or logging wrapper, and a guard that argues gets deleted.

    It is also why top-level comma splitting is not optional: a naive split
    would read the handler as `opts`.
    """
    tree({c.SERVE_ROOT_ANCHOR: """package main

func run() error {
\treturn http.Serve(ln, withTracing(srv.Handler(), opts))
}
"""})
    assert c.scan() == []


def test_servecontent_and_servemux_are_not_serve_sites(tree):
    """`http.ServeContent`, `http.ServeFile` and `http.ServeMux` share a prefix
    with `http.Serve` and install no handler at all. Matching them would report
    findings on every file that serves a static asset."""
    tree({"internal/api/h.go": """package api

var mux = new(http.ServeMux)

func handle(w http.ResponseWriter, r *http.Request) {
\thttp.ServeContent(w, r, "f", t, rs)
\thttp.ServeFile(w, r, "f")
}
"""})
    assert c.scan() == []
    assert c.serve_sites(c.ROOT / "internal/api/h.go", root=c.ROOT) == []


def test_no_serve_site_at_all_is_flagged(tree):
    """A sweep that covers nothing passes vacuously. If the serve call moves
    behind a helper this regex does not know, the kind must say so rather than
    report a clean tree — the same guard every sibling checker carries."""
    tree({})
    (c.ROOT / c.SERVE_ROOT_ANCHOR).unlink()
    found = c.scan()
    assert kinds(found) == ["unbounded-serve-root"]
    assert any("passes vacuously" in f["snippet"] for f in found)


def test_a_serve_site_in_a_test_file_is_not_swept(tree):
    """httptest spins up servers with bare handlers by design; a test is not a
    listener the emulator ships."""
    tree({"internal/api/h_test.go": """package api

func TestX(t *testing.T) {
\tgo http.Serve(ln, mux)
}
"""})
    assert c.scan() == []


# --------------------------------------------------------------------------
# the ledger, in both directions
# --------------------------------------------------------------------------

def test_a_ledger_entry_suppresses_a_finding(tree):
    tree({"internal/api/h.go": """package api

func handle(w http.ResponseWriter, r *http.Request) {
\tbody, _ := io.ReadAll(r.Body)
\t_ = body
}
"""}, ledger={"accepted": [{
        "file": "internal/api/h.go", "symbol": "handle",
        "kind": "unbounded-body-read", "reason": "accepted for the test"}]})
    assert c.main(["check_perf_regressions.py", "--strict"]) == 0


def test_an_unrecorded_finding_fails_strict(tree):
    tree({"internal/api/h.go": """package api

func handle(w http.ResponseWriter, r *http.Request) {
\tbody, _ := io.ReadAll(r.Body)
\t_ = body
}
"""})
    assert c.main(["check_perf_regressions.py", "--strict"]) == 1


def test_a_stale_ledger_entry_fails_strict(tree):
    """The direction a 'what's new' reader never checks. A one-directional ledger
    only ever grows, and a stale allowance goes on excusing a site that no longer
    exists — silently re-covering it if the shape ever comes back."""
    tree({}, ledger={"accepted": [{
        "file": "internal/api/vanished.go", "symbol": "gone",
        "kind": "unbounded-body-read", "reason": "the site was deleted"}]})
    assert c.main(["check_perf_regressions.py", "--strict"]) == 1


def test_plain_mode_reports_and_exits_zero(tree, capsys):
    tree({"internal/api/h.go": """package api

func handle(w http.ResponseWriter, r *http.Request) {
\tbody, _ := io.ReadAll(r.Body)
\t_ = body
}
"""})
    assert c.main(["check_perf_regressions.py"]) == 0
    out = capsys.readouterr().out
    assert "NEW" in out and "unbounded-body-read" in out


def test_a_ledger_entry_without_a_kind_is_refused():
    """A key with no kind would exempt the symbol from EVERY ban, silently
    widening what was reviewed — the defect check_python_test_flakiness.py had
    and fixed."""
    with pytest.raises(KeyError):
        c.ledger_key({"file": "a.go", "symbol": "h"})


def test_the_kind_scopes_the_exemption(tree):
    """An accepted `unbounded-body-read` must NOT exempt a `recompiled-regexp`
    added to the same symbol later."""
    tree({"internal/api/h.go": """package api

func handle() {
\tpat := regexp.MustCompile(`^a+$`)
\t_ = pat
}
"""}, ledger={"accepted": [{
        "file": "internal/api/h.go", "symbol": "handle",
        "kind": "unbounded-body-read", "reason": "a different shape"}]})
    # Fails twice over: the regexp is unrecorded AND the entry is now stale.
    assert c.main(["check_perf_regressions.py", "--strict"]) == 1


def test_the_kind_scopes_the_exemption_across_the_two_listeners(tree):
    """An accepted finding on one listener must not exempt the other. Both new
    kinds are three-part scoped like the four before them, so a ledger entry for
    the TDS ceiling cannot quietly cover an HTTP serve site that bypasses the
    root handler."""
    tree({
        c.TDS_PACKET_FILE: """package tds

func ReadMessage(r io.Reader) (typ byte, data []byte, err error) {
\tdata = append(data, p.Data...)
\treturn typ, data, nil
}
""",
        c.SERVE_ROOT_ANCHOR: """package main

func run() error {
\treturn http.Serve(ln, srv.mux)
}
""",
    }, ledger={"accepted": [{
        "file": c.TDS_PACKET_FILE, "symbol": c.TDS_CEILING,
        "kind": "unbounded-frame-accumulation",
        "reason": "accepted for the test"}]})
    found = c.scan()
    assert sorted(set(kinds(found))) == ["unbounded-frame-accumulation",
                                         "unbounded-serve-root"]
    # The frame finding is excused; the serve one is not, so strict still fails.
    assert c.main(["check_perf_regressions.py", "--strict"]) == 1


# --------------------------------------------------------------------------
# non-vacuity: the guards that stop this passing for the wrong reason
# --------------------------------------------------------------------------

def test_a_missing_scan_root_fails_rather_than_reporting_success(tree, tmp_path,
                                                                monkeypatch):
    """A renamed root would leave the sweep walking less than it believes, and
    report success over a surface it never read."""
    tree({})
    monkeypatch.setattr(c, "SCAN_ROOTS", ("internal", "does-not-exist"))
    assert c.missing_scan_roots() == ["does-not-exist"]
    assert c.main(["check_perf_regressions.py", "--strict"]) == 1


def test_walking_zero_files_fails_rather_than_reporting_success(tmp_path,
                                                               monkeypatch):
    """A check that inspects nothing passes quietly. Same guard every sibling
    carries."""
    root = tmp_path / "empty"
    for r in c.SCAN_ROOTS:
        (root / r).mkdir(parents=True)
    led = root / "docs" / "perf-regressions.json"
    led.parent.mkdir(parents=True)
    led.write_text('{"accepted": []}', encoding="utf-8")
    monkeypatch.setattr(c, "ROOT", root)
    monkeypatch.setattr(c, "LEDGER", led)
    assert c.go_files() == []
    assert c.main(["check_perf_regressions.py", "--strict"]) == 1


def test_relkey_is_posix_on_every_platform():
    """A ledger keyed on the native separator reads every real site as
    unrecorded and every real entry as stale on Windows alone."""
    assert c.relkey(pathlib.PurePath("internal") / "api" / "h.go",
                    root=pathlib.PurePath(".")) == "internal/api/h.go"


def test_the_real_tree_is_clean():
    """The checker, against this repository, with its real ledger.

    Deliberately last and deliberately weak: it proves the sweep runs on real
    source without crashing and that the tree it ships with holds. Every
    assertion above is what proves the checker would SPEAK UP, which this one
    cannot.
    """
    assert c.main(["check_perf_regressions.py", "--strict"]) == 0
