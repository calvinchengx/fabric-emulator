"""The refresh script's job is to vendor a COMPLETE set of schema files.

An incomplete set is the failure that matters: the validator would raise
"unresolvable $ref" at test time, which reads like a broken test rather than a
missing file. So the closure walk is tested with a stubbed fetch — no network,
and the interesting shapes (transitive refs, `#`-fragments, cycles, duplicate
paths) can be constructed rather than waited for.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import vendor_openmetadata_schema as v  # noqa: E402


@pytest.fixture
def upstream(monkeypatch):
    """A fake schema repo, keyed by the path under the spec root."""
    files = {}

    def fake_fetch(url):
        # The script builds ".../<sha>/<SPEC>/<rel>"; recover <rel>.
        rel = url.split(v.SPEC + "/", 1)[1] if v.SPEC + "/" in url else url.rsplit("/", 1)[-1]
        if rel not in files:
            raise AssertionError(f"fetched something not in the fake repo: {rel}")
        return json.dumps(files[rel]).encode() if isinstance(files[rel], dict) else files[rel]

    monkeypatch.setattr(v, "fetch", fake_fetch)
    return files


def column(*refs):
    return {"definitions": {"column": {"properties": {
        f"p{i}": {"$ref": r} for i, r in enumerate(refs)}}}}


def test_closure_follows_refs_transitively(upstream):
    upstream[v.ENTRY] = column("../../type/basic.json")
    upstream["type/basic.json"] = {"properties": {"x": {"$ref": "tagLabel.json"}}}
    upstream["type/tagLabel.json"] = {"properties": {}}

    got = v.closure("deadbeef")
    assert set(got) == {v.ENTRY, "type/basic.json", "type/tagLabel.json"}


def test_closure_ignores_refs_outside_the_column_node(upstream):
    # THE WHOLE POINT of validating `column` rather than `table`: table.json
    # references databaseService.json, which drags in every connector config.
    # Those must not be walked.
    entry = column("../../type/basic.json")
    entry["properties"] = {"service": {"$ref": "../services/databaseService.json"}}
    upstream[v.ENTRY] = entry
    upstream["type/basic.json"] = {"properties": {}}

    got = v.closure("deadbeef")
    assert "entity/services/databaseService.json" not in got
    assert set(got) == {v.ENTRY, "type/basic.json"}


def test_closure_strips_fragments_and_dedupes(upstream):
    # `basic.json#/definitions/entityName` and `basic.json#/definitions/date`
    # are one file. Fetching it twice would be harmless; recording it twice in
    # PROVENANCE.md would not be.
    upstream[v.ENTRY] = column(
        "../../type/basic.json#/definitions/entityName",
        "../../type/basic.json#/definitions/date",
    )
    upstream["type/basic.json"] = {"properties": {}}

    got = v.closure("deadbeef")
    assert set(got) == {v.ENTRY, "type/basic.json"}


def test_closure_terminates_on_a_reference_cycle(upstream):
    # tagLabel <-> tagLabelMetadata reference each other upstream; a walk
    # without a seen-set would never return.
    upstream[v.ENTRY] = column("../../type/a.json")
    upstream["type/a.json"] = {"properties": {"x": {"$ref": "b.json"}}}
    upstream["type/b.json"] = {"properties": {"y": {"$ref": "a.json"}}}

    got = v.closure("deadbeef")
    assert set(got) == {v.ENTRY, "type/a.json", "type/b.json"}


def test_the_entry_document_is_vendored_whole(upstream):
    # Subsetting table.json down to the column node would make the recorded
    # sha256 describe a file that exists nowhere upstream.
    upstream[v.ENTRY] = column()
    got = v.closure("deadbeef")
    assert json.loads(got[v.ENTRY]) == upstream[v.ENTRY]


@pytest.mark.parametrize("bad", ["1.13.2", "main", "2763bf9", "ZZZZ" * 10])
def test_a_moving_ref_is_refused(monkeypatch, capsys, bad):
    # third_party/README.md requires a commit SHA, "never a moving branch/tag".
    # A tag would make the vendored copy silently re-point on a retag.
    monkeypatch.setattr(sys, "argv", ["vendor", bad])
    assert v.main() == 1
    assert "40-character commit SHA" in capsys.readouterr().out


def test_a_full_sha_is_accepted(monkeypatch, capsys, upstream):
    upstream[v.ENTRY] = column()
    upstream_license = b"Apache License"
    monkeypatch.setattr(v, "VENDOR", pathlib.Path(v.VENDOR))

    real_fetch = v.fetch

    def fetch_with_license(url):
        if url.endswith("/LICENSE"):
            return upstream_license
        return real_fetch(url)

    monkeypatch.setattr(v, "fetch", fetch_with_license)
    written = {}
    monkeypatch.setattr(pathlib.Path, "write_bytes", lambda self, b: written.__setitem__(self, b))
    monkeypatch.setattr(pathlib.Path, "mkdir", lambda self, **kw: None)
    monkeypatch.setattr(sys, "argv", ["vendor", "a" * 40])

    assert v.main() == 0
    out = capsys.readouterr().out
    assert "| File | Bytes | sha256 |" in out, out
    assert any(p.name == "LICENSE" for p in written), "the licence must be vendored too"
