"""The ADF activity-type checker, driven by pytest as well as by `make check`.

Same convention as its Fabric sibling: a checker that guards a list is worth
nothing if it can itself drift, so the failure directions are asserted rather
than reviewed.

The property worth the most here is the DERIVED exclusion of abstract bases.
`Container` and `Execution` are discriminators on base classes nothing authors;
excluding them by a hand-written exemption would reintroduce, inside the fix,
exactly the declared list the fix removes.
"""
import hashlib
import json
import pathlib
import sys

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))

import check_adf_activity_types as c  # noqa: E402

SCHEMA = c.SCHEMA.read_bytes()
PROV = c.PROVENANCE.read_text(encoding="utf-8")
REFUSALS = c.REFUSALS.read_text(encoding="utf-8")


@pytest.fixture
def sandbox(tmp_path, monkeypatch):
    files = {}
    for attr, src in (("SCHEMA", SCHEMA), ("PROVENANCE", PROV.encode()),
                      ("DISPATCH", c.DISPATCH.read_bytes()),
                      ("INTERP", c.INTERP.read_bytes()),
                      ("REFUSALS", REFUSALS.encode())):
        p = tmp_path / attr.lower()
        p.write_bytes(src)
        monkeypatch.setattr(c, attr, p)
        files[attr] = p
    monkeypatch.setattr(c, "ROOT", tmp_path)
    return files


def repin(files, payload: bytes):
    files["SCHEMA"].write_bytes(payload)
    digest = hashlib.sha256(payload).hexdigest()
    files["PROVENANCE"].write_text(
        c.SHA.sub(f"`sha256:{digest}`", files["PROVENANCE"].read_text(encoding="utf-8")),
        encoding="utf-8")


def test_the_repo_as_it_stands_passes():
    assert c.main() == 0


def test_abstract_bases_are_excluded_by_derivation():
    """Container and Execution carry discriminators but are base classes.
    They must not appear as types to handle — and the exclusion must come from
    the schema's own allOf graph, not from a list in this repo."""
    types = c.concrete_types(json.loads(SCHEMA))
    assert "Container" not in types and "Execution" not in types
    # ...and the concrete ones ARE there, so the derivation is not just
    # excluding everything.
    assert {"Copy", "HDInsightHive", "SparkJob"} <= set(types)


def test_an_unhandled_type_fails(sandbox, capsys):
    """The defect: a type nothing handles reaches the dispatch default and is
    reported Succeeded having run nothing."""
    sandbox["REFUSALS"].write_text(REFUSALS.replace('\t"SparkJob":', '\t"SparkJobX":'),
                                   encoding="utf-8")
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "SparkJob" in out and "Succeeded having run nothing" in out


def test_a_tampered_schema_fails(sandbox, capsys):
    sandbox["SCHEMA"].write_bytes(SCHEMA + b" ")
    assert c.main() == 1
    assert "does not match its own hash" in capsys.readouterr().out


def test_a_schema_it_can_no_longer_read_fails(sandbox, capsys):
    """A parse that silently finds three types would 'pass' while checking
    almost nothing — the guard must notice it has stopped working."""
    repin(sandbox, json.dumps({"definitions": {}}).encode())
    assert c.main() == 1
    assert "reading it wrong" in capsys.readouterr().out


def test_all_three_handling_routes_count(sandbox):
    """Dispatch case, interpreter case, and refusal map are equally valid
    answers — the checker asserts a real outcome exists, not which one."""
    handled = c.handled(c.DISPATCH.read_text(encoding="utf-8"),
                        c.INTERP.read_text(encoding="utf-8"),
                        REFUSALS)
    assert "Copy" in handled            # dispatch
    assert "ForEach" in handled         # interpreter
    assert "HDInsightHive" in handled   # refused by name
