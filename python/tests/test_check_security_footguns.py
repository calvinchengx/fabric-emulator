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


# --- R1's `ssl` dialect ----------------------------------------------------
# The dialect R1 could not see for its first two releases. `verify=False` is
# requests' spelling; the stdlib's is `verify_mode`/`check_hostname`, and it is
# the one this repository's own Python actually reaches for -- 39 sites were
# invisible to the rule that claimed to ledger every certificate-verification
# skip in this tree, six of them carrying a client secret or a bearer token.

@pytest.mark.parametrize("body", [
    "import ssl\nctx = ssl.create_default_context()\nctx.verify_mode = ssl.CERT_NONE\n",
    "import ssl\nctx = ssl.create_default_context()\nctx.check_hostname = False\n",
    "import ssl\nCTX = ssl._create_unverified_context()\n",
    "import ssl\nssl._create_default_https_context = ssl._create_unverified_context\n",
])
def test_every_ssl_spelling_of_a_verification_skip_is_reported(tree, body):
    root = tree({"s.py": body})
    assert [m for m in messages(root) if "tls-verification-off" in m], body


@pytest.mark.parametrize("body", [
    # Verification LEFT ON. The overwhelmingly common shape, and reporting it
    # would make the rule about the `ssl` module rather than about trust.
    "import ssl\nctx = ssl.create_default_context()\n",
    "import ssl\nctx = ssl.create_default_context()\nctx.verify_mode = ssl.CERT_REQUIRED\n",
    "import ssl\nctx = ssl.create_default_context()\nctx.check_hostname = True\n",
    # A name that merely mentions the constant.
    "import ssl\nmodes = {'none': ssl.CERT_NONE}\n",
])
def test_verification_left_on_is_not_a_finding(tree, body):
    root = tree({"s.py": body})
    assert not [m for m in messages(root) if "tls-verification-off" in m], body


def test_the_paired_idiom_reports_once_not_twice(tree):
    """`check_hostname = False` beside `verify_mode = ssl.CERT_NONE` is ONE skip.

    The dominant shape in this tree is the two assignments together, and
    reporting both says the same thing twice about one context. The ledger key
    is (file, rule, symbol), so a pair always collapsed to one ENTRY -- this is
    about the report a human reads, and about a count in the prose matching it.
    """
    root = tree({"s.py": "import ssl\n"
                         "def mint():\n"
                         "    ctx = ssl.create_default_context()\n"
                         "    ctx.check_hostname = False\n"
                         "    ctx.verify_mode = ssl.CERT_NONE\n"
                         "    return ctx\n"})
    assert len([m for m in messages(root) if "tls-verification-off" in m]) == 1


def test_check_hostname_alone_is_still_reported(tree):
    """The suppression is about the PAIR, not about check_hostname.

    Accepting a certificate issued for a different host is the half that makes
    a substituted peer work, so on its own it must still fail.
    """
    root = tree({"s.py": "import ssl\n"
                         "def mint():\n"
                         "    ctx = ssl.create_default_context()\n"
                         "    ctx.check_hostname = False\n"
                         "    return ctx\n"})
    assert len([m for m in messages(root) if "tls-verification-off" in m]) == 1


def test_the_pair_suppression_does_not_cross_symbols(tree):
    """Two functions, two skips -- one function's pair cannot hide another's.

    The suppression keys on the ENCLOSING SYMBOL, and a version that keyed on
    the file would report one finding here and silently exempt the second
    function, which is the shape the ledger's (file, rule, symbol) key exists
    to keep separate.
    """
    root = tree({"s.py": "import ssl\n"
                         "def a():\n"
                         "    ctx = ssl.create_default_context()\n"
                         "    ctx.check_hostname = False\n"
                         "    ctx.verify_mode = ssl.CERT_NONE\n"
                         "def b():\n"
                         "    ctx = ssl.create_default_context()\n"
                         "    ctx.check_hostname = False\n"})
    hits = [f for f in c.scan(c.source_files(root))
            if f["rule"] == "tls-verification-off"]
    assert {f["symbol"] for f in hits} == {"a", "b"}


def test_an_ssl_skip_must_also_name_its_hosts(tree):
    """The `ssl` dialect reuses R1's id, so it inherits R1's `hosts` requirement.

    That is the whole reason for not giving it a rule of its own: the host set
    a client can reach is what decides whether a skip is safe, and a second
    rule id would have been a second place for that requirement to be dropped.
    """
    tree({"s.py": "import ssl\nctx = ssl.create_default_context()\n"
                  "ctx.verify_mode = ssl.CERT_NONE\n"},
         ledger={"accepted": [{"file": "s.py", "rule": "tls-verification-off",
                               "symbol": "<module>", "why": "a reason, no hosts"}]})
    with pytest.raises(KeyError, match="hosts"):
        c.load_ledger()


# --- archive extraction ----------------------------------------------------
# SECURITY.md puts "escape from the emulator to the host" in scope and R5/R6
# already serve it. This is the third shape: an archive member's path is chosen
# by whoever built the archive, so extracting one unchecked writes outside the
# destination (zip-slip / tar-slip).

@pytest.mark.parametrize("body", [
    "import tarfile\nwith tarfile.open(p) as t:\n    t.extractall(d)\n",
    "import zipfile\nwith zipfile.ZipFile(p) as z:\n    z.extractall(d)\n",
    "import shutil\nshutil.unpack_archive(p, d)\n",
])
def test_an_unbounded_extraction_is_reported(tree, body):
    root = tree({"a.py": body})
    assert [m for m in messages(root) if "archive-extraction-unchecked" in m], body


@pytest.mark.parametrize("body", [
    # tarfile's own containment filter: refuses absolute and `..` names, and
    # links pointing out of the destination. On this repo's >=3.12 floor it has
    # to be NAMED -- it only became the default in 3.14.
    'import tarfile\nwith tarfile.open(p) as t:\n    t.extractall(d, filter="data")\n',
    # A members= list means the caller built it, so the names went through
    # their own code rather than straight from the archive.
    "import zipfile\nwith zipfile.ZipFile(p) as z:\n"
    "    names = [n for n in z.namelist() if ok(n)]\n"
    "    z.extractall(d, members=names)\n",
])
def test_a_bounded_extraction_is_not_reported(tree, body):
    root = tree({"a.py": body})
    assert not [m for m in messages(root) if "archive-extraction-unchecked" in m], body


@pytest.mark.parametrize("body", [
    # The vendored-activity-types parser: `extract` of a TABLE out of HTML, in
    # a file with no archive module in it at all.
    "got = v.extract(page(rows))\n",
    # scripts/vendor_notebookutils_stubs.py: imports zipfile AND defines its
    # own `extract(url)`, so the import gate alone did not save it. A member
    # extraction is always a method ON an archive object.
    "import zipfile\ndef extract(url):\n    return {}\nfiles = extract(u)\n",
])
def test_an_extract_that_is_not_an_archive_member_is_not_reported(tree, body):
    root = tree({"a.py": body})
    assert not [m for m in messages(root) if "archive-extraction-unchecked" in m], body


def test_a_real_archive_member_extract_is_reported(tree):
    root = tree({"a.py": "import zipfile\n"
                         "with zipfile.ZipFile(p) as z:\n"
                         "    for n in z.namelist():\n"
                         "        z.extract(n, d)\n"})
    assert [m for m in messages(root) if "archive-extraction-unchecked" in m]


def test_the_go_archive_join_shape_is_reported(tree):
    """`filepath.Join(dest, hdr.Name)` with nothing checking where it lands.

    filepath.Join CLEANS its result, which is exactly what makes this read as
    safe and is not: Join("/out", "../../etc/x") is "/etc/x" -- a perfectly
    clean path outside /out.
    """
    root = tree({"x.go": 'package x\n\nimport "archive/tar"\n\n'
                         'func unpack(dest string, tr *tar.Reader) {\n'
                         '\tfor {\n'
                         '\t\thdr, _ := tr.Next()\n'
                         '\t\tp := filepath.Join(dest, hdr.Name)\n'
                         '\t\tos.WriteFile(p, nil, 0o644)\n'
                         '\t}\n}\n'})
    assert [m for m in messages(root) if "archive-extraction-unchecked" in m]


@pytest.mark.parametrize("guard", [
    '\t\tif !strings.HasPrefix(p, dest) {\n\t\t\tcontinue\n\t\t}\n',
    '\t\tif rel, err := filepath.Rel(dest, p); err != nil || rel == ".." {\n'
    '\t\t\tcontinue\n\t\t}\n',
])
def test_a_contained_go_join_is_not_reported(tree, guard):
    root = tree({"x.go": 'package x\n\nimport "archive/tar"\n\n'
                         'func unpack(dest string, tr *tar.Reader) {\n'
                         '\tfor {\n'
                         '\t\thdr, _ := tr.Next()\n'
                         '\t\tp := filepath.Join(dest, hdr.Name)\n'
                         + guard +
                         '\t\tos.WriteFile(p, nil, 0o644)\n'
                         '\t}\n}\n'})
    assert not [m for m in messages(root) if "archive-extraction-unchecked" in m], guard


def test_a_join_in_a_file_with_no_archive_reader_is_not_reported(tree):
    """`filepath.Join(dir, f.Name)` over an os.ReadDir listing is not an escape.

    `f.Name` is also how an fs.DirEntry is spelled, and those names come from
    the filesystem this process is already reading rather than from an archive.
    The import gate is what separates them.
    """
    root = tree({"x.go": 'package x\n\nfunc walk(dir string, fs []entry) {\n'
                         '\tfor _, f := range fs {\n'
                         '\t\tp := filepath.Join(dir, f.Name)\n'
                         '\t\t_ = p\n\t}\n}\n'})
    assert not [m for m in messages(root) if "archive-extraction-unchecked" in m]


def test_the_go_containment_check_must_be_in_the_same_function(tree):
    """A `filepath.Rel` three functions away does not protect this one."""
    root = tree({"x.go": 'package x\n\nimport "archive/tar"\n\n'
                         'func safe(dest, p string) bool {\n'
                         '\trel, _ := filepath.Rel(dest, p)\n'
                         '\treturn rel != ".."\n}\n\n'
                         'func unpack(dest string, tr *tar.Reader) {\n'
                         '\thdr, _ := tr.Next()\n'
                         '\tp := filepath.Join(dest, hdr.Name)\n'
                         '\tos.WriteFile(p, nil, 0o644)\n}\n'})
    assert [m for m in messages(root) if "archive-extraction-unchecked" in m]


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


# --- R8: SQL built by interpolation ----------------------------------------
# The near-misses matter more here than for any rule except R4, because SQL
# text and f-strings are everywhere in a Spark emulator: this tree has 36
# `.sql(f"...")` call sites and only 8 of them were defects. The narrowings
# below are each a measured false positive -- and the ALIAS clause, the one
# widening, is here too, because the eighth defect was the one R8's own method
# requirement could not see.

@pytest.mark.parametrize("body", [
    # The site this rule exists for: OneLake security's own enforcement view.
    'spark.sql(f"CREATE OR REPLACE TEMP VIEW {name} AS {sql}")\n',
    'spark.sql(f"SHOW TABLES IN `{db}`")\n',
    'spark.sql(f"DROP TABLE IF EXISTS `{db}`.`{name}`")\n',
    'cur.execute(f"SELECT {col} FROM {table}")\n',
    # Backticks around it are NOT quoting: a backtick in the name closes them.
    'spark.sql(f"CREATE TABLE IF NOT EXISTS `{schema}`.`{name}` USING delta")\n',
    # The other two build spellings.
    'cur.execute("SELECT * FROM %s" % table)\n',
    'cur.execute("DROP VIEW {}".format(view))\n',
    'cur.execute("SELECT * FROM " + table)\n',
])
def test_sql_assembled_from_a_name_is_reported(tree, body):
    root = tree({"q.py": f"def f(name, sql, db, col, table, schema, view):\n    {body}"})
    assert [m for m in messages(root) if "sql-identifier-interpolated" in m], body


def test_an_escaped_identifier_is_not_a_finding(tree):
    """THE RATCHET, and the reason the rule recognises a named escape at all.

    If an escaped site reported as loudly as a raw one, every repaired site
    would need a ledger entry -- and then DELETING the escape would leave that
    entry valid and the build green. Recognising `quote_ident` is what makes
    the gate able to tell a fix from a defect, so removing one fails.
    """
    root = tree({"q.py": "from sqlident import quote_ident, quote_literal\n"
                         "def f(spark, name, db, loc):\n"
                         "    spark.sql(f'DROP TABLE IF EXISTS "
                         "{quote_ident(db)}.{quote_ident(name)}')\n"
                         "    spark.sql(f'CREATE TABLE {quote_ident(name)} "
                         "LOCATION {quote_literal(loc)}')\n"})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_removing_the_escape_turns_the_site_back_into_a_finding(tree):
    """The same file, one call unwrapped. This is the assertion above's point."""
    root = tree({"q.py": "from sqlident import quote_ident\n"
                         "def f(spark, name, db):\n"
                         "    spark.sql(f'DROP TABLE IF EXISTS "
                         "`{db}`.{quote_ident(name)}')\n"})
    hits = [m for m in messages(root) if "sql-identifier-interpolated" in m]
    assert len(hits) == 1 and "db" in hits[0], hits


@pytest.mark.parametrize("body", [
    # THE DOMINANT e2e SHAPE: a module-level constant. Measured --
    # e2e/sail-session-isolation/probe.py is exactly this, and without the
    # narrowing a dozen harness probes crowd out the sites that decide who
    # sees which rows.
    'VIEW = "iso_probe"\ndef f(spark):\n    spark.sql(f"DROP VIEW IF EXISTS {VIEW}")\n',
    # A literal hole after constant folding.
    'def f(spark):\n    spark.sql(f"DROP VIEW IF EXISTS {\'v\'}")\n',
])
def test_an_interpolation_of_this_files_own_constants_is_not_reported(tree, body):
    root = tree({"p.py": body})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m], body


@pytest.mark.parametrize("body", [
    # NOT SQL. The verb gate is what stops this rule being a rule about
    # f-strings -- `.sql` and `.execute` are not rare method names.
    'def f(c, x):\n    c.execute(f"refresh {x}")\n',
    'def f(c, p):\n    c.sql(f"/items/{p}")\n',
    # A maintenance command over a URI rather than an identifier spliced into
    # a relation: e2e/engine-matrix/probes.py runs these over a local path and
    # including the verbs would add six harness findings saying nothing new.
    'def f(spark, p):\n    spark.sql(f"OPTIMIZE delta.`{p}`")\n',
    'def f(spark, p):\n    spark.sql(f"VACUUM delta.`{p}` RETAIN 168 HOURS")\n',
])
def test_an_fstring_that_is_not_a_ddl_statement_is_not_reported(tree, body):
    root = tree({"p.py": body})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m], body


def test_a_parameterized_query_is_never_reported(tree):
    """The shape this rule wants people to write must not be flagged.

    A fixed literal with the values BOUND by the driver is the correct answer
    to the whole class, so reporting it would be telling people to stop doing
    the right thing. The rule requires the template itself to be BUILT.
    """
    root = tree({"p.py": 'def f(cur, v, t):\n'
                         '    cur.execute("SELECT x FROM t WHERE id = ?", (v,))\n'
                         '    cur.execute("SELECT x FROM t WHERE id = %s", [v])\n'
                         '    cur.executemany("INSERT INTO t VALUES (?)", t)\n'})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_a_bare_call_is_not_read_as_a_sql_method(tree):
    """`execute` is a name anything may have -- R7's measured lesson reused.

    scripts/vendor_notebookutils_stubs.py defines its own `extract(url)`, which
    is why R7's clause requires a METHOD call. The same is true here: a
    module-level `execute()` helper is not a cursor.
    """
    root = tree({"p.py": 'def execute(q):\n    return q\n'
                         'def f(name):\n    return execute(f"DROP VIEW {name}")\n'})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_a_bare_name_this_file_bound_to_spark_sql_is_read_as_a_sql_call(tree):
    """THE THIRD BLIND SPOT, and the one that cost a real finding.

    `delta_ops` binds `original_sql = spark.sql` and passes it as a parameter,
    so the callee at the splice is a bare NAME. The method requirement above is
    measured and right -- `execute` is a name anything may have -- and its
    reasoning simply does not reach a name that IS `spark.sql`. The site it hid
    spliced a catalog-supplied schema location into a quoted literal with
    nothing doubling an apostrophe, in the one package R8's scope sentence
    claims to have swept, while the same value was escaped one module over.
    """
    root = tree({"d.py": "def wrap(spark):\n"
                         "    original_sql = spark.sql\n"
                         "    return original_sql\n"
                         "def ctas(original_sql, name, loc):\n"
                         "    original_sql(f\"CREATE TABLE {name} LOCATION '{loc}'\")\n"})
    hits = [m for m in messages(root) if "sql-identifier-interpolated" in m]
    assert len(hits) == 1 and "loc" in hits[0] and "name" in hits[0], hits


def test_the_alias_must_be_bound_in_this_file_to_a_sql_method(tree):
    """The narrowing that keeps the method requirement's reasoning intact.

    A bare call is still ignored -- see `test_a_bare_call_is_not_read_as_a_sql_method`
    -- unless the file itself says the name is a `.sql`/`.execute` attribute of
    something. Here `run` is bound to an unrelated attribute, so it stays
    invisible: the gate is not "a bare call" but "a bare call to a name this
    file aliased onto a SQL executor".
    """
    root = tree({"d.py": "def f(engine, name):\n"
                         "    run = engine.submit\n"
                         "    run(f\"DROP TABLE {name}\")\n"})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_an_aliased_call_is_escaped_by_the_same_helpers(tree):
    """The ratchet reaches the aliased site too, through `quote_name`.

    `quote_name` is the dotted composition of `quote_ident`, and it exists
    because the site this clause uncovered has an OPTIONAL schema: the
    two-hole spelling `catalog.register` uses cannot be written there without a
    conditional in the hole, and a conditional is not a call -- so the rule
    would read the repaired site as unescaped and the fix would not hold.
    """
    root = tree({"d.py": "from sqlident import quote_literal, quote_name\n"
                         "def wrap(spark):\n"
                         "    original_sql = spark.sql\n"
                         "def ctas(original_sql, schema, tbl, loc):\n"
                         "    original_sql(f\"CREATE TABLE "
                         "{quote_name(schema, tbl)} "
                         "LOCATION {quote_literal(loc)}\")\n"})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_the_interpolated_part_cannot_supply_the_verb(tree):
    """The verb must be text the AUTHOR wrote, not text a hole provides.

    `f"{verb} FROM t"` would otherwise pass the gate on a word nobody typed,
    and a rule whose scope depends on runtime values has no scope.
    """
    root = tree({"p.py": 'def f(c, verb, t):\n    c.execute(f"{verb} FROM {t}")\n'})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_one_report_per_symbol_not_per_statement(tree):
    """A loop building three statements is one context and one ledger entry.

    The key is (file, rule, symbol), so these were always ONE entry; this keeps
    the printed report, and the counts in the prose, matching what a reader
    would count by hand. Same decision as R1's paired `ssl` idiom.
    """
    root = tree({"p.py": 'def sweep(spark, db, name):\n'
                         '    spark.sql(f"DROP VIEW IF EXISTS {name}")\n'
                         '    spark.sql(f"DROP TABLE IF EXISTS {name}")\n'
                         '    spark.sql(f"SHOW TABLES IN `{db}`")\n'})
    assert len([m for m in messages(root)
                if "sql-identifier-interpolated" in m]) == 1


def test_the_symbol_dedup_does_not_cross_functions(tree):
    root = tree({"p.py": 'def a(spark, n):\n'
                         '    spark.sql(f"DROP VIEW IF EXISTS {n}")\n'
                         'def b(spark, n):\n'
                         '    spark.sql(f"DROP TABLE IF EXISTS {n}")\n'})
    hits = [f for f in c.scan(c.source_files(root))
            if f["rule"] == "sql-identifier-interpolated"]
    assert {f["symbol"] for f in hits} == {"a", "b"}


def test_a_test_file_building_the_violation_is_out_of_scope(tree):
    root = tree({"python/tests/test_sql.py":
                 'def test_x(spark, name):\n'
                 '    spark.sql(f"DROP VIEW IF EXISTS {name}")\n'})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


# --- R8's Go half ----------------------------------------------------------

def test_go_sql_assembled_in_a_function_that_executes_it_is_reported(tree):
    root = tree({"x.go": 'package x\n\nimport "database/sql"\n\n'
                         'func sync(ctx context.Context, db *sql.DB, name string) error {\n'
                         '\tvar b strings.Builder\n'
                         '\tfmt.Fprintf(&b, "ALTER ROLE [%s] DROP MEMBER [%s];", name, name)\n'
                         '\t_, err := db.ExecContext(ctx, b.String())\n'
                         '\treturn err\n}\n'})
    assert [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_go_sprintf_with_no_sql_handle_in_the_file_is_not_reported(tree):
    """The import gate, and a MEASURED false positive it removes.

    `.Query(` is also how `net/url`'s is spelled, and internal/api/fabriciq.go's
    `u.Query().Has("pbi_source")` satisfied the "this function executes SQL"
    test on a URL parse. Same gate R7's archive clause carries.
    """
    root = tree({"x.go": 'package x\n\n'
                         'func tool(u *url.URL, id string) string {\n'
                         '\tif u.Query().Has("x") {\n'
                         '\t\treturn fmt.Sprintf("CREATE a report %q", id)\n'
                         '\t}\n\treturn ""\n}\n'})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_go_english_prose_is_not_read_as_sql(tree):
    """The case-sensitivity narrowing, also measured.

    Four of the verbs -- WITH, USE, SET, SHOW -- are ordinary English, and the
    Go clause SEARCHES rather than anchors. With `re.I` this reported
    fabriciq.go's `"No Report or SemanticModel with id %q that you can
    access."` on the word "with". Every SQL statement this tree's Go assembles
    spells its keywords upper case.
    """
    root = tree({"x.go": 'package x\n\nimport "database/sql"\n\n'
                         'func f(ctx context.Context, db *sql.DB, id string) error {\n'
                         '\trow := db.QueryRowContext(ctx, "SELECT 1")\n'
                         '\t_ = row\n'
                         '\treturn fmt.Errorf("no model with id %q, use the list", id)\n}\n'})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_go_sql_built_in_a_function_that_does_not_execute_is_not_reported(tree):
    """Judged on the ENCLOSING FUNCTION, which is what keeps fmt.Sprintf usable.

    The same call builds request URLs and ids all over this tree; only the
    Exec/Query beside it says this one is a statement.
    """
    root = tree({"x.go": 'package x\n\nimport "database/sql"\n\n'
                         'func describe(name string) string {\n'
                         '\treturn fmt.Sprintf("CREATE TABLE %s", name)\n}\n\n'
                         'func run(ctx context.Context, db *sql.DB) error {\n'
                         '\t_, err := db.ExecContext(ctx, "SELECT 1")\n'
                         '\treturn err\n}\n'})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


def test_go_a_writestring_of_a_bare_literal_interpolates_nothing(tree):
    root = tree({"x.go": 'package x\n\nimport "database/sql"\n\n'
                         'func f(ctx context.Context, db *sql.DB) error {\n'
                         '\tvar b strings.Builder\n'
                         '\tb.WriteString("CREATE TABLE t (id int);")\n'
                         '\t_, err := db.ExecContext(ctx, b.String())\n'
                         '\treturn err\n}\n'})
    assert not [m for m in messages(root) if "sql-identifier-interpolated" in m]


# --- R8's ledger field -----------------------------------------------------

def test_a_sql_entry_must_name_what_is_interpolated_and_what_escapes_it(tree):
    """`values` is to R8 what `hosts` is to R1.

    What decides whether spliced SQL is safe is WHAT goes in and WHAT escapes
    it -- and for the Go sites this field is the only place the escape is
    written down at all, because that half of the rule reads the enclosing
    function rather than the argument.
    """
    tree({"q.py": 'def f(spark, n):\n    spark.sql(f"DROP VIEW IF EXISTS {n}")\n'},
         ledger={"accepted": [{"file": "q.py",
                               "rule": "sql-identifier-interpolated",
                               "symbol": "f", "why": "a reason, but no values"}]})
    with pytest.raises(KeyError, match="values"):
        c.load_ledger()


def test_a_sql_entry_with_values_is_accepted(tree):
    tree({"q.py": 'def f(spark, n):\n    spark.sql(f"DROP VIEW IF EXISTS {n}")\n'},
         ledger={"accepted": [{"file": "q.py",
                               "rule": "sql-identifier-interpolated",
                               "symbol": "f",
                               "values": "`n`, a module constant in this file",
                               "why": "harness code over its own constants"}]})
    assert c.main(["check", "--strict"]) == 0


def test_a_stale_sql_entry_fails_like_every_other_rule(tree, capsys):
    """The other direction: a site that got FIXED cannot leave an entry behind.

    This is the direction that matters most for R8, because the fix is a
    one-line wrapper -- an entry outliving it would go on excusing the symbol
    and would silently re-cover the shape if someone unwrapped it.
    """
    tree({"q.py": "from sqlident import quote_ident\n"
                  "def f(spark, n):\n"
                  "    spark.sql(f'DROP VIEW IF EXISTS {quote_ident(n)}')\n"},
         ledger={"accepted": [{"file": "q.py",
                               "rule": "sql-identifier-interpolated",
                               "symbol": "f", "values": "`n`, unescaped",
                               "why": "was raw once"}]})
    assert c.main(["check", "--strict"]) == 1
    assert "stale" in capsys.readouterr().out


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
