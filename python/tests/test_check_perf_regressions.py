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


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """Write a fake Go tree and point the checker's module constants at it.

    `files` maps repo-relative paths to source text. The two structural kinds
    read fixed paths (internal/httpx/body.go, internal/server/server.go), so a
    healthy pair is written unless the caller overrides them — otherwise every
    test would trip those two kinds and drown the one it is about.
    """
    def build(files, ledger=None):
        root = tmp_path / "repo"
        base = {
            c.CEILINGS_FILE: GOOD_BODY_GO,
            c.ROOT_HANDLER_FILE: GOOD_SERVER_GO,
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
