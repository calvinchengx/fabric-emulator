"""`read.json(multiLine=True)` is intercepted, not left to Sail's NDJSON reader.

The engine-matrix probe writes a four-line JSON *array* through Spark's text
writer (a directory with a part file and `_SUCCESS`) and asserts two rows.
Sail parses NDJSON only; wrapping every json() would hide that. These tests
pin the named shape, and pin everything else falling through.
"""
import json
import sys
import types
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import json_multiline as jm  # noqa: E402

PROBE_BODY = '[\n  {"id": 1},\n  {"id": 2}\n]'


class FakeSpark:
    read: object

    def __init__(self):
        self.frames = []
        self.read = types.SimpleNamespace()

    def createDataFrame(self, data, schema=None):  # noqa: N802 — pyspark's name
        self.frames.append((data, schema))
        return f"DF{len(data)}"


def _spark_text_dir(tmp_path, body=PROBE_BODY):
    """What `coalesce(1).write.text` leaves: a part file plus `_SUCCESS`."""
    d = tmp_path / "t_multiline"
    d.mkdir()
    (d / "_SUCCESS").write_text("")
    (d / "part-00000-xxx.txt").write_text(body)
    return d


# --- intercept decision -------------------------------------------------------

def test_multiline_truthy_accepts_spark_spellings():
    assert jm.multiline_truthy("true") and jm.multiline_truthy("TRUE")
    assert jm.multiline_truthy("1") and jm.multiline_truthy(True)
    assert not jm.multiline_truthy("false") and not jm.multiline_truthy(None)
    assert not jm.multiline_truthy("") and not jm.multiline_truthy(False)


def test_should_intercept_only_the_named_shapes():
    assert jm.should_intercept("/tmp/t", multiLine=True)
    assert jm.should_intercept("/tmp/t", multiLine="true")
    assert jm.should_intercept("/tmp/t", options={"multiLine": "true"})
    assert jm.should_intercept("/tmp/t", options={"multiline": "1"})
    # Plain json, explicit false, a list of paths, a schema: the engine's.
    assert not jm.should_intercept("/tmp/t")
    assert not jm.should_intercept("/tmp/t", multiLine=False)
    assert not jm.should_intercept("/tmp/t", multiLine=True, schema="id INT")
    assert not jm.should_intercept(["/tmp/a", "/tmp/b"], multiLine=True)
    assert not jm.should_intercept(None, multiLine=True)


# --- parse: the probe fixture and its neighbours ------------------------------

def test_probe_shaped_spark_text_dir_yields_two_objects(tmp_path):
    d = _spark_text_dir(tmp_path)
    assert jm.records_from_path(str(d)) == [{"id": 1}, {"id": 2}]


def test_success_crc_and_dotfiles_are_skipped(tmp_path):
    d = tmp_path / "t"
    d.mkdir()
    (d / "_SUCCESS").write_text("not json")
    (d / "part-00000.txt.crc").write_text("not json")
    (d / ".hidden.json").write_text('[{"id": 9}]')
    (d / "part-00000.txt").write_text('[{"id": 1}]')
    assert jm.records_from_path(str(d)) == [{"id": 1}]


def test_a_single_json_object_file_is_one_row(tmp_path):
    p = tmp_path / "one.json"
    p.write_text('{"id": 7, "name": "x"}')
    assert jm.records_from_path(str(p)) == [{"id": 7, "name": "x"}]


def test_two_part_files_are_concatenated(tmp_path):
    d = tmp_path / "t"
    d.mkdir()
    (d / "part-00000").write_text('[{"id": 1}]')
    (d / "part-00001").write_text('[{"id": 2}]')
    assert jm.records_from_path(str(d)) == [{"id": 1}, {"id": 2}]


def test_an_empty_directory_names_the_gap(tmp_path):
    d = tmp_path / "empty"
    d.mkdir()
    (d / "_SUCCESS").write_text("")
    with pytest.raises(jm.JsonMultilineError, match="no JSON"):
        jm.records_from_path(str(d))


def test_a_json_array_of_non_objects_is_refused(tmp_path):
    p = tmp_path / "n.json"
    p.write_text("[1, 2, 3]")
    with pytest.raises(jm.JsonMultilineError, match="objects"):
        jm.records_from_path(str(p))


def test_invalid_json_is_refused_not_sent_to_sail(tmp_path):
    p = tmp_path / "bad.json"
    p.write_text("not json")
    with pytest.raises(jm.JsonMultilineError, match="not valid JSON"):
        jm.records_from_path(str(p))


def test_ndjson_is_not_a_json_array(tmp_path):
    # multiLine=True on NDJSON is the wrong option; do not silently split lines.
    p = tmp_path / "ndjson.json"
    p.write_text('{"id": 1}\n{"id": 2}\n')
    with pytest.raises(jm.JsonMultilineError, match="not valid JSON"):
        jm.records_from_path(str(p))


def test_records_from_abfss_uses_notebookutils_fs(monkeypatch):
    class Info:
        def __init__(self, path, name, is_file=True):
            self.path, self.name, self.isFile = path, name, is_file

    body = {("abfss://ws@host/t/part-00000"): PROBE_BODY}

    def ls(path):
        assert path == "abfss://ws@host/t"
        return [
            Info("abfss://ws@host/t/_SUCCESS", "_SUCCESS"),
            Info("abfss://ws@host/t/part-00000", "part-00000"),
        ]

    fake_fs = types.SimpleNamespace(
        ls=ls,
        read=lambda p: body[p].encode(),
    )
    monkeypatch.setitem(sys.modules, "notebookutils", types.SimpleNamespace(fs=fake_fs))
    monkeypatch.setitem(sys.modules, "notebookutils.fs", fake_fs)
    assert jm.records_from_path("abfss://ws@host/t") == [{"id": 1}, {"id": 2}]


def test_read_json_multiline_materialises_and_announces(tmp_path, capsys):
    d = _spark_text_dir(tmp_path)
    spark = FakeSpark()
    out = jm.read_json_multiline(spark, str(d))
    assert out == "DF2"
    data, _schema = spark.frames[0]
    assert data == [{"id": 1}, {"id": 2}]
    assert "materialised LocalRelation" in capsys.readouterr().err


# --- the json() wrap ----------------------------------------------------------

def test_patch_intercepts_keyword_multiline_and_skips_the_engine(tmp_path):
    d = _spark_text_dir(tmp_path)

    class Reader:
        def json(self, path=None, *args, **kwargs):
            raise AssertionError("engine must not see multiLine=True")

    spark = FakeSpark()
    jm.patch_json_reader(Reader, spark)
    assert Reader().json(str(d), multiLine=True) == "DF2"
    assert spark.frames[0][0] == [{"id": 1}, {"id": 2}]


def test_patch_intercepts_option_multiline(tmp_path):
    p = tmp_path / "a.json"
    p.write_text(json.dumps([{"id": 1}, {"id": 2}]))

    class Reader:
        def json(self, path=None, *args, **kwargs):
            raise AssertionError("engine must not see option multiLine")

    spark = FakeSpark()
    jm.patch_json_reader(Reader, spark)
    reader = Reader()
    reader._options = {"multiLine": "true"}
    assert reader.json(str(p)) == "DF2"


def test_patch_leaves_plain_json_and_multiline_false_on_the_engine(tmp_path):
    p = tmp_path / "a.json"
    p.write_text('{"id": 1}\n{"id": 2}\n')

    class Reader:
        def json(self, path=None, *args, **kwargs):
            return ("engine", path, kwargs.get("multiLine"))

    spark = FakeSpark()
    jm.patch_json_reader(Reader, spark)
    r = Reader()
    assert r.json(str(p))[0] == "engine"
    assert r.json(str(p), multiLine=False) == ("engine", str(p), False)
    assert spark.frames == []


def test_patch_leaves_a_schema_call_on_the_engine(tmp_path):
    p = tmp_path / "a.json"
    p.write_text(PROBE_BODY)

    class Reader:
        def json(self, path=None, *args, **kwargs):
            return "engine"

    spark = FakeSpark()
    jm.patch_json_reader(Reader, spark)
    assert Reader().json(str(p), schema="id INT", multiLine=True) == "engine"
    assert spark.frames == []


def test_install_is_a_noop_without_a_connect_reader():
    spark = FakeSpark()
    assert jm.install(spark) is False


def test_a_missing_path_is_no_json_records(tmp_path):
    with pytest.raises(jm.JsonMultilineError, match="no JSON"):
        jm.records_from_path(str(tmp_path / "does-not-exist"))


def test_an_empty_file_in_the_directory_is_skipped(tmp_path):
    d = tmp_path / "t"
    d.mkdir()
    (d / "part-empty").write_text("  \n")
    (d / "part-00000").write_text('{"id": 1}')
    assert jm.records_from_path(str(d)) == [{"id": 1}]


def test_a_json_scalar_is_refused(tmp_path):
    p = tmp_path / "n.json"
    p.write_text("1")
    with pytest.raises(jm.JsonMultilineError, match="object or array"):
        jm.records_from_path(str(p))


def test_abfss_ls_failure_treats_the_path_as_a_file(monkeypatch):
    body = {"abfss://ws@host/t.json": '[{"id": 1}]'}

    def ls(_path):
        raise FileNotFoundError("not a directory")

    fake_fs = types.SimpleNamespace(
        ls=ls,
        read=lambda p: body[p].encode(),
    )
    monkeypatch.setitem(sys.modules, "notebookutils", types.SimpleNamespace(fs=fake_fs))
    monkeypatch.setitem(sys.modules, "notebookutils.fs", fake_fs)
    assert jm.records_from_path("abfss://ws@host/t.json") == [{"id": 1}]


def test_a_whole_file_is_read_not_previewed(monkeypatch):
    """`head` is a PREVIEW — first `max_bytes`, 100 KB by default. This reader
    parses JSON and needs every byte, so it calls `read`.

    It used to call `head` and worked only because the shim's `head` diverged
    from Fabric's by returning the whole file. Once corrected (docs/56 Phase 2)
    a document over 100 KB would have been truncated mid-parse — or worse,
    parsed as a shorter valid one. The oversized body here is the regression
    guard: a `head`-shaped reader cannot pass it."""
    big = "[" + ", ".join(f'{{"id": {i}}}' for i in range(20000)) + "]"
    assert len(big) > 100 * 1024, "the body must exceed head's default to bite"
    fake_fs = types.SimpleNamespace(
        ls=lambda p: (_ for _ in ()).throw(RuntimeError("file")),
        read=lambda p: big.encode(),
        head=lambda f, max_bytes=1024 * 100: big[:max_bytes],
    )
    monkeypatch.setitem(sys.modules, "notebookutils", types.SimpleNamespace(fs=fake_fs))
    monkeypatch.setitem(sys.modules, "notebookutils.fs", fake_fs)
    got = jm.records_from_path("abfs://ws@host/one.json")
    assert len(got) == 20000
    assert got[-1] == {"id": 19999}


def test_patch_is_idempotent(tmp_path):
    p = tmp_path / "a.json"
    p.write_text('{"id": 1}')

    class Reader:
        def json(self, path=None, *args, **kwargs):
            return "engine"

    spark = FakeSpark()
    jm.patch_json_reader(Reader, spark)
    first = Reader.json
    jm.patch_json_reader(Reader, spark)
    assert Reader.json is first


def test_patch_treats_positional_schema_as_the_engine(tmp_path):
    p = tmp_path / "a.json"
    p.write_text(PROBE_BODY)

    class Reader:
        def json(self, path=None, *args, **kwargs):
            return ("engine", args)

    spark = FakeSpark()
    jm.patch_json_reader(Reader, spark)
    assert Reader().json(str(p), "id INT", multiLine=True) == ("engine", ("id INT",))
    assert spark.frames == []


def test_install_patches_a_connect_reader(monkeypatch, tmp_path):
    class DataFrameReader:
        def json(self, path=None, *args, **kwargs):
            return "engine"

    rw = types.ModuleType("pyspark.sql.connect.readwriter")
    rw.DataFrameReader = DataFrameReader
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect", types.ModuleType("pyspark.sql.connect"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.readwriter", rw)

    p = tmp_path / "a.json"
    p.write_text('[{"id": 1}, {"id": 2}]')
    spark = FakeSpark()
    spark.read = DataFrameReader()
    assert jm.install(spark) is True
    assert spark.read.json(str(p), multiLine=True) == "DF2"


def test_delta_ops_install_installs_json_multiline(monkeypatch):
    # The sail-delta probe only calls delta_ops.install. If this seam breaks,
    # the matrix stays red while unit tests stay green.
    import delta_ops as d

    seen = []
    monkeypatch.setattr(jm, "install", lambda s: seen.append(s))
    spark = types.SimpleNamespace(sql=lambda q: q, read=None)
    d.install(spark, storage_options={})
    assert seen == [spark]
