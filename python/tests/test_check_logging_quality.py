"""Tests for the logging-quality checker.

DRIVEN AGAINST EACH VIOLATION IT MUST CATCH, and against the near-misses that
would make it noise. scripts/check_script_test_coverage.py makes this file
mandatory, and its reasoning is the reason this one is worth writing rather than
filing: both defects this tree has recorded inside a checker --
check_doc_drift's 84 false positives, check_python_test_flakiness's kind-blind
match key that exempted 42 of 44 recorded symbols -- were found by DRIVING the
checker against something it had to catch, and neither would have been found by
reading it.

The near-misses here are specific and they all bit during development. A Go
source is not a bag of lines: `log.Fatal` appears inside a COMMENT in
cmd/fabric-emulator/main.go (in a sentence explaining why log.Fatal is a problem
there), and the audit's own opening grep counted it -- reporting 31 call sites
where there are 30. It appears inside STRING literals in this repository's own
checkers. A format string is frequently SPLIT across lines with `+`, so reading
only the first fragment reads half a message. And `log.Println("tds:", line)`
carries its tag with no trailing space, because Println supplies the space
itself -- refusing that would be enforcing the implementation rather than the
output a reader greps for.

The ledger is asserted in BOTH directions, like docs/test-flakiness.json and
docs/script-test-coverage.json before it: an unrecorded finding must fail, and an
entry whose call site has gone away must fail too. A one-directional ledger only
ever grows, and a stale exemption goes on excusing a file it no longer describes.

The last two tests point the checker at THIS repository rather than at a
fixture, which is what pins it against both failure directions at once: a false
negative (the real tree is clean, so a rule that stopped matching would be
invisible) and a false positive (a rule that over-matches would fail `make
check` for everyone).
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_logging_quality as c  # noqa: E402

LEDGER = {
    "subsystems": ["livy", "onelake-dfs", "tds"],
    "exemptions": [],
    "legacyEnvNames": [],
    "redactionExemptions": [],
}


def write_ledger(tmp_path, **overrides):
    """A ledger on disk, loaded through the checker's own validator."""
    data = dict(LEDGER)
    data.update(overrides)
    path = tmp_path / "logging-subsystems.json"
    path.write_text(json.dumps(data), encoding="utf-8")
    return c.load_ledger(path)


@pytest.fixture
def tree(tmp_path):
    """A fake source tree the checker can be pointed at.

    `internal/`, `pkg/` and `cmd/` under a root, so the cmd/ carve-out for
    log.Fatal is exercised by position rather than by a flag.
    """
    def write(rel, body):
        p = tmp_path / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text("package x\n\nimport \"log\"\n\n" + body, encoding="utf-8")
        return p
    return type("Tree", (), {"root": tmp_path, "write": staticmethod(write)})


# --------------------------------------------------------------------------
# R1: every log line names its subsystem.
# --------------------------------------------------------------------------

def test_an_untagged_log_line_is_reported(tree, tmp_path):
    tree.write("internal/a/a.go", 'func f() { log.Printf("something went wrong: %v", err) }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert [(s["file"], s["line"]) for s in found["untagged"]] == [("internal/a/a.go", 5)]
    assert "no subsystem tag" in found["untagged"][0]["why"]


def test_a_tag_absent_from_the_ledger_is_reported(tree, tmp_path):
    """The half that makes the vocabulary a closed set rather than a suggestion.

    Without it, `log.Printf("whatver: ...")` -- a typo, or a thirteenth
    subsystem nobody registered -- satisfies the prefix rule and the ledger goes
    on describing twelve.
    """
    tree.write("internal/a/a.go", 'func f() { log.Printf("notaknownsubsystem: %v", err) }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert len(found["untagged"]) == 1
    assert "not in the ledger" in found["untagged"][0]["why"]


def test_a_known_tag_passes(tree, tmp_path):
    tree.write("internal/a/a.go", 'func f() { log.Printf("livy: session %s died", s) }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert found["untagged"] == []
    assert found["emitted"] == ["livy"]


def test_the_bracketed_spelling_this_audit_replaced_is_reported(tree, tmp_path):
    """`[onelake-dfs]` is why no single grep selected one subsystem's lines."""
    tree.write("internal/a/a.go", 'func f() { log.Printf("[onelake-dfs] %s", r) }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert len(found["untagged"]) == 1


def test_the_space_delimited_spelling_is_reported(tree, tmp_path):
    """`notebook drive job=%s` -- four sites used this and none carried a tag."""
    tree.write("internal/a/a.go", 'func f() { log.Printf("notebook drive job=%s", j) }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert len(found["untagged"]) == 1


def test_println_may_carry_its_tag_with_no_trailing_space(tree, tmp_path):
    """`log.Println("tds:", line)` -- Println supplies the separating space.

    A reader and a grep see the identical `tds: ...` either way, so refusing it
    would be enforcing the call form rather than the output. This is a real site
    (internal/server/server.go), so getting it wrong would have meant either a
    false positive or a pointless exemption.
    """
    tree.write("internal/a/a.go", 'func f() { log.Println("tds:", line) }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert found["untagged"] == []
    assert found["emitted"] == ["tds"]


def test_a_tagged_prefix_needs_the_separator(tree, tmp_path):
    """`livyfoo: ...` must not satisfy the `livy` tag by being a prefix of it.

    A substring match would let one subsystem's grep select another's lines,
    which is the exact property the tag exists to provide.
    """
    tree.write("internal/a/a.go", 'func f() { log.Printf("livyfoo: %v", e) }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert len(found["untagged"]) == 1


def test_a_split_format_string_is_read_whole(tree, tmp_path):
    """Several real sites wrap a long format string with `+`.

    Reading only the first fragment reads half the message -- and in the other
    direction, a tag that lands in the SECOND fragment is not a tag at all,
    because the line does not start with it.
    """
    tree.write("internal/a/a.go",
               'func f() {\n\tlog.Printf("livy: lakehouse %s did "+\n'
               '\t\t"not mount: %v", id, err)\n}\n')
    assert c.scan(root=tree.root, ledger=write_ledger(tmp_path))["untagged"] == []

    tree.write("internal/b/b.go",
               'func g() {\n\tlog.Printf("lakehouse %s "+\n'
               '\t\t"livy: did not mount", id)\n}\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert [s["file"] for s in found["untagged"]] == ["internal/b/b.go"]


def test_a_non_literal_first_argument_is_reported_with_its_own_reason(tree, tmp_path):
    """`log.Fatal(err)` has no format string to prefix.

    Reported, with a reason that says so, rather than silently skipped: this is
    the one site in the real tree that needs an exemption, and an exemption
    nobody is prompted to write is an exemption nobody writes.
    """
    tree.write("cmd/e/main.go", "func main() { log.Fatal(err) }\n")
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert len(found["untagged"]) == 1
    assert "not a string literal" in found["untagged"][0]["why"]


# --------------------------------------------------------------------------
# The Go-aware scan: comments and strings are text, not code.
# --------------------------------------------------------------------------

def test_a_log_call_inside_a_comment_is_not_a_call_site(tree, tmp_path):
    """The real false positive this checker was corrected for.

    cmd/fabric-emulator/main.go contains, in a comment:

        // clean shutdown -- and, because main would then log.Fatal, os.Exit would

    The audit's own opening grep counted it, reporting 31 sites where there are
    30. A checker counting it would report an untagged log.Fatal inside a
    sentence explaining log.Fatal, with no call there to fix.
    """
    tree.write("internal/a/a.go",
               "// because main would then log.Fatal, os.Exit would not run\n"
               "/* log.Printf(\"untagged in a block comment\") */\n"
               'func f() { log.Printf("livy: fine") }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert found["untagged"] == []
    assert len(found["sites"]) == 1


def test_a_log_call_inside_a_string_is_not_a_call_site(tree, tmp_path):
    """This repository's own checkers hold these shapes as string literals."""
    tree.write("internal/a/a.go",
               'var pat = "log.Printf(\\"untagged\\")"\n'
               "var raw = `log.Fatal(\"also untagged\")`\n"
               'func f() { log.Printf("livy: fine") }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert found["untagged"] == []
    assert len(found["sites"]) == 1


def test_test_files_are_not_scanned(tree, tmp_path):
    """A test's log line is scaffolding, not the emulator's account of itself.

    internal/api/livy_catalog_test.go redirects the logger precisely so it can
    read a production line back; holding its helpers to the production
    convention would be holding the wrong file to it.
    """
    tree.write("internal/a/a_test.go", 'func f() { log.Printf("untagged in a test") }\n')
    assert c.scan(root=tree.root, ledger=write_ledger(tmp_path))["sites"] == []


# --------------------------------------------------------------------------
# R2: no library exits the process.
# --------------------------------------------------------------------------

def test_log_fatal_under_internal_is_reported(tree, tmp_path):
    tree.write("internal/a/a.go", 'func f() { log.Fatalf("livy: %v", err) }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert [(s["file"], s["func"]) for s in found["exits"]] == [("internal/a/a.go", "Fatalf")]
    # ...and it is a VALID tag, so R2 is doing the work rather than R1.
    assert found["untagged"] == []


def test_log_panic_under_pkg_is_reported(tree, tmp_path):
    tree.write("pkg/a/a.go", 'func f() { log.Panicln("livy: boom") }\n')
    assert len(c.scan(root=tree.root, ledger=write_ledger(tmp_path))["exits"]) == 1


def test_log_fatal_under_cmd_is_allowed(tree, tmp_path):
    """cmd/ IS the process, so exiting is a decision it gets to make."""
    tree.write("cmd/e/main.go", 'func main() { log.Fatalf("livy: %v", err) }\n')
    assert c.scan(root=tree.root, ledger=write_ledger(tmp_path))["exits"] == []


# --------------------------------------------------------------------------
# R3: a log knob the surface ledger can see.
# --------------------------------------------------------------------------

def test_an_unprefixed_trace_knob_is_reported(tree, tmp_path):
    """The finding this audit started from, reproduced.

    check_backward_compat.py scans for the FABRIC_* literal, so a knob named
    anything else carries no `envVars` row, no `docsUndocumented` reason, and no
    mention in docs/ -- and can be renamed or deleted with every gate green.
    """
    tree.write("internal/a/a.go",
               'func f() {\n\tif os.Getenv("ONELAKE_TRACE") != "" {\n'
               '\t\tlog.Printf("livy: tracing")\n\t}\n}\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert [(e["file"], e["name"]) for e in found["unprefixed_env"]] == \
        [("internal/a/a.go", "ONELAKE_TRACE")]


def test_a_fabric_prefixed_trace_knob_passes(tree, tmp_path):
    tree.write("internal/a/a.go",
               'func f() {\n\tif os.Getenv("FABRIC_ONELAKE_TRACE") != "" {\n'
               '\t\tlog.Printf("livy: tracing")\n\t}\n}\n')
    assert c.scan(root=tree.root, ledger=write_ledger(tmp_path))["unprefixed_env"] == []


def test_a_recorded_legacy_env_name_passes(tree, tmp_path):
    """The compatibility read, excused with its reason."""
    tree.write("internal/a/a.go",
               'func f() {\n\tif os.Getenv("ONELAKE_TRACE") != "" {\n'
               '\t\tlog.Printf("livy: tracing")\n\t}\n}\n')
    ledger = write_ledger(tmp_path, legacyEnvNames=[
        {"name": "ONELAKE_TRACE", "replacedBy": "FABRIC_ONELAKE_TRACE",
         "reason": "the released spelling, still honoured"}])
    found = c.scan(root=tree.root, ledger=ledger)
    assert found["unprefixed_env"] == []
    assert found["stale_legacy"] == []


def test_an_env_knob_that_gates_no_log_output_is_not_a_log_knob(tree, tmp_path):
    """The near-miss that decides whether this rule is noise.

    FABRIC_RECORD_RESPONSES, FABRIC_S3_REGION and every FABRIC_* in
    internal/config gate behaviour rather than diagnostics. A rule that reported
    every env read would be a rule about naming in general, which is
    check_backward_compat.py's job and not this one's -- and an unprefixed
    non-log knob is a different finding with a different fix.
    """
    tree.write("internal/a/a.go", 'func f() { region := os.Getenv("AWS_REGION") }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert found["unprefixed_env"] == []
    assert c.env_log_knobs(root=tree.root) == []


def test_a_knob_is_a_log_knob_by_its_guarded_block_too(tree, tmp_path):
    """A name that says nothing, guarding a block that logs.

    The name rule alone would miss it, and this is the half that makes R3 about
    log output rather than about a word in an identifier.
    """
    tree.write("internal/a/a.go",
               'func f() {\n\tif os.Getenv("ONELAKE_SPEW_EVERYTHING") != "" {\n'
               '\t\tlog.Printf("livy: spewing")\n\t}\n}\n')
    assert [n for _, _, n in c.env_log_knobs(root=tree.root)] == ["ONELAKE_SPEW_EVERYTHING"]


# --------------------------------------------------------------------------
# R5: a log line does not carry a credential.
#
# THE FINDING, reproduced below as the first two tests: the FABRIC_ONELAKE_TRACE
# lines printed `r.URL.RawQuery` and the `x-ms-copy-source` and
# `x-ms-rename-source` headers VERBATIM, and all three carry an Azure SAS in
# ordinary use. The asymmetry was inside one repository -- internal/server/
# record.go refuses to record response headers AT ALL because "a recording
# written to a file that CI may upload as an artifact is exactly the place a
# token should never be", while the sibling diagnostic one package over printed
# the same class of credential to stderr, which compose captures and CI uploads
# by the identical route.
# --------------------------------------------------------------------------

def test_a_log_line_carrying_a_raw_query_is_reported(tree, tmp_path):
    """The DFS trace site as it stood. An Azure SAS IS a query string."""
    tree.write("internal/a/a.go",
               'func f() { log.Printf("livy: %s?%s", r.URL.Path, r.URL.RawQuery) }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert [(s["file"], s["line"]) for s in found["leaky_args"]] == [("internal/a/a.go", 5)]
    assert "credential-bearing source" in found["leaky_args"][0]["why"]
    # ...and R1 is satisfied, so R5 is doing the work rather than the tag rule.
    assert found["untagged"] == []


def test_a_log_line_carrying_a_credential_header_is_reported(tree, tmp_path):
    """The Blob trace site. `x-ms-copy-source` is by convention a full SAS URL,
    and `x-ms-rename-source`'s own handler comment says it "may carry a
    sas/query suffix"."""
    for hdr in ("x-ms-copy-source", "x-ms-rename-source", "Authorization",
                "Cookie", "x-ms-encryption-key"):
        tree.write("internal/a/a.go",
                   f'func f() {{ log.Printf("livy: %q", r.Header.Get("{hdr}")) }}\n')
        found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
        assert len(found["leaky_args"]) == 1, f"{hdr} was not caught"


def test_a_log_line_naming_a_secret_identifier_is_reported(tree, tmp_path):
    """The BY-NAME half of the rule, which is what catches a credential that
    reached the call under a variable rather than straight off a header."""
    for expr in ("token", "t.Secret", "creds.Password", "sasURL", "theBearer",
                 "conn.Credentials", "apiKey", "accessKey", "sharedKey",
                 "theSignature"):
        tree.write("internal/a/a.go",
                   f'func f() {{ log.Printf("livy: %v", {expr}) }}\n')
        found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
        assert len(found["leaky_args"]) == 1, f"{expr} was not caught"
        assert "reads as a credential" in found["leaky_args"][0]["why"]


def test_a_redacted_call_does_not_fire(tree, tmp_path):
    """The fix, as it landed. The redactor is checked FIRST, which is why the
    call passes even though it still names RawQuery -- and why `redactSASURL`
    does not trip the `sas` word rule against itself."""
    tree.write("internal/a/a.go",
               'func f() {\n'
               '\tlog.Printf("livy: %s?%s rename=%q", r.URL.Path,\n'
               '\t\tredactSAS(r.URL.RawQuery),\n'
               '\t\tredactSASURL(r.Header.Get("x-ms-rename-source")))\n}\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert found["leaky_args"] == []


def test_an_innocent_log_line_does_not_fire(tree, tmp_path):
    """The near-miss that decides whether R5 is a rule or noise.

    A substring scan for "sas" would fire on any format string containing those
    three letters, and on `Username` for "name". The word split is what keeps
    the rule about identifiers rather than about letters.
    """
    for expr in ('"livy: lakehouse %s did not mount", id',
                 '"livy: user %s", conn.Username',
                 '"livy: %s sessions, %d assassinations", n, m',
                 '"livy: signal %v", sigChan',
                 '"livy: shutting down on %v", sigint',
                 '"livy: item %s", it.PrimaryKey',
                 '"livy: %s", r.URL.Path'):
        tree.write("internal/a/a.go", f'func f() {{ log.Printf({expr}) }}\n')
        found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
        assert found["leaky_args"] == [], f"false positive on {expr}"


def test_the_real_fault_line_does_not_fire(tree, tmp_path):
    """internal/server/fault.go logs the PATH and never the query, for exactly
    this reason -- and that line is UNCONDITIONAL, unlike the trace, so a
    credential in it would reach every compose log of every run."""
    tree.write("internal/a/a.go",
               'func f() { log.Printf("livy: %s %s -> %d%s", r.Method, r.URL.Path, '
               'fw.status, fw.codeSuffix()) }\n')
    assert c.scan(root=tree.root, ledger=write_ledger(tmp_path))["leaky_args"] == []


def test_a_recorded_redaction_exemption_suppresses_the_finding(tree, tmp_path):
    tree.write("internal/a/a.go",
               'func f() { log.Printf("livy: %s", r.URL.RawQuery) }\n')
    ledger = write_ledger(tmp_path, redactionExemptions=[
        {"file": "internal/a/a.go", "call": 'log.Printf("livy: %s", r.URL.RawQuery)',
         "reason": "this surface has no SAS; the query is a documented filter"}])
    found = c.scan(root=tree.root, ledger=ledger)
    assert found["leaky_args"] == []
    assert found["stale_redaction"] == []


def test_a_stale_redaction_exemption_fails(tree, tmp_path):
    """The direction nobody thinks to check, and here it is the sharper one: a
    redaction exemption whose call site is gone does not merely excuse an
    untagged line, it goes on excusing a CREDENTIAL -- and would silently
    re-excuse it if the call came back."""
    tree.write("internal/a/a.go", 'func f() { log.Printf("livy: fine") }\n')
    ledger = write_ledger(tmp_path, redactionExemptions=[
        {"file": "internal/a/a.go", "call": 'log.Printf("livy: %s", r.URL.RawQuery)',
         "reason": "gone"}])
    found = c.scan(root=tree.root, ledger=ledger)
    assert found["stale_redaction"] == [
        ("internal/a/a.go", 'log.Printf("livy: %s", r.URL.RawQuery)')]


def test_a_redaction_exemption_with_no_reason_is_refused(tmp_path):
    path = tmp_path / "l.json"
    path.write_text(json.dumps({
        "subsystems": ["livy"],
        "redactionExemptions": [{"file": "a.go", "call": 'log.Printf("%s", q)'}],
    }), encoding="utf-8")
    with pytest.raises(KeyError, match="reason"):
        c.load_ledger(path)


def test_a_duplicate_redaction_exemption_is_refused(tmp_path):
    entry = {"file": "a.go", "call": 'log.Printf("%s", q)', "reason": "r"}
    path = tmp_path / "l.json"
    path.write_text(json.dumps({"subsystems": [],
                                "redactionExemptions": [entry, dict(entry, reason="r2")]}),
                    encoding="utf-8")
    with pytest.raises(ValueError, match="redaction-exempt twice"):
        c.load_ledger(path)


def test_r5_is_scanned_even_on_a_tag_exempt_site(tree, tmp_path):
    """`exemptions` excuses the missing TAG and nothing else.

    A site exempt from R1 is still a log line, and a credential in it is still a
    credential. Skipping those sites would have made the one exemption in the
    real ledger a hole in R5 as well as a decision about R1.
    """
    tree.write("cmd/e/main.go", "func main() { log.Fatal(theToken) }\n")
    ledger = write_ledger(tmp_path, exemptions=[
        {"file": "cmd/e/main.go", "call": "log.Fatal(theToken)",
         "reason": "the whole message IS the error"}])
    found = c.scan(root=tree.root, ledger=ledger)
    assert found["untagged"] == []       # R1 excused
    assert len(found["leaky_args"]) == 1  # R5 not


# --------------------------------------------------------------------------
# The ledger, in both directions.
# --------------------------------------------------------------------------

def test_a_recorded_exemption_passes(tree, tmp_path):
    tree.write("cmd/e/main.go", "func main() { log.Fatal(err) }\n")
    ledger = write_ledger(tmp_path, exemptions=[
        {"file": "cmd/e/main.go", "call": "log.Fatal(err)",
         "reason": "the whole message IS the error, and main() is refusing to start"}])
    found = c.scan(root=tree.root, ledger=ledger)
    assert found["untagged"] == []
    assert found["stale_exempt"] == []


def test_a_stale_exemption_fails(tree, tmp_path):
    """A recorded entry whose call site is gone.

    THE DIRECTION A "FLAG WHAT IS NEW" READER WOULD NEVER ASK ABOUT, and the one
    that does the long-term work: an allowance nobody revisits is how a ledger
    stops describing the tree and starts excusing it -- and it would silently
    re-cover the site if the call came back.
    """
    tree.write("cmd/e/main.go", 'func main() { log.Printf("livy: started") }\n')
    ledger = write_ledger(tmp_path, exemptions=[
        {"file": "cmd/e/main.go", "call": "log.Fatal(err)", "reason": "gone"}])
    found = c.scan(root=tree.root, ledger=ledger)
    assert found["stale_exempt"] == [("cmd/e/main.go", "log.Fatal(err)")]


def test_a_ledgered_tag_nothing_emits_fails(tree, tmp_path):
    """A tag nobody emits goes on blessing a spelling.

    It is also how the derived list drifts into fiction: the subsystem it names
    may have been deleted, or may have stopped logging its failure path -- which
    is the finding this whole gate exists for.
    """
    tree.write("internal/a/a.go", 'func f() { log.Printf("livy: fine") }\n')
    found = c.scan(root=tree.root, ledger=write_ledger(tmp_path))
    assert found["stale_tags"] == ["onelake-dfs", "tds"]


def test_a_stale_legacy_env_name_fails(tree, tmp_path):
    tree.write("internal/a/a.go", 'func f() { log.Printf("livy: fine") }\n')
    ledger = write_ledger(tmp_path, legacyEnvNames=[
        {"name": "ONELAKE_TRACE", "reason": "nothing reads this any more"}])
    assert c.scan(root=tree.root, ledger=ledger)["stale_legacy"] == ["ONELAKE_TRACE"]


def test_an_exemption_with_no_reason_is_refused(tmp_path):
    """An exemption with no reason suppresses a finding while explaining nothing
    -- an omission wearing a decision's clothes."""
    path = tmp_path / "l.json"
    path.write_text(json.dumps({
        "subsystems": ["livy"],
        "exemptions": [{"file": "cmd/e/main.go", "call": "log.Fatal(err)"}],
    }), encoding="utf-8")
    with pytest.raises(KeyError, match="reason"):
        c.load_ledger(path)


def test_a_duplicate_exemption_is_refused(tmp_path):
    """Two reasons for one call means one of them is not being read."""
    entry = {"file": "a.go", "call": "log.Fatal(err)", "reason": "r"}
    path = tmp_path / "l.json"
    path.write_text(json.dumps({"subsystems": [], "exemptions": [entry, dict(entry, reason="r2")]}),
                    encoding="utf-8")
    with pytest.raises(ValueError, match="exempt twice"):
        c.load_ledger(path)


def test_a_tag_that_is_not_lower_kebab_is_refused(tmp_path):
    """A tag is lower-kebab: one grep, one subsystem, nothing to shell-quote."""
    path = tmp_path / "l.json"
    path.write_text(json.dumps({"subsystems": ["OneLake DFS"]}), encoding="utf-8")
    with pytest.raises(ValueError, match="not a subsystem tag"):
        c.load_ledger(path)


def test_a_missing_ledger_fails_loudly(tmp_path):
    with pytest.raises(FileNotFoundError):
        c.load_ledger(tmp_path / "nope.json")


# --------------------------------------------------------------------------
# R4: --update rewrites the derived list and nothing else.
# --------------------------------------------------------------------------

def test_update_preserves_the_hand_written_maps(tmp_path, monkeypatch):
    """The refusal that makes `--update` safe to run without reading first.

    `exemptions` and `legacyEnvNames` are decisions somebody wrote down. If
    regenerating erased them, regenerating would be how an exemption gets
    laundered into a clean diff -- the same laundering
    check_backward_compat.py's --update refuses explicitly.
    """
    path = tmp_path / "logging-subsystems.json"
    original = {
        "_comment": ["the measured baseline and why this file exists"],
        "counts": {"subsystems": 1, "exemptions": 1, "legacyEnvNames": 1,
                   "redactionExemptions": 1},
        "subsystems": ["stale-tag-nothing-emits"],
        "exemptions": [{"file": "cmd/e/main.go", "call": "log.Fatal(err)", "reason": "keep me"}],
        "legacyEnvNames": [{"name": "ONELAKE_TRACE", "reason": "keep me too"}],
        # A laundered redaction exemption does not merely excuse an untagged
        # line, it excuses a credential -- so this is the entry whose survival
        # across --update matters most.
        "redactionExemptions": [{"file": "internal/a/a.go",
                                 "call": 'log.Printf("livy: %s", r.URL.RawQuery)',
                                 "reason": "keep me as well"}],
    }
    path.write_text(json.dumps(original), encoding="utf-8")
    monkeypatch.setattr(c, "LEDGER", path)

    c.write_ledger(["livy", "record"], c.load_ledger(path), path)
    after = json.loads(path.read_text(encoding="utf-8"))

    assert after["subsystems"] == ["livy", "record"]
    assert after["exemptions"] == original["exemptions"]
    assert after["legacyEnvNames"] == original["legacyEnvNames"]
    assert after["redactionExemptions"] == original["redactionExemptions"]
    assert after["_comment"] == original["_comment"]
    assert after["counts"] == {"subsystems": 2, "exemptions": 1, "legacyEnvNames": 1,
                               "redactionExemptions": 1}
    # Key order is fixed, so the diff reads as what it is rather than as a
    # reshuffle nobody can review.
    assert list(after) == ["_comment", "counts", "subsystems", "exemptions",
                           "legacyEnvNames", "redactionExemptions"]


def test_update_refuses_to_write_an_empty_list(tmp_path, monkeypatch):
    """A sweep that resolved nothing would write `[]` and pass forever after.

    The vacuity guard every checker in this directory carries: an empty surface
    is never an answer, it is a parser that stopped matching.
    """
    (tmp_path / "internal").mkdir()
    (tmp_path / "internal" / "a.go").write_text("package x\n", encoding="utf-8")
    ledger = tmp_path / "logging-subsystems.json"
    ledger.write_text(json.dumps(LEDGER), encoding="utf-8")
    monkeypatch.setattr(c, "ROOT", tmp_path)
    monkeypatch.setattr(c, "LEDGER", ledger)
    assert c.main(["--update"]) == 1
    assert json.loads(ledger.read_text(encoding="utf-8"))["subsystems"] == LEDGER["subsystems"]


def test_a_tree_with_no_go_sources_fails_rather_than_passing_vacuously(tmp_path, monkeypatch):
    monkeypatch.setattr(c, "ROOT", tmp_path)
    monkeypatch.setattr(c, "LEDGER", tmp_path / "nope.json")
    assert c.main(["--strict"]) == 1


# --------------------------------------------------------------------------
# ...and against this repository's own tree, which pins both directions.
# --------------------------------------------------------------------------

def test_this_repository_passes_strict():
    """A false POSITIVE here fails `make check` for everyone."""
    assert c.main(["--strict"]) == 0


def test_the_scan_still_finds_this_repositorys_log_lines():
    """A false NEGATIVE is invisible: the real tree is clean, so a rule that
    stopped matching would report the same green as a rule that works.

    The floor is deliberately well below the measured 30, so ordinary churn does
    not touch it while a scanner that broke outright still fails.
    """
    sites = c.log_sites()
    assert len(sites) >= 25, f"found only {len(sites)} log sites; the scanner is broken"
    assert {s["file"].split("/")[0] for s in sites} <= set(c.SOURCES)
    # The two surfaces this audit renamed, asserted by tag rather than by count.
    emitted = {s["tag"] for s in sites}
    assert {"onelake-dfs", "onelake-blob", "record", "notebook-drive",
            "spark-job-drive", "arm-capacities", "fault"} <= emitted


def test_the_real_tree_has_no_leaky_log_argument():
    """R5 against this repository, which is the assertion that matters most.

    A FALSE POSITIVE breaks `make check` for everyone -- which is why the
    near-miss tests above are specific rather than illustrative. A FALSE
    NEGATIVE is invisible: the tree is clean, so a rule that stopped matching
    reports the same green as a rule that works. The negative control is the
    next test, which drives the rule against the text as it stood BEFORE the
    redaction landed.
    """
    found = c.scan()
    assert found["leaky_args"] == [], found["leaky_args"]
    assert found["stale_redaction"] == [], found["stale_redaction"]


def test_r5_would_have_caught_the_trace_lines_as_they_stood():
    """THE NEGATIVE CONTROL for the test above.

    These are the two call expressions verbatim as they were before this audit
    redacted them -- internal/onelake/onelake.go and blob.go, under
    FABRIC_ONELAKE_TRACE. Both printed `r.URL.RawQuery`, and between them the
    `x-ms-copy-source` and `x-ms-rename-source` headers, all three of which
    carry an Azure SAS in ordinary use. Without this, "the real tree is clean"
    would be a statement about the rule never firing rather than about the tree.
    """
    pre_fix = [
        'log.Printf("onelake-dfs: %s %s?%s range=%q x-ms-range=%q rename=%q -> %d (%dB)", '
        'r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Range"), '
        'r.Header.Get("x-ms-range"), r.Header.Get("x-ms-rename-source"), tw.status, tw.n)',
        'log.Printf("onelake-blob: %s %s?%s inm=%q copy=%q -> %d (%dB)", '
        'r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("If-None-Match"), '
        'r.Header.Get("x-ms-copy-source"), tw.status, tw.n)',
    ]
    for call in pre_fix:
        assert c.leak_reason(call) is not None, f"R5 no longer catches {call!r}"

    # ...and the lines as they stand now do not fire, which is the fix.
    sites = {s["file"]: s["call"] for s in c.log_sites()
             if s["file"].startswith("internal/onelake/")}
    assert len(sites) == 2, f"the two OneLake trace sites moved: {sites}"
    for f, call in sites.items():
        assert c.leak_reason(call) is None, f"{f} still logs a credential: {call!r}"
        assert "redact" in call.lower(), f"{f} no longer passes through a redactor"


def test_the_fault_log_landed_and_names_its_subsystem():
    """F6's fix, asserted from the ledger side.

    The Go tests pin what the line SAYS; this pins that the tag exists and is
    registered, which is the property that makes the subsystem going quiet a
    failing build rather than a silent absence.
    """
    sites = [s for s in c.log_sites() if s["file"] == "internal/server/fault.go"]
    assert len(sites) == 1, f"expected one log site in fault.go, got {sites}"
    assert sites[0]["tag"] == "fault"
    assert "fault" in c.load_ledger()["subsystems"]
    # The path, never the query -- the line is unconditional, so a SAS in it
    # would reach every compose log of every run.
    assert "RawQuery" not in sites[0]["call"]


def test_the_knob_rename_actually_landed():
    """FABRIC_ONELAKE_TRACE is read, and ONELAKE_TRACE is read beside it.

    Asserted here as well as in Go (internal/onelake/blob_test.go) because this
    is the file that would notice the LEDGER and the source disagreeing: a
    legacy entry whose read had been deleted is a stale exemption, and that is
    the direction nobody thinks to check.
    """
    knobs = {name for _, _, name in c.env_log_knobs()}
    assert "FABRIC_ONELAKE_TRACE" in knobs
    assert "ONELAKE_TRACE" in knobs, (
        "the compatibility read is gone, so docs/logging-subsystems.json's "
        "legacyEnvNames entry is now stale and should be deleted")


# --------------------------------------------------------------------------
# ...and the reporter itself, driven through main().
#
# THE REASON THESE EXIST rather than being written off as printing. The one real
# defect found in this guard during development was in `relkey`, and it was a
# crash in the ERROR REPORTER: the malformed-ledger cases it exists to report
# were precisely the cases that killed it, because a tmp_path is outside the
# repository root and `relative_to` raises. A message-assembly branch that
# nobody drives has the same property -- a stray key in an f-string turns a
# finding into a traceback, and `make check` then fails for a reason that is not
# the one it found.
# --------------------------------------------------------------------------

@pytest.fixture
def violating(tree, tmp_path, monkeypatch):
    """A tree holding one of each violation, with main() pointed at it."""
    tree.write("internal/a/a.go",
               'func f() {\n\tlog.Printf("untagged prose: %v", err)\n'
               '\tlog.Fatalf("livy: exiting from a library")\n'
               '\tlog.Printf("livy: %s", r.URL.RawQuery)\n'
               '\tif os.Getenv("ONELAKE_TRACE") != "" {\n'
               '\t\tlog.Printf("livy: tracing")\n\t}\n}\n')
    ledger = tmp_path / "logging-subsystems.json"
    ledger.write_text(json.dumps({
        "subsystems": ["livy", "a-tag-nothing-emits"],
        "exemptions": [{"file": "gone.go", "call": "log.Fatal(err)", "reason": "stale"}],
        "legacyEnvNames": [{"name": "NOTHING_READS_THIS", "reason": "stale too"}],
        "redactionExemptions": [{"file": "also-gone.go",
                                 "call": 'log.Printf("%s", q)', "reason": "stale as well"}],
    }), encoding="utf-8")
    monkeypatch.setattr(c, "ROOT", tree.root)
    monkeypatch.setattr(c, "LEDGER", ledger)
    return ledger


def test_strict_reports_every_rule_and_exits_non_zero(violating, capsys):
    assert c.main(["--strict"]) == 1
    err = capsys.readouterr().err
    for want in [
        "no subsystem tag",                  # R1, the untagged line
        "internal/a/a.go",                   # ...named by file
        "log.Fatalf",                        # R2, the library exit
        "ONELAKE_TRACE",                     # R3, the unprefixed knob
        "credential",                        # R5, the leaked query string
        "a-tag-nothing-emits",               # stale derived tag
        "gone.go",                           # stale exemption
        "NOTHING_READS_THIS",                # stale legacy name
        "also-gone.go",                      # stale redaction exemption
    ]:
        assert want in err, f"strict output never mentions {want!r}:\n{err}"


def test_report_mode_names_the_same_findings_and_exits_zero(violating, capsys):
    """Report mode is what someone reads while editing, so it must carry BOTH
    directions -- half a contract is not a contract, and a stale allowance is
    the direction a "flag what is new" reader would never think to ask about."""
    assert c.main([]) == 0
    out = capsys.readouterr().out
    for want in ["UNTAGGED", "EXIT", "ENVNAME", "LEAKY", "STALE",
                 "log call site(s)", "redaction exemption"]:
        assert want in out, f"report output never mentions {want!r}:\n{out}"


def test_a_clean_tree_reports_success_through_main(tree, tmp_path, monkeypatch, capsys):
    tree.write("internal/a/a.go", 'func f() { log.Printf("livy: fine") }\n')
    ledger = tmp_path / "logging-subsystems.json"
    ledger.write_text(json.dumps({"subsystems": ["livy"]}), encoding="utf-8")
    monkeypatch.setattr(c, "ROOT", tree.root)
    monkeypatch.setattr(c, "LEDGER", ledger)
    assert c.main(["--strict"]) == 0
    assert "every one naming one of 1 subsystem" in capsys.readouterr().out


def test_a_tree_with_sources_but_no_log_sites_fails(tree, tmp_path, monkeypatch, capsys):
    """The second vacuity guard. Sources present and zero sites found means the
    SCANNER stopped matching, not that the logging went away -- and that
    distinction is invisible unless something refuses to call it clean."""
    tree.write("internal/a/a.go", "func f() { _ = 1 }\n")
    ledger = tmp_path / "logging-subsystems.json"
    ledger.write_text(json.dumps({"subsystems": []}), encoding="utf-8")
    monkeypatch.setattr(c, "ROOT", tree.root)
    monkeypatch.setattr(c, "LEDGER", ledger)
    assert c.main(["--strict"]) == 1
    assert "stopped matching" in capsys.readouterr().err


def test_a_malformed_ledger_is_reported_rather_than_crashing(tree, tmp_path, monkeypatch, capsys):
    """The defect this guard actually shipped once, pinned.

    `relkey` called `Path.relative_to` with no fallback, so the reporter raised
    ValueError on a path outside ROOT -- which is every tmp_path. The cases it
    exists to report were the cases that killed it.
    """
    tree.write("internal/a/a.go", 'func f() { log.Printf("livy: fine") }\n')
    ledger = tmp_path / "logging-subsystems.json"
    ledger.write_text(json.dumps({
        "subsystems": ["livy"],
        "exemptions": [{"file": "a.go", "call": "log.Fatal(err)"}],
    }), encoding="utf-8")
    monkeypatch.setattr(c, "ROOT", tree.root)
    monkeypatch.setattr(c, "LEDGER", ledger)
    assert c.main(["--strict"]) == 1
    assert "reason" in capsys.readouterr().err


def test_update_creates_a_ledger_that_does_not_exist_yet(tree, tmp_path, monkeypatch, capsys):
    tree.write("internal/a/a.go", 'func f() { log.Printf("livy: fine") }\n')
    ledger = tmp_path / "logging-subsystems.json"
    monkeypatch.setattr(c, "ROOT", tree.root)
    monkeypatch.setattr(c, "LEDGER", ledger)
    assert c.main(["--update"]) == 0
    assert json.loads(ledger.read_text(encoding="utf-8"))["subsystems"] == ["livy"]
    assert "ledger rewritten" in capsys.readouterr().out


def test_strict_without_a_ledger_refuses(tree, tmp_path, monkeypatch, capsys):
    tree.write("internal/a/a.go", 'func f() { log.Printf("livy: fine") }\n')
    monkeypatch.setattr(c, "ROOT", tree.root)
    monkeypatch.setattr(c, "LEDGER", tmp_path / "nope.json")
    assert c.main(["--strict"]) == 1
    assert "Create" in capsys.readouterr().err
