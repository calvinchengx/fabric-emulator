"""Tests for the security anti-pattern checker.

DRIVEN AGAINST VIOLATIONS AND AGAINST NEAR-MISSES, in both directions, because
for this checker the near-misses are the whole design problem. The shapes it
looks for are spelled with words that appear all over a Fabric emulator for
innocent reasons -- `fmt.Errorf("unexpected token %q", t.text)` in a parser,
`ErrBadToken` wrapped thirteen times in internal/auth, `creds.CredentialType`
named in a refusal, a seeded `random.Random` building fixture rows, a table
called `secret` inside a SQL string, `subprocess.run(cmd + ["up"])` with no
shell anywhere near it. Every one of those was reported by an early version of
this checker against this tree: 89 findings on the first run against 37 now, and
the difference is entirely near-misses. A guard that cries wolf on a parser gets
skimmed, so each exclusion below is pinned by a test that fails if it is lost.

The other half is the ledger, asserted in both directions like its siblings: an
unrecorded finding must fail, and an entry whose site has gone away must fail
too -- a stale allowance is how a ban quietly stops applying.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_security_footguns as c  # noqa: E402


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """Write a synthetic source tree and point the checker at it."""
    def build(files, ledger=None):
        root = tmp_path / "repo"
        root.mkdir(exist_ok=True)
        for name, body in files.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body, encoding="utf-8")
        docs = root / "docs"
        docs.mkdir(parents=True, exist_ok=True)
        ledger_path = docs / "security-footguns.json"
        if ledger is not None:
            ledger_path.write_text(json.dumps(ledger), encoding="utf-8")
        monkeypatch.setattr(c, "ROOT", root)
        monkeypatch.setattr(c, "LEDGER", ledger_path)
        return root
    return build


def rules_found(root):
    """The (file, rule) pairs the checker reports over a tree."""
    return {(f["file"], f["rule"]) for f in c.scan(c.source_files(root))}


def messages(root):
    return [f"{f['file']}:{f['rule']}:{f['message']}" for f in c.scan(c.source_files(root))]


# --- R1: TLS verification ---------------------------------------------------

def test_an_unledgered_tls_skip_fails_and_a_ledgered_one_passes(tree, capsys):
    src = ("package x\n\nfunc New() {\n"
           "\ttr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}\n}\n")
    root = tree({"internal/thing/thing.go": src}, ledger={"accepted": []})
    assert c.main(["check", "--strict"]) == 1
    assert "tls-verification-off" in capsys.readouterr().out

    tree({"internal/thing/thing.go": src}, ledger={"accepted": [{
        "file": "internal/thing/thing.go", "rule": "tls-verification-off",
        "symbol": "New", "hosts": ["a compose sibling"], "why": "self-signed cert"}]})
    assert c.main(["check", "--strict"]) == 0
    assert root  # the tree was written where the checker looked


def test_a_tls_entry_must_name_the_hosts_that_client_can_reach(tree):
    """`hosts` is the field that makes a TLS entry reviewable.

    It is exactly what was missing when internal/akv's skip was transport-wide
    on the one client whose allowlist admits a real Azure vault: the site was
    plainly visible, and nothing recorded which hosts it applied to.
    """
    tree({"a.go": "package a\nfunc New() {\nInsecureSkipVerify: true\n}\n"},
         ledger={"accepted": [{"file": "a.go", "rule": "tls-verification-off",
                               "symbol": "New", "why": "a reason but no hosts"}]})
    with pytest.raises(KeyError, match="hosts"):
        c.load_ledger()


def test_an_entry_without_a_reason_is_refused(tree):
    tree({"a.go": "package a\nfunc New() {\nInsecureSkipVerify: true\n}\n"},
         ledger={"accepted": [{"file": "a.go", "rule": "tls-verification-off",
                               "symbol": "New", "hosts": ["x"]}]})
    with pytest.raises(KeyError, match="why"):
        c.load_ledger()


def test_python_verify_false_in_both_spellings(tree):
    root = tree({"drive.py": "import requests\n"
                             "requests.get(u, verify=False)\n"
                             "BlobServiceClient(url, connection_verify=False)\n"})
    assert len([m for m in messages(root) if "tls-verification-off" in m]) == 2


# --- the ledger's other direction ------------------------------------------

def test_a_stale_ledger_entry_fails(tree, capsys):
    """A recorded site that has been FIXED must fail, not pass quietly.

    A closed gap left recorded goes on excusing the file, and would silently
    re-cover the shape if it came back -- the direction a "what's new" reader
    would never think to check.
    """
    tree({"internal/thing/thing.go": "package x\n\nfunc New() {\n\t_ = 1\n}\n"},
         ledger={"accepted": [{
             "file": "internal/thing/thing.go", "rule": "tls-verification-off",
             "symbol": "New", "hosts": ["a sibling"], "why": "was needed once"}]})
    assert c.main(["check", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "stale" in out and "internal/thing/thing.go" in out


def test_the_rule_is_part_of_the_ledger_key(tree):
    """An accepted TLS skip must not also exempt a credential leak beside it.

    Without the rule in the key, one reviewed decision would silently widen into
    an exemption for every rule in that function.
    """
    tree({"a.go": 'package a\n\nfunc New() {\n'
                  '\tInsecureSkipVerify: true\n'
                  '\tlog.Printf("auth: %s", bearerToken)\n}\n'},
         ledger={"accepted": [{"file": "a.go", "rule": "tls-verification-off",
                               "symbol": "New", "hosts": ["x"], "why": "y"}]})
    assert c.main(["check", "--strict"]) == 1


# --- R4: the format string is never scanned --------------------------------

@pytest.mark.parametrize("line", [
    # The dominant shape in internal/auth: thirteen of these wrap a SENTINEL.
    'return fmt.Errorf("%w: signature", ErrBadToken)',
    'return fmt.Errorf("%w: audience not accepted", ErrBadToken)',
    # The pipeline and DAX parsers, where "token" is a lexeme.
    'return fmt.Errorf("unexpected token %q", t.text)',
    'return fmt.Errorf("bad token at %d", pos)',
    # A discriminator, not a value -- both of this repo's real hits under the
    # arguments-only reading were this.
    'return fmt.Errorf("credential type %q cannot list", creds.CredentialType)',
    # Quantities derived from a credential.
    'log.Printf("token expires in %ds", get_token_seconds_remaining(t))',
    'log.Printf("%d secrets loaded", tokenCount)',
    # A lookup, not the value.
    'log.Printf("header %s", r.Header.Get("Authorization"))',
])
def test_a_credential_word_in_the_message_is_not_a_leak(tree, line):
    root = tree({"a.go": f"package a\n\nfunc f() {{\n\t{line}\n}}\n"})
    assert not [m for m in messages(root) if "credential-in-message" in m], line


@pytest.mark.parametrize("line", [
    'log.Printf("auth failed for %s", bearerToken)',
    'log.Printf("using %s", cfg.ClientSecret)',
    'return fmt.Errorf("vault refused %s", secret)',
    'log.Printf("connecting with %s", connectionString)',
])
def test_a_credential_value_in_the_arguments_is_a_leak(tree, line):
    root = tree({"a.go": f"package a\n\nfunc f() {{\n\t{line}\n}}\n"})
    assert [m for m in messages(root) if "credential-in-message" in m], line


def test_a_python_fstring_leak_is_caught_but_a_sql_table_named_secret_is_not(tree):
    root = tree({"p.py": 'import logging\n'
                         'logging.info(f"token {bearer_token}")\n'
                         'logging.info(f"rows {scalar(owner, \'SELECT 1 FROM secret\')}")\n'})
    hits = [m for m in messages(root) if "credential-in-message" in m]
    assert len(hits) == 1 and "bearer_token" in hits[0]


# --- R3: constant-time comparison ------------------------------------------

@pytest.mark.parametrize("line", [
    'if token == "" {',                                    # a presence check
    'if secret != nil {',
    'if n != 0 {',
    # internal/purview: TrimPrefix having changed nothing means no Bearer prefix.
    'if token == r.Header.Get("Authorization") {',
])
def test_a_structural_check_is_not_a_secret_comparison(tree, line):
    root = tree({"a.go": f"package a\n\nfunc f() {{\n\t{line}\n\t}}\n}}\n"})
    assert not [m for m in messages(root) if "secret-compared-directly" in m], line


@pytest.mark.parametrize("line", [
    'if mac == expectedMAC {',
    'if signature != wantSignature {',
    'if strings.EqualFold(gotToken, wantToken) {',
    'if sig != computeMAC(body) {',   # a credential-named function on the other side
])
def test_comparing_a_secret_bytewise_is_reported(tree, line):
    root = tree({"a.go": f"package a\n\nfunc f() {{\n\t{line}\n\t}}\n}}\n"})
    assert [m for m in messages(root) if "secret-compared-directly" in m], line


# --- R2: randomness --------------------------------------------------------

def test_seeded_fixture_randomness_is_not_a_finding(tree):
    """examples/contoso-fixtures is the real shape this must not report.

    A seeded generator producing the rows an example asserts against is
    DETERMINISTIC on purpose; `secrets` would break it. The first version of
    this rule asked only whether the file mentioned "key" -- which those files do
    because they describe a primary key -- and reported four of them.
    """
    root = tree({"fixtures.py": 'import random\n'
                                '# columns: id, primary key, api_key label\n'
                                'def _build():\n'
                                '    rnd = random.Random(1234)\n'
                                '    qty = rnd.randint(1, 9)\n'
                                '    return qty\n'})
    assert not [m for m in messages(root) if "weak-random-for-secrets" in m]


@pytest.mark.parametrize("body", [
    'import random\ndef f():\n    token = random.choice(alphabet)\n    return token\n',
    'import random\ndef new_secret():\n    return random.randint(0, 99)\n',
])
def test_randomness_bound_to_a_credential_is_reported(tree, body):
    root = tree({"g.py": body})
    assert [m for m in messages(root) if "weak-random-for-secrets" in m], body


def test_go_math_rand_for_a_credential_is_reported(tree):
    root = tree({"a.go": 'package a\n\nimport "math/rand"\n\n'
                         'func newToken() string {\n'
                         '\treturn fmt.Sprint(rand.Int63())\n}\n'})
    assert [m for m in messages(root) if "weak-random-for-secrets" in m]


# --- R5: shell and dynamic execution ---------------------------------------

def test_argv_list_concatenation_is_not_shell_injection(tree):
    """`subprocess.run(cmd + ["up"])` with no shell cannot be reinterpreted.

    Twenty sites across e2e/ have exactly this shape and the first version of
    this rule reported every one of them.
    """
    root = tree({"h.py": 'import subprocess\n'
                         'cmd = ["docker", "compose"]\n'
                         'subprocess.run(cmd + ["up", "--build"], check=True)\n'
                         'subprocess.run([exe, f"--flag={value}"], check=False)\n'})
    assert not [m for m in messages(root) if "python-shell-injection" in m]


@pytest.mark.parametrize("body", [
    'import subprocess\nsubprocess.run(f"rm {p}", shell=True)\n',
    'import os\nos.system(f"rm {p}")\n',
    'import yaml\nd = yaml.load(text)\n',
    'v = eval(expr)\n',
    'exec(source)\n',
])
def test_shell_and_dynamic_execution_are_reported(tree, body):
    root = tree({"h.py": body})
    assert [m for m in messages(root) if "python-shell-injection" in m], body


@pytest.mark.parametrize("body", [
    'import yaml\nd = yaml.safe_load(text)\n',
    'import yaml\nd = yaml.load(text, Loader=yaml.SafeLoader)\n',
    'v = eval("1 + 1")\n',
])
def test_the_safe_spellings_are_not_reported(tree, body):
    root = tree({"h.py": body})
    assert not [m for m in messages(root) if "python-shell-injection" in m], body


# --- R6: file modes --------------------------------------------------------

def test_world_writable_modes_only(tree):
    root = tree({"m.py": 'import os\n'
                         'os.chmod(a, 0o777)\n'
                         'os.chmod(b, 0o666)\n'
                         'os.chmod(c, 0o755)\n'
                         'os.chmod(d, 0o644)\n'})
    assert len([m for m in messages(root) if "world-writable-mode" in m]) == 2


def test_a_go_world_writable_create_is_reported(tree):
    root = tree({"a.go": 'package a\n\nfunc f() {\n'
                         '\tos.WriteFile(p, b, 0o666)\n'
                         '\tos.MkdirAll(d, 0o755)\n}\n'})
    assert len([m for m in messages(root) if "world-writable-mode" in m]) == 1


# --- scope -----------------------------------------------------------------

def test_test_files_and_vendored_trees_are_out_of_scope(tree):
    """A test's job includes constructing the violation.

    python/tests/test_check_example_portability.py holds the literal string
    `verify=False` as a fixture, and this module holds one of every rule.
    """
    root = tree({
        "internal/x/x_test.go": "package x\nfunc T() { InsecureSkipVerify: true }\n",
        "python/tests/test_thing.py": "requests.get(u, verify=False)\n",
        "e2e/x/test_probe.py": "requests.get(u, verify=False)\n",
        "node_modules/p/i.py": "requests.get(u, verify=False)\n",
        "third_party/s/x.py": "requests.get(u, verify=False)\n",
        "portal/dist/b.py": "requests.get(u, verify=False)\n",
    })
    assert rules_found(root) == set()


def test_e2e_harness_code_is_in_scope(tree):
    """e2e/ is harness code, not test code: its skips are the ones worth recording."""
    root = tree({"e2e/governance/run.py": "requests.get(u, verify=False)\n"})
    assert ("e2e/governance/run.py", "tls-verification-off") in rules_found(root)


# --- non-vacuity ----------------------------------------------------------

def test_the_checker_actually_reads_files(tree):
    """The `test_the_makefile_target_is_parsed_at_all` pattern.

    Every assertion above is about what the checker does NOT report, and a
    checker that read nothing would satisfy all of them. This one fails if the
    walk stops finding files or the rules stop firing.
    """
    root = tree({
        "internal/a/a.go": "package a\nfunc New() { InsecureSkipVerify: true }\n",
        "b.py": "import os\nos.chmod(p, 0o777)\nos.system(f'rm {x}')\n",
        "c.py": "import logging\nlogging.info('x %s', client_secret)\n",
    })
    found = c.scan(c.source_files(root))
    assert len(c.source_files(root)) == 3
    assert len(found) >= 4
    assert {f["rule"] for f in found} >= {
        "tls-verification-off", "world-writable-mode",
        "python-shell-injection", "credential-in-message"}


def test_walking_zero_files_is_a_failure_not_a_pass(tree):
    """A check that inspects nothing passes for the wrong reason, and quietly."""
    tree({}, ledger={"accepted": []})
    assert c.main(["check", "--strict"]) == 1


def test_an_unparseable_python_file_is_reported_rather_than_skipped(tree):
    root = tree({"bad.py": "def (:\n"})
    assert [f for f in c.scan(c.source_files(root)) if f["rule"] == "unparsed"]


# --- this repository's own tree -------------------------------------------

def test_the_real_tree_passes_strict():
    """The checker against THIS repo, not a synthetic one.

    check_example_portability.py's unit test pointed only at synthetic trees, so
    neither it nor CI ever ran the checker over the real `examples/` -- the gap
    ci.yml records in its own comment. This closes that by construction.
    """
    assert c.main(["check", "--strict"]) == 0


def test_every_real_finding_is_inside_a_known_rule():
    found = c.scan()
    assert found, "the real tree reported nothing at all -- the sweep is vacuous"
    assert {f["rule"] for f in found} <= set(c.REMEDY)
