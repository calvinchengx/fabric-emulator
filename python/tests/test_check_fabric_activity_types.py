"""The activity-type checker, driven by pytest as well as by `make check`.

A checker that guards against a list drifting is worth nothing if it can itself
drift — `check_arch_services` shipped with its logic executed by no test at all,
and this repo's convention since is a `check_*.py` in scripts/ with a
`test_check_*.py` here.

The properties below are the ones a reader would otherwise take on trust: that
it FAILS in each of the three directions it claims to cover, and that a passing
run means the Go list and the vendored table are genuinely equal rather than
compared loosely.
"""
import hashlib
import json
import pathlib
import sys

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))

import check_fabric_activity_types as c  # noqa: E402

GO_SRC = c.GO.read_text(encoding="utf-8")
VENDORED = c.ARTIFACT.read_bytes()
PROV = c.PROVENANCE.read_text(encoding="utf-8")


@pytest.fixture
def sandbox(tmp_path, monkeypatch):
    """A throwaway copy of the three real files, so a case can corrupt one."""
    go = tmp_path / "fabricactivitytypes.go"
    art = tmp_path / "activity-types.json"
    prov = tmp_path / "PROVENANCE.md"
    go.write_text(GO_SRC, encoding="utf-8")
    art.write_bytes(VENDORED)
    prov.write_text(PROV, encoding="utf-8")
    monkeypatch.setattr(c, "GO", go)
    monkeypatch.setattr(c, "ARTIFACT", art)
    monkeypatch.setattr(c, "PROVENANCE", prov)
    monkeypatch.setattr(c, "ROOT", tmp_path)
    return go, art, prov


def repin(art: pathlib.Path, prov: pathlib.Path, payload: bytes):
    """Rewrite the artifact AND its hash, so a case can change the table
    without tripping the integrity check it is not testing."""
    art.write_bytes(payload)
    digest = hashlib.sha256(payload).hexdigest()
    prov.write_text(c.SHA.sub(f"`sha256:{digest}`", prov.read_text(encoding="utf-8")),
                    encoding="utf-8")


def test_the_repo_as_it_stands_passes():
    assert c.main() == 0


def test_a_type_missing_from_the_go_list_fails(sandbox, capsys):
    """The defect this exists for: Microsoft adds a type, nobody notices, and
    the dispatch is never checked for it — so it falls to the success stub."""
    _, art, prov = sandbox
    data = json.loads(VENDORED)
    data["types"]["BrandNewActivity"] = "Something Fabric added this month"
    repin(art, prov, (json.dumps(data, indent=2, ensure_ascii=False) + "\n").encode())
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "BrandNewActivity" in out and "success stub" in out


def test_a_type_the_table_does_not_have_fails(sandbox, capsys):
    """The other direction: an invented name, or one Microsoft removed."""
    go, _, _ = sandbox
    go.write_text(GO_SRC.replace('\t"Copy",', '\t"Copy",\n\t"NotAFabricType",'), encoding="utf-8")
    assert c.main() == 1
    assert "NotAFabricType" in capsys.readouterr().out


def test_a_tampered_vendored_file_fails(sandbox, capsys):
    """third_party/README.md requires a sha256 per vendored file; a hash that
    is not checked is decoration."""
    _, art, _ = sandbox
    art.write_bytes(VENDORED + b"\n")  # changed bytes, PROVENANCE untouched
    assert c.main() == 1
    assert "does not match its own hash" in capsys.readouterr().out


def test_a_duplicate_in_the_go_list_fails(sandbox, capsys):
    """Set comparison alone would pass a list with a name twice."""
    go, _, _ = sandbox
    go.write_text(GO_SRC.replace('\t"Copy",', '\t"Copy",\n\t"Copy",'), encoding="utf-8")
    assert c.main() == 1
    assert "duplicated" in capsys.readouterr().out


def test_the_go_list_shape_is_part_of_the_contract(sandbox):
    """The checker parses a []string literal. If someone rewrites it as a map
    or builds it at runtime, this must fail loudly rather than silently find
    nothing and report agreement."""
    go, _, _ = sandbox
    go.write_text(GO_SRC.replace("var fabricActivityTypes = []string{",
                                 "var fabricActivityTypes = buildThem("), encoding="utf-8")
    with pytest.raises(SystemExit):
        c.main()


def test_it_compares_every_name_not_just_the_count(sandbox, capsys):
    """A length check would pass a swap. Rename one on each side."""
    _, art, prov = sandbox
    data = json.loads(VENDORED)
    desc = data["types"].pop("Wait")
    data["types"]["Waiting"] = desc
    repin(art, prov, (json.dumps(data, indent=2, ensure_ascii=False) + "\n").encode())
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "Waiting" in out and "Wait" in out
