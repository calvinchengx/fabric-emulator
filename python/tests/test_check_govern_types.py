"""The catalog-type guard must keep refusing the column that costs a table.

OpenMetadata rejects a table outright — 400, the whole table, not a warning on
the column — when a char/varchar/binary/varbinary column carries no
`dataLength`. `scripts/check_govern_types.py` is the cheap guard for that; the
only other witness is a containerized e2e, so if this guard stopped detecting
the shape, a table would vanish from the catalog and the cause would be a SQL
type-map change nobody connected to it.

Two things are worth pinning: that the guard's import shim does not mask a real
dependency, and that the guard actually fails on the column it exists to catch.
"""
import importlib.util
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "check_govern_types", REPO / "scripts" / "check_govern_types.py")
# spec_from_file_location returns Optional; a None here means the path
# is wrong, and failing at import beats an AttributeError mid-test.
assert spec and spec.loader
cg = importlib.util.module_from_spec(spec)
sys.modules["check_govern_types"] = cg
spec.loader.exec_module(cg)


# --- the import shim -------------------------------------------------------

def test_stub_is_installed_only_when_genuinely_absent():
    """`_stub_missing` must not shadow a real module. Stubbing over an
    installed package is how a guard added to catch a governance break becomes
    one, silently testing a stand-in instead of the code that ships."""
    built = []
    cg._stub_missing("json", lambda: built.append(1))
    assert built == [], "json is installed; no stub should have been built"


def test_stub_is_installed_when_the_module_is_missing():
    name = "definitely_not_a_real_module_xyz"
    sys.modules.pop(name, None)
    try:
        cg._stub_missing(name, lambda: __import__("types").ModuleType(name))
        assert name in sys.modules
    finally:
        sys.modules.pop(name, None)


def test_load_ingest_returns_the_real_mapping_module():
    """The whole guard rests on reaching govern_ingest's column mapping without
    the governance dependency group installed."""
    gi = cg.load_ingest()
    assert hasattr(gi, "TYPE_MAP") and hasattr(gi, "om_column")
    assert hasattr(gi, "LENGTH_REQUIRED")


# --- the invariant itself --------------------------------------------------

def test_every_mapped_type_produces_an_acceptable_column():
    """The guard passes on the committed mapping."""
    assert cg.main() == 0


def test_a_length_required_type_without_length_would_be_caught(capsys):
    """The defect the guard exists for, injected: a binary column that reports
    no dataLength must fail, because OpenMetadata refuses the whole table."""
    gi = cg.load_ingest()
    original = gi.om_column

    def stripped(*args, **kwargs):
        col = original(*args, **kwargs)
        if col["dataType"] in gi.LENGTH_REQUIRED:
            col.pop("dataLength", None)
        return col

    gi.om_column = stripped
    real_loader = cg.load_ingest
    try:
        cg.load_ingest = lambda: gi
        assert cg.main() == 1
        out = capsys.readouterr().out
        # The FLAT-column message specifically. Both branches say "no
        # dataLength", so asserting only that let the flat check be deleted
        # while the struct-child branch kept this test green.
        assert "rejects the whole table for this" in out
        assert "'binary' ->" in out.replace('"', "'")
    finally:
        gi.om_column = original
        cg.load_ingest = real_loader


def test_a_decimal_losing_its_scale_would_be_caught(capsys):
    """A decimal without precision/scale defeats the point of a decimal column,
    and the guard says so rather than letting the catalog round it away."""
    gi = cg.load_ingest()
    original = gi.om_column

    def scaleless(*args, **kwargs):
        col = original(*args, **kwargs)
        if col["dataType"] == "DECIMAL":
            col.pop("precision", None)
            col.pop("scale", None)
        return col

    gi.om_column = scaleless
    real_loader = cg.load_ingest
    try:
        cg.load_ingest = lambda: gi
        assert cg.main() == 1
        assert "without precision/scale" in capsys.readouterr().out
    finally:
        gi.om_column = original
        cg.load_ingest = real_loader


def test_a_struct_child_without_length_is_caught(capsys):
    """The nested branch, isolated from the flat one: only the struct child
    loses its length, so nothing but the children check can fail this."""
    gi = cg.load_ingest()
    original = gi.om_column

    def child_only(*args, **kwargs):
        col = original(*args, **kwargs)
        for child in col.get("children", []):
            if child["dataType"] in gi.LENGTH_REQUIRED:
                child.pop("dataLength", None)
        return col

    gi.om_column = child_only
    real_loader = cg.load_ingest
    try:
        cg.load_ingest = lambda: gi
        assert cg.main() == 1
        assert "struct child" in capsys.readouterr().out
    finally:
        gi.om_column = original
        cg.load_ingest = real_loader


def test_nested_struct_children_are_checked(capsys):
    """A nested type still has to produce something legal for its children —
    the branch a flat-column-only guard would miss."""
    gi = cg.load_ingest()
    nested = gi.om_column("s", {"type": "struct", "fields": [
        {"name": "b", "type": "binary", "nullable": True}]})
    children = nested.get("children", [])
    assert children, "a struct must report its children, or the guard checks nothing"
    for child in children:
        if child["dataType"] in gi.LENGTH_REQUIRED:
            assert child.get("dataLength") is not None
