"""The `input_file_name()` shim must not be visible.

The shim tags each file's rows with that file's path so the function can answer
truthfully on an engine that lacks it. That tag is bookkeeping: real Fabric has
no such column, and `docs/37-runtime-fidelity-gaps.md` names `df.columns`,
`printSchema`, `SELECT *` and `toPandas` as the surfaces where leaking it would
create the parity drift this module exists to prevent.

It leaked on all of them. Stripping at write covered persisted data only, so a
landing-to-bronze step that counted `len(df.columns)` saw one column more than
its vendor export had, while the table it wrote was correct — which is what made
it expensive to find.

These tests use a fake Connect DataFrame rather than an engine: what is being
checked is which frame each surface delegates to, and that is a property of the
patching, not of Spark.
"""

import os
import sys
import types
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import input_file as input_file_mod  # noqa: E402

TAG = "__emu_input_file_name"


class FakeDFBase:
    """Just enough DataFrame to observe what the patches delegate to."""

    def __init__(self, cols):
        self._cols = list(cols)

    # The real class exposes these as properties, which is what the shim patches.
    @property
    def columns(self):
        return list(self._cols)

    @property
    def schema(self):
        return f"struct<{','.join(self._cols)}>"

    @property
    def dtypes(self):
        return [(c, "string") for c in self._cols]

    @property
    def write(self):
        return f"writer({','.join(self._cols)})"

    def drop(self, *names):
        return type(self)([c for c in self._cols if c not in names])

    def printSchema(self):  # noqa: N802 - PySpark's spelling
        return self.schema

    def show(self, *a, **k):
        return f"show({','.join(self._cols)})"

    def collect(self):
        return [tuple(self._cols)]

    def take(self, n):
        return [tuple(self._cols)][:n]

    def head(self, *a):
        return tuple(self._cols)

    def first(self):
        return tuple(self._cols)

    def toPandas(self):  # noqa: N802 - PySpark's spelling
        return {"columns": list(self._cols)}

    def toLocalIterator(self):  # noqa: N802 - PySpark's spelling
        return iter([tuple(self._cols)])

    def createOrReplaceTempView(self, name):  # noqa: N802 - PySpark's spelling
        return f"{name}:{','.join(self._cols)}"

    def select(self, *args):
        return type(self)([str(a) for a in args] if args else self._cols)

    def selectExpr(self, *args):  # noqa: N802 - PySpark's spelling
        return type(self)([str(a) for a in args])


def _stub_pyspark(monkeypatch, frame_cls):
    """Put the pyspark surface this module imports into sys.modules."""
    functions = types.ModuleType("pyspark.sql.functions")
    functions.lit = lambda v: f"lit({v})"
    functions.col = lambda c: c
    functions.coalesce = lambda *a: f"coalesce({','.join(str(x) for x in a)})"
    # Real pyspark HAS this attribute — the probe's job is to find out whether
    # the ENGINE resolves it, not whether the client library declares it. A stub
    # without it makes every engine look like one that lacks the function.
    functions.input_file_name = lambda: "input_file_name()"
    sql_mod = types.ModuleType("pyspark.sql")
    sql_mod.functions = functions
    root_mod = types.ModuleType("pyspark")
    root_mod.sql = sql_mod
    connect_mod = types.ModuleType("pyspark.sql.connect.dataframe")
    connect_mod.DataFrame = frame_cls
    for name, mod in (
        ("pyspark", root_mod),
        ("pyspark.sql", sql_mod),
        ("pyspark.sql.functions", functions),
        ("pyspark.sql.connect", types.ModuleType("pyspark.sql.connect")),
        ("pyspark.sql.connect.dataframe", connect_mod),
    ):
        monkeypatch.setitem(sys.modules, name, mod)


@pytest.fixture
def stubs(monkeypatch):
    """Only the import stubs — nothing in the module itself is replaced.

    The helpers below ARE the code under test, so a fixture that monkeypatched
    them would leave the test asserting against its own lambda.
    """
    _stub_pyspark(monkeypatch, type("FakeDF", (FakeDFBase,), {}))
    return input_file_mod


@pytest.fixture
def patched(monkeypatch):
    """Install the shim's DataFrame patches against FakeDF.

    The module imports `pyspark.sql.connect.dataframe.DataFrame` inside
    `install()`, so a stub module is enough — no engine, and no pyspark import
    behaviour to work around.
    """
    input_file = input_file_mod
    FakeDF = type("FakeDF", (FakeDFBase,), {})

    # STUB THE WHOLE PYSPARK SURFACE THIS MODULE IMPORTS, rather than relying on
    # pyspark being installed. It is not installed in CI — the agent's imports
    # are why `unresolved-import` is ignored for the type checker — and these
    # tests passed locally only because a development venv happened to carry
    # `pyspark-client`. A test that depends on an ambient package tests the
    # machine it runs on.
    _stub_pyspark(monkeypatch, FakeDF)

    class FakeReader:
        def csv(self, path, *a, **k):
            return FakeDF(["id", "name"])

    class FakeSpark:
        read = FakeReader()

    # The engine "lacks" the function, which is when the shim installs.
    monkeypatch.setattr(input_file, "engine_has_input_file_name", lambda _s: False)
    monkeypatch.setattr(input_file, "_list_files", lambda _p: ["/landing/part-1.csv"])
    input_file.install(FakeSpark())
    yield input_file, FakeDF


def test_the_tag_is_hidden_from_the_schema_surfaces(patched):
    _, FakeDF = patched
    df = FakeDF(["id", "name", TAG])
    assert df.columns == ["id", "name"], "columns still shows the bookkeeping tag"
    assert TAG not in df.schema
    assert [c for c, _ in df.dtypes] == ["id", "name"]
    assert TAG not in df.printSchema()


def test_the_tag_is_hidden_from_materialised_rows(patched):
    _, FakeDF = patched
    df = FakeDF(["id", "name", TAG])
    assert df.collect() == [("id", "name")]
    assert df.take(1) == [("id", "name")]
    assert df.head() == ("id", "name")
    assert df.first() == ("id", "name")
    assert df.toPandas() == {"columns": ["id", "name"]}
    assert list(df.toLocalIterator()) == [("id", "name")]
    assert TAG not in df.show()


def test_the_tag_does_not_reach_sql_through_a_temp_view(patched):
    # A view is how the tag would escape into `SELECT *` on the SQL path, which
    # no amount of write-stripping would catch.
    _, FakeDF = patched
    assert TAG not in FakeDF(["id", "name", TAG]).createOrReplaceTempView("v")


def test_star_expansion_does_not_carry_the_tag(patched):
    _, FakeDF = patched
    assert FakeDF(["id", "name", TAG]).select("*").columns == ["*"]
    # The frame handed to select must already be clean, or the server expands
    # `*` over a plan that still has the tag.
    assert FakeDF(["id", "name", TAG]).select().columns == ["id", "name"]


def test_a_select_that_asks_for_the_tag_still_gets_it(patched):
    # The whole point of the module. `input_file_name()` resolves to the tag, so
    # hiding it unconditionally here would make provenance impossible — the
    # failure mode this fix must not introduce.
    _, FakeDF = patched
    out = FakeDF(["id", "name", TAG]).select("id", f"coalesce({TAG}, '')")
    assert any(TAG in c for c in out.columns)


def test_writes_still_strip_the_tag(patched):
    _, FakeDF = patched
    assert TAG not in FakeDF(["id", "name", TAG]).write


def test_an_untagged_frame_is_untouched(patched):
    # Frames that never came from a file read must pass through unchanged, or
    # the shim starts editing data it has no business touching.
    _, FakeDF = patched
    plain = FakeDF(["a", "b"])
    assert plain.columns == ["a", "b"]
    assert plain.collect() == [("a", "b")]
    assert plain.write == "writer(a,b)"


def test_the_engine_probe_answers_both_ways(stubs):
    # `install()` is a no-op on an engine that already has the function — that
    # is what keeps the JVM overlay's real implementation from being shadowed
    # by an approximation, so both answers matter.
    input_file = stubs

    class Has:
        def range(self, _n):
            return self

        def select(self, *_a):
            return self

        def collect(self):
            return [1]

    class Lacks:
        def range(self, _n):
            raise RuntimeError("function: input_file_name")

    assert input_file.engine_has_input_file_name(Has()) is True
    assert input_file.engine_has_input_file_name(Lacks()) is False


def test_listing_files_behind_a_local_path(stubs, tmp_path):
    # The lister is what turns one glob read into a read per file, which is how
    # each row can carry the file it truly came from. Order is asserted because
    # the per-file union is positional: an unstable order would silently
    # misalign rows against their paths.
    input_file = stubs
    for name in ("part-0003.csv", "part-0001.csv", "part-0002.csv"):
        (tmp_path / name).write_text("id\n1\n")
    (tmp_path / "notes.txt").write_text("ignore me")

    got = input_file._list_files(str(tmp_path / "*.csv"))
    # os.path.basename, not a "/" split: glob returns the platform's own
    # separator, and CI runs this on Windows too.
    assert [os.path.basename(p) for p in got] == [
        "part-0001.csv",
        "part-0002.csv",
        "part-0003.csv",
    ]


def test_a_glob_in_a_directory_segment_is_refused(stubs):
    # Expanding a directory glob would need a listing per candidate directory,
    # so the module keeps the plain glob read instead and the tag degrades to
    # the glob string: coarse, but honest. An empty list is what signals that.
    assert stubs._list_files("/landing/*/customers/part-0001.csv") == []


def test_a_single_file_read_carries_its_own_path(patched, monkeypatch):
    # The tagging path itself: one file in, one tagged frame out, and the tag
    # holds THAT file rather than the glob it was asked for.
    input_file, FakeDF = patched
    tagged_with = {}

    class Reader:
        def csv(self, path, *a, **k):
            return FakeDF(["id", "name"])

    monkeypatch.setattr(input_file, "_list_files", lambda _p: ["/landing/part-0007.csv"])

    class Spark:
        read = Reader()

    # Re-install against this reader so the wrapper is the one under test.
    original_with_column = FakeDF.withColumn if hasattr(FakeDF, "withColumn") else None

    def withColumn(self, name, value):  # noqa: N802 - PySpark's spelling
        tagged_with[name] = value
        return type(self)([*self._cols, name])

    FakeDF.withColumn = withColumn
    monkeypatch.setattr(input_file, "engine_has_input_file_name", lambda _s: False)
    input_file.install(Spark())

    out = Spark.read.csv("/landing/*.csv")
    assert TAG in out._cols, "a file read must carry the tag for provenance to resolve"
    assert "part-0007.csv" in str(tagged_with[TAG]), "the tag must name the file, not the glob"
    if original_with_column is None:
        del FakeDF.withColumn
