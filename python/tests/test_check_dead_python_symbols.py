"""Tests for the dead-module-level-symbol check.

WHAT IS ACTUALLY AT RISK HERE, and it is the same shape as the defect the
checker exists to catch. This guard fails SILENT: an AST walk that stops
recognising definitions, a scan root that moves, or a counting pass that reads
one file too many all report zero orphans -- which is indistinguishable from a
healthy tree and is exactly what a reader wants to see. So almost every case
below drives a tree it MUST report, and the clean tree is the single control.
A suite that only fed it good input would stay green through the deletion of
the check.

The load-bearing case is `test_the_checkers_own_prose_does_not_vouch_for_a_name`.
That is not a hypothetical: this checker's FIRST RUN over the unrepaired tree
reported 0 orphans and exit 0, because the house style requires it to name its
measured baseline in its own docstring and a mention-anywhere reading counted
that paragraph as a second occurrence of all three symbols. A guard that
exempts whatever it reports is worse than no guard -- it reports success. Its
sibling, `test_the_checkers_own_code_does_vouch_for_a_name`, holds the repair's
own first version, which skipped the file outright and then reported nine of
its internal helpers as orphans.

Synthetic trees throughout, with `c.ROOT` moved to tmp_path, rather than this
repository's own scripts/ and python/: the real tree is edited by other work,
and a test that reads it asserts today's symbol list rather than the checker's
behaviour. The two exceptions are the last tests, which are the control in the
other direction.

SYNTHETIC NAMES ONLY, deliberately. A test module is exempt from being CHECKED
but is still read as a REFERRER, so naming a real candidate here would vouch
for it and quietly exempt it -- this file would become the laundering the
docstring case above is about. It had already happened: the first version of
this file spelled a real finding in one docstring, which is one of the three
sites that left the repaired guard still blind to it. The discipline is now
held mechanically as well -- `prose_blind` covers `test_check_*.py`, so this
file's prose no longer vouches for anything -- but it is stated first, because
a convention that only a mechanism remembers is one nobody writes to.
"""
import pathlib
import re
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_dead_python_symbols as c  # noqa: E402


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """Build a synthetic tree and point the checker at it.

    `files` maps a repo-relative path to its contents, so each test writes
    exactly the arrangement it is about. Defaults to the healthy shape: one
    in-scope module whose only module-level name is read by a sibling.
    """

    def build(files=None):
        files = {
            "scripts/thing.py": "ZSYNTH_USED = 1\n",
            "docs/thing.md": "The pipeline reads ZSYNTH_USED at start-up.\n",
        } if files is None else files
        for rel, body in files.items():
            path = tmp_path / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body, encoding="utf-8")
        monkeypatch.setattr(c, "ROOT", tmp_path)
        return tmp_path

    return build


def _names():
    found, _considered, _unparsed = c.orphans()
    return [f["name"] for f in found]


# --- the control, and the orphan it must find -----------------------------


def test_a_referenced_name_is_not_an_orphan(tree):
    """The control. Without it the reports below prove only that it reports."""
    tree()
    assert _names() == []
    assert c.main(["check_dead_python_symbols.py", "--strict"]) == 0


def test_a_name_nothing_mentions_is_an_orphan(tree):
    """The shape all three real findings had: a constant spelled nowhere else."""
    tree({"scripts/thing.py": "ZSYNTH_USED = 1\nZSYNTH_DEAD = 2\n",
          "docs/thing.md": "reads ZSYNTH_USED\n"})
    assert _names() == ["ZSYNTH_DEAD"]
    assert c.main(["check_dead_python_symbols.py", "--strict"]) == 1


def test_a_dead_function_and_class_are_both_found(tree):
    """def, class and assign are all module-level bindings, not just constants."""
    tree({"python/m.py": "def zsynth_dead_fn():\n    pass\n\n\n"
                         "class ZsynthDeadCls:\n    pass\n"})
    assert sorted(_names()) == ["ZsynthDeadCls", "zsynth_dead_fn"]


def test_the_orphan_is_named_with_its_file_and_line(tree, capsys):
    """A count is not a finding: `1532 -> 1531` in review names nothing.

    check_route_coverage.py records the same lesson one directory over, where
    storing `registered` as a count meant deleting a served route read as a
    number going down.
    """
    tree({"scripts/thing.py": "A_USED = 1\n\nZSYNTH_DEAD = 2\n",
          "docs/d.md": "A_USED\n"})
    assert c.main(["check_dead_python_symbols.py", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "scripts/thing.py:3 ZSYNTH_DEAD" in out
    assert "A_USED" not in out


# --- the generous reading: what counts as a reference ---------------------


@pytest.mark.parametrize("rel,body", [
    # The two that matter most for THIS repository: every invariant script is
    # wired from the Makefile and from ci.yml, so a Python-only sweep would
    # report all of them as orphans.
    ("Makefile", "check:\n\t@$(PY) -c 'import m; m.ZSYNTH_USED'\n"),
    (".github/workflows/ci.yml", "jobs:\n  a:\n    steps:\n      - run: ZSYNTH_USED\n"),
    ("docs/note.md", "The ZSYNTH_USED constant decides the ceiling.\n"),
    ("docs/ledger.json", '{"accepted": [{"symbol": "ZSYNTH_USED"}]}\n'),
    ("scripts/other.py", '"""A docstring naming ZSYNTH_USED."""\n'),
    ("scripts/other.py", 'MSG = "see ZSYNTH_USED"\n'),
])
def test_a_mention_in_any_text_file_counts_as_a_reference(tree, rel, body):
    """Mention-anywhere, generously, so a finding is never arguable.

    A stricter reading -- imports resolved, call graph walked -- would produce
    findings someone has to litigate, and check_perf_regressions.py records at
    length what happens to a guard whose output gets argued with
    (`defer-in-loop`, one false positive, dropped before it shipped).
    """
    tree({"python/m.py": "ZSYNTH_USED = 1\n", rel: body})
    # Not `== []`: where the referrer is itself an in-scope .py it brings its
    # own module-level name along, and that name being an orphan is correct.
    # What is under test is that the MENTION counted.
    assert "ZSYNTH_USED" not in _names()


def test_a_name_in_a_lock_file_is_not_a_reference(tree):
    """A machine-written lock file is not a caller.

    These are enormous and full of package names and hash substrings; letting
    one vouch for a symbol would make the guard's answer depend on a
    dependency's spelling.
    """
    tree({"python/m.py": "ZSYNTH_DEAD = 1\n", "uv.lock": "name = 'ZSYNTH_DEAD'\n"})
    assert _names() == ["ZSYNTH_DEAD"]


def test_a_name_in_the_committed_portal_bundle_is_not_a_reference(tree):
    """portal/dist is a build artifact committed for go:embed.

    A minified bundle happening to contain a short identifier is not somebody
    using it -- the same derived-artifact-is-not-a-caller reading `build/`
    gets below.
    """
    tree({"python/m.py": "ZSYNTH_DEAD = 1\n",
          "portal/dist/assets/index-x.js": "var ZSYNTH_DEAD=1;\n"})
    assert _names() == ["ZSYNTH_DEAD"]


# --- the exemptions -------------------------------------------------------


def test_a_pytest_module_is_not_checked(tree):
    """pytest DISCOVERS by name and injects fixtures by name.

    Measured on this repository: 2190 would-be findings without this
    exemption, which makes it by far the load-bearing one.
    """
    tree({"python/tests/test_x.py":
          "def test_zsynth_nothing_calls_me():\n    assert True\n"})
    assert _names() == []


def test_a_conftest_is_not_checked(tree):
    tree({"python/tests/conftest.py": "ZSYNTH_DEAD = 1\n"})
    assert _names() == []


def test_an_init_barrel_is_not_checked(tree):
    """A barrel publishes a surface whether or not today's callers use it all.

    The same directory-vs-file reasoning check_portal_components.py records
    for `card-action.svelte`: flagging a re-export produces findings nobody
    should act on.
    """
    tree({"python/pkg/__init__.py": "ZSYNTH_REEXPORTED = 1\n"})
    assert _names() == []


def test_a_decorated_top_level_def_is_not_checked(tree):
    """Reached through its decorator, not by its name."""
    tree({"python/m.py": "import contextlib\n\n\n"
                         "@contextlib.contextmanager\n"
                         "def zsynth_scope():\n    yield\n"})
    assert _names() == []


def test_an_undecorated_sibling_is_still_checked(tree):
    """The decorator exemption must not spill onto the next definition.

    Without the `continue` being scoped to the decorated node, one decorated
    def would exempt the rest of the module -- a far wider hole than the
    exemption asks for, and invisible because it only ever reports less.
    """
    tree({"python/m.py": "import contextlib\n\n\n"
                         "@contextlib.contextmanager\n"
                         "def zsynth_scope():\n    yield\n\n\n"
                         "ZSYNTH_DEAD = 1\n"})
    assert _names() == ["ZSYNTH_DEAD"]


def test_a_name_in_all_is_not_checked(tree):
    """The module has declared the name public, for callers this tree may lack."""
    tree({"python/m.py": '__all__ = ["ZSYNTH_PUBLIC"]\n\nZSYNTH_PUBLIC = 1\n'})
    assert _names() == []


def test_a_name_outside_all_is_still_checked(tree):
    """`__all__` exempts what it lists, not the module around it."""
    tree({"python/m.py":
          '__all__ = ["ZSYNTH_PUBLIC"]\n\nZSYNTH_PUBLIC = 1\nZSYNTH_DEAD = 2\n'})
    assert _names() == ["ZSYNTH_DEAD"]


def test_a_dunder_and_main_are_not_checked(tree):
    """Called by protocol rather than by reference."""
    tree({"python/m.py": "def __getattr__(name):\n    raise AttributeError(name)\n\n\n"
                         "def main(argv):\n    return 0\n"})
    assert _names() == []


def test_a_committed_build_copy_neither_checks_nor_vouches(tree):
    """THE EXEMPTION THAT WOULD HAVE DISABLED THIS OVER A WHOLE PACKAGE.

    `python/build/` is gitignored here, but
    `python/fabric-target/build/lib/fabric_target/` is TRACKED -- a committed
    copy of the source, predating the ignore rule. Left in the counting pass,
    every symbol in that package would appear twice (source, plus its own
    build copy), so nothing in it could ever be reported; left in the scope,
    the copy's own definitions would be checked as if they were source.

    Both directions are asserted, because they fail differently and only one
    of them is visible: the vouching direction reports LESS, silently.
    """
    tree({"python/fabric_target/m.py": "ZSYNTH_DEAD = 1\n",
          "python/fabric-target/build/lib/fabric_target/m.py": "ZSYNTH_DEAD = 1\n"})
    # Not vouched for by its own build copy...
    assert _names() == ["ZSYNTH_DEAD"]
    # ...and the copy is not itself in scope, so the finding is reported once,
    # against the source.
    found, _considered, _unparsed = c.orphans()
    assert [f["file"] for f in found] == ["python/fabric_target/m.py"]


def test_an_out_of_scope_root_is_not_checked(tree):
    """e2e/ and examples/ hold deliberately-retained probes.

    `e2e/xmla/run.py`'s `_shapes_screen5/6/7` are referenced by nothing and
    each says "Kept for the record" in its own docstring -- an archived probe,
    not an oversight. Deleting them would destroy evidence and recording three
    exemptions would be a ledger apologising for a directory this check should
    not read. So the scope stops at scripts/ and python/.
    """
    tree({"scripts/thing.py": "ZSYNTH_USED = 1\n",
          "docs/d.md": "ZSYNTH_USED\n",
          "e2e/xmla/run.py": "def _shapes_screen5(host):\n    return host\n"})
    assert _names() == []


# --- the checker's own prose, which is the defect it shipped with ---------


def _self_tree(tmp_path, monkeypatch, docstring_names, code):
    """A synthetic copy of the checker at its own real path, plus a victim."""
    path = tmp_path / c.SELF
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(f'"""Guard.\n\n{docstring_names}\n"""\n{code}', encoding="utf-8")
    victim = tmp_path / "python" / "m.py"
    victim.parent.mkdir(parents=True, exist_ok=True)
    victim.write_text("ZSYNTH_DEAD = 1\n", encoding="utf-8")
    monkeypatch.setattr(c, "ROOT", tmp_path)


def test_the_checkers_own_prose_does_not_vouch_for_a_name(tmp_path, monkeypatch):
    """THE DEFECT THIS CHECKER SHIPPED WITH, and the reason SELF exists.

    The house style here requires a guard to record its measured baseline, so
    this one's docstring spells the three symbols it found. Under a
    mention-anywhere reading that paragraph IS a second occurrence of each, and
    the first run over the unrepaired tree reported 0 orphans at exit 0 --
    having laundered all three of the symbols it was written to find, and every
    future finding with them.
    """
    _self_tree(tmp_path, monkeypatch,
               docstring_names="Measured: ZSYNTH_DEAD was reached by nothing.",
               code="HELPER_USED = 1\nprint(HELPER_USED)\n")
    found, _considered, _unparsed = c.orphans()
    assert [f["name"] for f in found] == ["ZSYNTH_DEAD"]


def test_the_checkers_own_code_does_vouch_for_a_name(tmp_path, monkeypatch):
    """THE REPAIR'S OWN FIRST VERSION, which skipped the file outright.

    That dropped its internal calls from the counting pass too, so the nine
    helpers it invokes from inside itself -- `text_files`, `module_symbols`,
    `identifier_counts` and the rest -- every one reported as an orphan. The
    prose is what must not vouch for a name; the code still must.
    """
    _self_tree(tmp_path, monkeypatch,
               docstring_names="Nothing named here.",
               code="def helper_called():\n    return 1\n\n\n"
                    "RESULT = helper_called()\n")
    found, _considered, _unparsed = c.orphans()
    assert "helper_called" not in [f["name"] for f in found]
    assert "RESULT" in [f["name"] for f in found]


def test_name_tokens_exclude_comments_and_strings():
    """`tokenize` classifies prose exactly, where a regex would guess.

    Driven directly because the SELF path depends on it, and a tokenizer that
    silently started returning string contents would reopen the laundering
    above while every other test here stayed green.
    """
    src = ('"""Doc naming ZSYNTH_PROSE."""\n'
           "# comment naming ZSYNTH_PROSE\n"
           'MSG = "ZSYNTH_PROSE"\n'
           "ZSYNTH_CODE = 1\n")
    ids = c.code_identifiers(src)
    assert "ZSYNTH_PROSE" not in ids
    assert "ZSYNTH_CODE" in ids and "MSG" in ids


def test_an_untokenizable_source_falls_back_to_the_generous_reading():
    """A tokenizer failure must not INVENT findings.

    The fallback reports more references, never fewer, so a file this cannot
    tokenize costs coverage rather than producing a false orphan.
    """
    assert "ZSYNTH_X" in c.code_identifiers("def (:\n  ZSYNTH_X\n")


# --- the sweep that covers nothing ----------------------------------------


def test_no_in_scope_files_is_itself_a_finding(tree, capsys):
    """A moved scan root leaves this reporting zero orphans, which looks healthy."""
    tree({"docs/only.md": "nothing to check\n"})
    assert c.main(["check_dead_python_symbols.py", "--strict"]) == 1
    assert "passes" in capsys.readouterr().out


def test_zero_symbols_considered_is_itself_a_finding(tree, capsys):
    """And the subtler one: files present, definitions no longer recognised.

    An AST walk that stopped matching `Assign`/`FunctionDef` would report zero
    orphans over 107 files, which is the vacuous pass in its most convincing
    form -- the success line would even quote a plausible file count.
    """
    tree({"scripts/thing.py": "import os\n\nprint(os.name)\n"})
    assert c.main(["check_dead_python_symbols.py", "--strict"]) == 1
    assert "zero module-level symbols" in capsys.readouterr().out


def test_an_unparsable_in_scope_file_fails_rather_than_being_skipped(tree, capsys):
    """A file no rule was applied to must not be reported as passing.

    check_security_footguns.py makes the same call for the same reason: a rule
    that inspects nothing passes vacuously.
    """
    tree({"scripts/ok.py": "A_USED = 1\n", "docs/d.md": "A_USED\n",
          "scripts/broken.py": "def (:\n"})
    assert c.main(["check_dead_python_symbols.py", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "could not be parsed" in out and "scripts/broken.py" in out


# --- the non-strict mode, paths, and the real repository ------------------


def test_without_strict_an_orphan_is_reported_but_does_not_fail(tree, capsys):
    """Same contract as every sibling: report at exit 0, refuse at --strict."""
    tree({"scripts/thing.py": "ZSYNTH_DEAD = 1\n"})
    assert c.main(["check_dead_python_symbols.py"]) == 0
    out = capsys.readouterr().out
    assert "ORPHAN" in out and "ZSYNTH_DEAD" in out



# --- the three sites the first repair missed ------------------------------
#
# The checker's first repair excluded its own module and stopped. The house
# style had already copied that same baseline paragraph into the `make check`
# recipe, the CI job comment and this very file -- so re-adding a real finding
# to the tree left the guard at exit 0 while a freshly invented name in the
# same file was reported correctly. Each case below is one of those sites.


@pytest.mark.parametrize("rel,body", [
    # The `make check` recipe's tabbed `@#` block. `@#` is the idiom, and the
    # `#` follows `@` rather than whitespace, so a naive YAML-shaped rule
    # misses exactly the line that matters most here.
    ("Makefile",
     "check:\n\t@# Measured when this landed: ZSYNTH_DEAD reached by nothing.\n"
     "\t@echo done\n"),
    # The CI job's comment above the step.
    (".github/workflows/ci.yml",
     "jobs:\n  witnesses:\n    steps:\n"
     "      # Measured: ZSYNTH_DEAD was reached by nothing.\n"
     "      - run: true\n"),
    # This file's own prose. It is a guard's test, which the style makes
    # restate the finding being driven.
    ("python/tests/test_check_dead_python_symbols.py",
     '"""Drives the ZSYNTH_DEAD case."""\n'),
    # An ordinary Python comment, anywhere.
    ("python/helper.py", "# ZSYNTH_DEAD is the old allow list\nX = 1\nprint(X)\n"),
])
def test_a_comment_does_not_vouch_for_a_name(tree, rel, body):
    tree({"python/m.py": "ZSYNTH_DEAD = 1\n", rel: body})
    assert _names() == ["ZSYNTH_DEAD"]


def test_a_comment_in_the_symbols_own_file_does_not_vouch(tree):
    """The shape ordinary Python style produces, and the cheapest launder.

    `# NAME is the allow list` directly above `NAME = (...)` is a second
    occurrence of the name in the dead symbol's OWN file, so it would exempt
    the symbol single-handedly with no second file involved at all. This tree
    escaped it only because the comment above the real finding described the
    tuple without spelling it.
    """
    tree({"python/m.py": "# ZSYNTH_DEAD names where to get a token.\n"
                         "ZSYNTH_DEAD = 1\n"})
    assert _names() == ["ZSYNTH_DEAD"]


def test_a_makefile_recipe_still_vouches_for_a_name(tree):
    """The control for all of the above: only COMMENTS stopped counting.

    Without this, blanking the whole Makefile would pass every case above
    while breaking the generous reading the module docstring argues for --
    this repository wires its scripts from the Makefile, so a recipe is one of
    the most common places a real reference lives.
    """
    tree({"python/m.py": "ZSYNTH_USED = 1\n",
          "Makefile": "check:\n\t@$(PY) -c 'import m; m.ZSYNTH_USED'\n"})
    assert _names() == []


def test_a_guards_code_still_vouches_though_its_prose_does_not(tree):
    """A sibling guard's CALL is a reference; its docstring is not."""
    tree({"python/m.py": "ZSYNTH_USED = 1\nZSYNTH_DEAD = 2\n",
          "scripts/check_other.py": '"""Mentions ZSYNTH_DEAD in prose."""\n'
                                    "from m import ZSYNTH_USED\n"})
    assert _names() == ["ZSYNTH_DEAD"]


@pytest.mark.parametrize("rel,blind", [
    ("scripts/check_dead_python_symbols.py", True),
    ("scripts/check_portal_components.py", True),
    ("python/tests/test_check_dead_python_symbols.py", True),
    # NOT the whole of scripts/ or of python/tests/. A generator is not a
    # guard, and a test that is not a guard's test has no finding to restate
    # -- while its string literals routinely ARE references
    # (`monkeypatch.setattr(mod, "NAME", ...)`), which NAME-only counting
    # would drop and turn into a false positive.
    ("scripts/gen_sbom.py", False),
    ("python/tests/test_build_landing_data.py", False),
    ("python/spark_agent/usercontext.py", False),
    ("docs/check_anything.md", False),
])
def test_prose_blind_is_the_guard_family_and_stops_there(rel, blind):
    assert c.prose_blind(rel) is blind


@pytest.mark.parametrize("line,name,stripped", [
    ("\t@# Measured: ZSYNTH_A reached by nothing", "ZSYNTH_A", True),
    ("      # Measured: ZSYNTH_B reached by nothing", "ZSYNTH_B", True),
    ("# ZSYNTH_C at line start", "ZSYNTH_C", True),
    # A `#` inside a quoted scalar is data, not a comment.
    ('key: "a # ZSYNTH_D here"', "ZSYNTH_D", False),
    # ...and an unquoted `#` with no space before it is literal in YAML and in
    # shell: a URL fragment, or `${#arr}`.
    ("url: http://example.test/page#ZSYNTH_E", "ZSYNTH_E", False),
    ("n=${#ZSYNTH_F}", "ZSYNTH_F", False),
])
def test_hash_comment_stripping_cuts_comments_and_nothing_else(line, name, stripped):
    assert (name not in c.strip_hash_comments(line)) is stripped


def test_an_ordinary_modules_string_literal_still_vouches(tree):
    """`getattr(m, "NAME")` is a real reach, so strings count outside the
    guard family. Only a prose-blind module drops them."""
    tree({"python/m.py": "ZSYNTH_USED = 1\n",
          "python/caller.py": 'import m\nprint(getattr(m, "ZSYNTH_USED"))\n'})
    assert _names() == []


def test_a_path_inside_the_repo_is_shortened():
    """Forward slashes, so one finding reads the same on all three platforms."""
    assert c.relkey(c.ROOT / "scripts" / "x.py") == "scripts/x.py"


def test_the_real_repository_has_no_dead_module_level_symbols():
    """The one case that reads the repository.

    Everything above drives synthetic trees, which proves the checker works
    and says nothing about what is committed. This is the control in the other
    direction -- and it is NOT skipped when a root is absent, because a skip
    would look identical to a pass on the day somebody moves one.
    """
    for scan_root in c.SCAN_ROOTS:
        assert (c.ROOT / scan_root).is_dir(), f"{scan_root}/ is missing"
    found, considered, unparsed = c.orphans()
    assert unparsed == []
    assert found == [], f"dead module-level symbols: {found}"

    # THE STATED BASELINE, RE-DERIVED RATHER THAN TRUSTED, and the reason this
    # is not `considered > 500`. That loose bound was what let the docstring
    # ship claiming "115 in-scope files carrying 1568 module-level symbols"
    # when the checker measured 107 and 1538 -- no scope configuration
    # reproduced it, so it was simply wrong, and nothing in the suite could
    # say so. In a guard whose whole subject is prose drifting from the code
    # beneath it, its own baseline is the last number that should be taken on
    # trust.
    # `or ""` because `__doc__` is `str | None` -- stripped under `python -OO`,
    # which is also the reading `ty` takes.
    stated = re.search(
        r"(\d+) in-scope files carrying\s+(\d+) module-level symbols",
        c.__doc__ or "")
    assert stated, "the module docstring no longer states a measured baseline"
    assert (len(c.in_scope_files()), considered) == \
        (int(stated.group(1)), int(stated.group(2))), (
            f"the docstring claims {stated.group(1)} files and "
            f"{stated.group(2)} symbols; a run measures "
            f"{len(c.in_scope_files())} and {considered}. Adding a "
            "module-level name to scripts/ or python/ moves the second "
            "number -- re-run `python scripts/check_dead_python_symbols.py` "
            "and update the WHAT WAS MEASURED paragraph.")
