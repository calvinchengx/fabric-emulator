"""Tests for the Axis A derivation: our shim against Microsoft's two sources.

THE FAILURE THIS GUARDS AGAINST IS ITS OWN. A surface checker that stops
matching reports nothing and exits 0, which is indistinguishable from a shim
that agrees with Microsoft. That is the same defect one tier up from the one it
exists to catch, so every test drives it with a divergence it must find, and
the real repository is the control at the end.

The interesting behaviour is the arbitration. There are three descriptions of
this API — Fabric's documentation, Microsoft's stub package, and our shim — and
the first two disagree. The rule is that the documentation wins where it
speaks, and that a disagreement between the two Microsoft sources is DERIVED
rather than declared, so nobody has to remember to write it down.
"""
import importlib.util
import json
import pathlib
import sys
import textwrap

import pytest

REPO = pathlib.Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_notebookutils_surface", REPO / "scripts" / "check_notebookutils_surface.py")
assert spec and spec.loader
c = importlib.util.module_from_spec(spec)
sys.modules["check_notebookutils_surface"] = c
spec.loader.exec_module(c)


STUB_FS = textwrap.dedent("""\
    def ls(dir):
        pass


    def cp(src, dest, recurse=False):
        pass
    """)

OURS_FS = textwrap.dedent("""\
    def ls(path):
        return []


    def cp(src, dest, recurse=False):
        return True
    """)

# What Fabric's own page says, which is `path` where the stub says `dir`.
DOC_FS = {"ls": ["path"], "cp": ["src", "dest", "recurse"]}


@pytest.fixture(autouse=True)
def _restore():
    saved = {name: getattr(c, name) for name in
             ("STUBS", "SHIM", "DOCS", "FABRIC_MODULES", "NOT_FABRIC_MODULES",
              "WAIVED", "PLANNED", "EXTRA_PARAMS")}
    yield
    for name, value in saved.items():
        setattr(c, name, value)


@pytest.fixture
def surface(tmp_path):
    """A stub tree, a shim tree and a documentation reference we control."""

    def build(stub_modules, shim_modules, docs=None, fabric=("fs",), not_fabric=(),
              waived=None, planned=None, extra=None):
        stubs, shim = tmp_path / "stubs", tmp_path / "shim"
        for root, modules in ((stubs, stub_modules), (shim, shim_modules)):
            root.mkdir(exist_ok=True)
            for name, body in modules.items():
                (root / f"{name}.py").write_text(body, encoding="utf-8")
        reference = {"modules": {
            f"notebookutils.{module}": {
                member: ({"params": params} if isinstance(params, list) else params)
                for member, params in members.items()}
            for module, members in (docs if docs is not None else {"fs": DOC_FS}).items()}}
        doc_path = tmp_path / "reference.json"
        doc_path.write_text(json.dumps(reference), encoding="utf-8")
        c.STUBS, c.SHIM, c.DOCS = stubs, shim, doc_path
        c.FABRIC_MODULES = set(fabric)
        c.NOT_FABRIC_MODULES = dict.fromkeys(not_fabric, "declared out of scope")
        c.WAIVED = dict(waived or {})
        c.PLANNED = dict(planned or {})
        c.EXTRA_PARAMS = dict(extra or {})
        return c.problems

    return build


# --- the control -------------------------------------------------------------


def test_a_shim_matching_the_documentation_is_clean_of_failures(surface):
    """And the stub's disagreement on `ls` is reported, not raised."""
    failures, notes = surface({"fs": STUB_FS}, {"fs": OURS_FS})()
    assert failures == []
    assert any("stub(dir) docs(path)" in n for n in notes)


# --- arbitration between the three descriptions ------------------------------


def test_the_documentation_wins_over_the_stub(surface):
    """Following the stub against the docs must FAIL, not pass quietly.

    This is the case that decides whether the check is worth having: the stub
    is Synapse-lineage and the pages are Fabric's own.
    """
    ours = OURS_FS.replace("def ls(path):", "def ls(dir):")
    failures, _ = surface({"fs": STUB_FS}, {"fs": ours})()
    assert any("DOCUMENTED signature" in f and "docs(path) ours(dir)" in f
               for f in failures)


def test_a_disagreement_between_the_two_microsoft_sources_needs_no_declaration(surface):
    """Derived, so it cannot go stale and nobody has to maintain a list."""
    _, notes = surface({"fs": STUB_FS}, {"fs": OURS_FS})()
    assert [n for n in notes if n.startswith("stub")]


def test_a_member_documented_by_fabric_and_absent_from_ours_fails(surface):
    failures, _ = surface({"fs": STUB_FS}, {"fs": "def cp(src, dest, recurse=False):\n    pass\n"})()
    assert any("DOCUMENTED Fabric surface" in f and "fs.ls" in f for f in failures)


def test_a_property_in_the_reference_is_not_compared_as_a_function(surface):
    """`runtime.context` is a dict a notebook reads.

    Comparing it as a callable reported our shim as missing documented surface
    that it implements as a module attribute — a false alarm, and false alarms
    are how a gate gets switched off.
    """
    docs = {"fs": dict(DOC_FS, context={"params": [], "kind": "property"})}
    failures, _ = surface({"fs": STUB_FS}, {"fs": OURS_FS}, docs=docs)()
    assert failures == []


# --- surface only the stub knows about ---------------------------------------


def test_an_undocumented_stub_member_must_be_classified(surface):
    stub = STUB_FS + "\n\ndef help(method_name=None):\n    pass\n"
    failures, _ = surface({"fs": stub}, {"fs": OURS_FS})()
    assert any("fs.help" in f and "WAIVED" in f and "PLANNED" in f for f in failures)


def test_a_waived_member_is_silent(surface):
    stub = STUB_FS + "\n\ndef mountToDriverNode(source):\n    pass\n"
    failures, notes = surface({"fs": stub}, {"fs": OURS_FS},
                              waived={"fs.mountToDriverNode": "Synapse-era"})()
    assert failures == []
    assert not [n for n in notes if n.startswith("gap")]


def test_a_planned_member_is_reported_and_not_fatal(surface):
    stub = STUB_FS + "\n\ndef help(method_name=None):\n    pass\n"
    failures, notes = surface({"fs": stub}, {"fs": OURS_FS},
                              planned={"fs.help": "we have it on no module"})()
    assert failures == []
    assert any(n.startswith("gap") and "fs.help" in n for n in notes)


def test_undocumented_surface_we_do_implement_is_held_to_the_stub(surface):
    """With no page to arbitrate, the stub is the only source there is."""
    stub = STUB_FS + "\n\ndef nbResPath():\n    pass\n"
    ours = OURS_FS + "\n\ndef nbResPath(scope):\n    return ''\n"
    failures, _ = surface({"fs": stub}, {"fs": ours})()
    assert any("only source there is" in f for f in failures)


# --- parameters we add -------------------------------------------------------


def test_an_extra_parameter_must_be_declared(surface):
    """Forgiving is still divergent: a notebook written here can pass an
    argument that does not exist upstream, and fails there rather than here."""
    ours = OURS_FS.replace("def ls(path):", "def ls(path, depth=1):")
    failures, _ = surface({"fs": STUB_FS}, {"fs": ours})()
    assert any("accepts depth" in f for f in failures)


def test_a_declared_extra_parameter_is_reported_and_not_fatal(surface):
    ours = OURS_FS.replace("def ls(path):", "def ls(path, depth=1):")
    failures, notes = surface({"fs": STUB_FS}, {"fs": ours},
                              extra={"fs.ls": "an emulator lever"})()
    assert failures == []
    assert any(n.startswith("extra") and "depth" in n for n in notes)


# --- the two-source scope rule ----------------------------------------------


def test_a_module_in_neither_list_is_refused(surface):
    failures, _ = surface({"fs": STUB_FS, "conf": "def get(key):\n    pass\n"},
                          {"fs": OURS_FS})()
    assert any("neither FABRIC_MODULES nor NOT_FABRIC_MODULES" in f for f in failures)


def test_a_module_declared_out_of_scope_is_not_compared(surface):
    failures, _ = surface({"fs": STUB_FS, "conf": "def get(key):\n    pass\n"},
                          {"fs": OURS_FS}, not_fabric=("conf",))()
    assert failures == []


def test_a_fabric_module_the_pin_does_not_carry_is_reported(surface):
    failures, _ = surface({"fs": STUB_FS}, {"fs": OURS_FS}, fabric=("fs", "udf"))()
    assert any("FABRIC_MODULES lists udf" in f for f in failures)


def test_an_out_of_scope_entry_for_a_vanished_module_is_reported(surface):
    failures, _ = surface({"fs": STUB_FS}, {"fs": OURS_FS}, not_fabric=("gone",))()
    assert any("NOT_FABRIC_MODULES excludes gone" in f for f in failures)


# --- staleness of every declaration -----------------------------------------


@pytest.mark.parametrize("declaration", ["waived", "planned", "extra"])
def test_a_declaration_that_describes_nothing_is_reported(surface, declaration):
    """The failure mode every map in this repo eventually hits: it reads as
    current, and the gap it was hiding survives the fix."""
    failures, _ = surface({"fs": STUB_FS}, {"fs": OURS_FS},
                          **{declaration: {"fs.cp": "a reason from the past"}})()
    assert any("fs.cp" in f and ("no longer" in f) for f in failures)


# --- the pin itself ----------------------------------------------------------


def test_a_missing_pin_is_an_error_not_a_pass(surface, tmp_path):
    surface({"fs": STUB_FS}, {"fs": OURS_FS})
    c.STUBS = tmp_path / "not-vendored"
    with pytest.raises(c.Unreadable, match="vendor_notebookutils_stubs"):
        c.problems()


def test_an_empty_pin_is_an_error_not_a_pass(surface):
    """A directory with no modules compares nothing and would exit 0."""
    surface({}, {"fs": OURS_FS})
    with pytest.raises(c.Unreadable, match="vacuous"):
        c.problems()


def test_a_missing_documentation_reference_is_an_error(surface, tmp_path):
    surface({"fs": STUB_FS}, {"fs": OURS_FS})
    c.DOCS = tmp_path / "gone.json"
    with pytest.raises(c.Unreadable, match="half the check"):
        c.problems()


# --- the real repository -----------------------------------------------------


def test_the_committed_shim_is_fully_declared():
    """Every difference between our shim and Microsoft's is allowed; none may
    be silent."""
    assert c.main(["--strict"]) == 0


def test_the_derivation_covers_the_whole_documented_surface():
    """Axis A was graded against one source. If this drops to a handful of
    members, the derivation has quietly stopped working."""
    stubs = c.stub_modules()
    checked = c.FABRIC_MODULES & set(stubs)
    assert len(checked) >= 8, sorted(checked)
    assert sum(len(stubs[m]) for m in checked) >= 60


def test_the_two_microsoft_sources_are_known_to_disagree():
    """The finding this was built for, asserted so a future pin that silently
    agrees with the docs is noticed rather than assumed."""
    _, notes = c.problems()
    disagreements = [n for n in notes if n.startswith("stub")]
    assert len(disagreements) >= 5, notes


def test_the_vendored_pin_is_microsofts_and_carries_its_licence():
    """A golden reference without its licence is one third_party/README
    forbids, and one nobody can legally keep."""
    vendored = REPO / "third_party" / "notebookutils-stubs"
    licence = (vendored / "LICENSE").read_text(encoding="utf-8")
    assert "MIT" in licence
    assert "Microsoft Corporation" in licence
    provenance = (vendored / "PROVENANCE.md").read_text(encoding="utf-8")
    assert "dummy-notebookutils" in provenance
    assert "sha256" in provenance


# --- the command line, which is what CI actually runs ------------------------


def test_a_documented_module_our_shim_lacks_entirely_is_a_failure(surface):
    failures, _ = surface({"fs": STUB_FS}, {})()
    assert any("our shim has no such file" in f for f in failures)


def test_strict_exits_non_zero_and_says_why(surface, capsys):
    """The path CI takes. A checker whose failure branch is untested is one
    nobody has watched fail."""
    ours = OURS_FS.replace("def ls(path):", "def ls(dir):")
    surface({"fs": STUB_FS}, {"fs": ours})
    assert c.main(["--strict"]) == 1
    printed = capsys.readouterr().out
    assert "Undeclared:" in printed
    assert "none may be silent" in printed


def test_without_strict_it_reports_and_exits_zero(surface, capsys):
    ours = OURS_FS.replace("def ls(path):", "def ls(dir):")
    surface({"fs": STUB_FS}, {"fs": ours})
    assert c.main([]) == 0
    assert "Undeclared:" in capsys.readouterr().out


def test_an_unreadable_pin_exits_non_zero_from_main(surface, tmp_path, capsys):
    surface({"fs": STUB_FS}, {"fs": OURS_FS})
    c.STUBS = tmp_path / "not-vendored"
    assert c.main(["--strict"]) == 1
    assert "vendor_notebookutils_stubs" in capsys.readouterr().out
