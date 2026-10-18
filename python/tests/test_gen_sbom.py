"""The SBOM generator, held to the standard it holds the tree to.

A parser that reads nothing produces a valid, empty, green SBOM -- so every
test here asserts the parsers FIND things, and one runs them against this
repository's own lockfiles so a format change fails instead of emptying the
inventory.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import gen_sbom as S  # noqa: E402

REPO = pathlib.Path(__file__).resolve().parents[2]


def test_go_block_and_single_line_requires_with_indirect_marker():
    got = S.go_components(
        "module x\n\nrequire a.io/b v1.2.3\n\nrequire (\n"
        "\tc.io/d v0.1.0\n\te.io/f v2.0.0 // indirect\n)\nreplace x => y\n")
    assert [(c["name"], c["version"], c["indirect"]) for c in got] == [
        ("a.io/b", "v1.2.3", False), ("c.io/d", "v0.1.0", False),
        ("e.io/f", "v2.0.0", True)]


def test_uv_keeps_registry_packages_and_drops_the_projects_own():
    got = S.uv_components(
        '[[package]]\nname = "Foo_Bar"\nversion = "1.0"\n'
        'source = { registry = "https://pypi.org/simple" }\n\n'
        '[[package]]\nname = "me"\nversion = "0.1"\nsource = { editable = "." }\n\n'
        '[[package]]\nname = "noversion"\nsource = { registry = "x" }\n')
    assert [c["name"] for c in got] == ["Foo_Bar"]


def test_pnpm_reads_scoped_and_plain_keys_and_stops_at_next_section():
    got = S.pnpm_components(
        "importers:\n  '@no/pe@1':\n\npackages:\n\n  '@a/b@1.0.0':\n    r: x\n\n"
        "  plain@2.0.0:\n    r: x\n\nsnapshots:\n\n  later@3.0.0:\n")
    assert [(c["name"], c["version"]) for c in got] == [
        ("@a/b", "1.0.0"), ("plain", "2.0.0")]


@pytest.mark.parametrize("kind,name,ver,want", [
    ("npm", "@a/b", "1.0.0", "pkg:npm/%40a/b@1.0.0"),
    ("pypi", "Foo_Bar.baz", "1.0", "pkg:pypi/foo-bar-baz@1.0"),
    ("golang", "github.com/x/y", "v1.2.3", "pkg:golang/github.com/x/y@v1.2.3"),
])
def test_purl_spellings(kind, name, ver, want):
    assert S._purl(kind, name, ver) == want


def _tree(tmp_path):
    (tmp_path / "go.mod").write_text("module m\nrequire a.io/b v1.0.0 // indirect\n")
    (tmp_path / "sub").mkdir()
    (tmp_path / "sub" / "go.mod").write_text("module n\nrequire a.io/b v1.0.0\n")
    return ["go.mod", "sub/go.mod"]


def test_build_merges_by_purl_and_direct_wins_over_indirect(tmp_path):
    bom, empty = S.build(tmp_path, _tree(tmp_path))
    assert empty == []
    (c,) = bom["components"]
    assert c["version"] == "1.0.0" and c["purl"] == "pkg:golang/a.io/b@v1.0.0"
    assert {"name": "fabric:scope", "value": "indirect"} not in c["properties"]
    assert [p["value"] for p in c["properties"]] == ["go.mod", "sub/go.mod"]


def test_indirect_only_component_is_marked(tmp_path):
    bom, _ = S.build(tmp_path, ["go.mod"] if _tree(tmp_path) else [])
    assert {"name": "fabric:scope", "value": "indirect"} in \
        bom["components"][0]["properties"]


def test_empty_manifest_is_reported_and_fails_main(tmp_path, capsys, monkeypatch):
    (tmp_path / "go.mod").write_text("module m\n")
    bom, empty = S.build(tmp_path, ["go.mod"])
    assert empty == ["go.mod"] and bom["components"] == []
    monkeypatch.setattr(S, "tracked_manifests", lambda root: ["go.mod"])
    assert S.main([], root=tmp_path) == 1
    assert "parser drift" in capsys.readouterr().err


def test_main_writes_out_file_and_is_deterministic(tmp_path, monkeypatch):
    names = _tree(tmp_path)
    monkeypatch.setattr(S, "tracked_manifests", lambda root: names)
    a, b = tmp_path / "a.json", tmp_path / "b.json"
    assert S.main(["--out", str(a)], root=tmp_path) == 0
    assert S.main(["--out", str(b)], root=tmp_path) == 0
    assert a.read_bytes() == b.read_bytes()
    assert json.loads(a.read_text())["bomFormat"] == "CycloneDX"


def test_main_defaults_to_stdout(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(S, "tracked_manifests", lambda root: _tree(tmp_path))
    assert S.main([], root=tmp_path) == 0
    assert json.loads(capsys.readouterr().out)["specVersion"] == "1.5"


def test_this_repositorys_own_lockfiles_all_yield_components():
    """The non-vacuity witness: a lockfile format change must fail here."""
    manifests = S.tracked_manifests(REPO)
    assert {"go.mod", "uv.lock", "pnpm-lock.yaml"} <= {pathlib.Path(m).name for m in manifests}
    bom, empty = S.build(REPO, manifests)
    assert empty == []
    kinds = {c["purl"].split("/")[0] for c in bom["components"]}
    assert kinds == {"pkg:golang", "pkg:pypi", "pkg:npm"}
