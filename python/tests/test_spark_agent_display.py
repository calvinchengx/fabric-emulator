"""`display()` and `displayHTML()` — the notebook builtins.

docs/56 carried this row as "unverified". Measured, it was ABSENT: no
definition anywhere in the tree and no caller, so `display(df)` — one of the
most common lines in any Fabric notebook — raised NameError. Not a fidelity
nuance; a notebook written the ordinary way did not run.

Rendering is separated from printing so the OUTPUT is assertable without
capturing stdout, and the frames below are duck-typed for the same reason the
module is: it must not drag pyspark in, and the two engines here expose
different classes for the same thing.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import notebook_display  # noqa: E402


class FakeSparkFrame:
    """The three attributes `_is_spark_frame` keys on, and nothing else."""

    def __init__(self, columns, rows, dtypes=None):
        self.columns = columns
        self.schema = object()
        self._rows = rows
        self.dtypes = dtypes or [(c, "string") for c in columns]

    def show(self):  # present so the duck-type matches
        raise AssertionError("render must not call show(): it prints, and the "
                             "output has to be returnable to be assertable")

    def limit(self, _n):
        return self

    def collect(self):
        return self._rows


class FakePandasFrame:
    def __init__(self, text, columns=()):
        self._text = text
        self.columns = list(columns)

    def to_string(self):
        return self._text


def test_a_spark_frame_renders_its_rows():
    df = FakeSparkFrame(["id", "name"], [(1, "a"), (2, "b")])
    out = notebook_display.render(df)
    assert "id | name" in out
    assert "1 | a" in out and "2 | b" in out


def test_a_none_renders_as_empty_not_as_the_word_None():
    """A null cell in a table is blank on Fabric. Printing "None" would make a
    missing value indistinguishable from the string 'None'."""
    df = FakeSparkFrame(["id", "name"], [(1, None)])
    assert "1 | " in notebook_display.render(df)
    assert "None" not in notebook_display.render(df)


def test_an_empty_frame_still_renders_its_header():
    """A frame with no rows is a real answer — showing nothing at all would
    look like the call failed."""
    out = notebook_display.render(FakeSparkFrame(["id"], []))
    assert out.strip() == "id"


def test_a_pandas_frame_renders_through_to_string():
    assert notebook_display.render(FakePandasFrame("  id\n0  1")) == "  id\n0  1"


def test_summary_reports_type_unique_and_missing_for_pandas():
    """Fabric documents summary as column name, type, unique values and
    missing values — a data QUALITY read a notebook branches on. Ignoring the
    flag would answer a different question than the one asked."""
    class Column:
        def __init__(self, values):
            self._values = values
            self.dtype = "int64"

        def nunique(self):
            return len({v for v in self._values if v is not None})

        def isna(self):
            return type("S", (), {"sum": lambda _s: sum(
                1 for v in self._values if v is None)})()

    class Frame:
        def __init__(self):
            self.columns = ["id"]

        def to_string(self):
            return "unused"

        def __getitem__(self, name):
            return Column([1, 2, 2, None])

    out = notebook_display.render(Frame(), summary=True)
    assert "column" in out and "unique" in out and "missing" in out
    assert "int64" in out
    assert " 2 " in out or out.rstrip().endswith("2")


def test_a_scalar_renders_as_itself():
    assert notebook_display.render(42) == "42"
    assert notebook_display.render("hello") == "hello"


def test_an_rdd_renders_its_take():
    """Fabric renders RDDs too. A frame is distinguished by having `columns`;
    an RDD has `take` and does not."""
    rdd = type("RDD", (), {"take": lambda _s, n: [1, 2, 3][:n]})()
    assert notebook_display.render(rdd) == "1\n2\n3"


def test_display_prints_and_returns_nothing(capsys):
    """It is an output call, like Fabric's."""
    assert notebook_display.display(FakePandasFrame("body")) is None
    assert "body" in capsys.readouterr().out


def test_displayHTML_emits_the_markup_rather_than_swallowing_it(capsys):
    """There is nothing here to render into, so the markup is emitted as-is: a
    notebook's output still CONTAINS what it asked to show, and a harness
    reading that output can assert on it. Swallowing it would make the call
    look successful and produce nothing."""
    notebook_display.displayHTML("<h1>Title</h1>")
    assert "<h1>Title</h1>" in capsys.readouterr().out


def test_a_spark_frame_is_not_mistaken_for_a_pandas_one():
    """Both have `columns`. Getting this backwards would send a Spark frame
    down `to_string`, which it does not have."""
    df = FakeSparkFrame(["id"], [(1,)])
    assert notebook_display._is_spark_frame(df) is True
    assert notebook_display._is_pandas_frame(df) is False


def test_the_spark_summary_asks_the_engine_for_distinct_and_null_counts(monkeypatch):
    """The summary is a real aggregation, not a guess. Fabric computes it;
    inventing plausible numbers would be worse than not having the flag.

    The engine is faked at the `functions` boundary so the SHAPE of the
    aggregation is asserted — one countDistinct and one null-count per column —
    without starting Spark.
    """
    import types

    asked = []

    class Col:
        def __init__(self, name):
            self.name = name

        def isNull(self):  # noqa: N802 - pyspark's spelling
            return f"{self.name} IS NULL"

    def alias_of(kind):
        def make(arg):
            return types.SimpleNamespace(
                alias=lambda name: asked.append((kind, name)) or name)
        return make

    fake = types.SimpleNamespace(
        col=lambda name: Col(name.strip("`")),
        countDistinct=alias_of("uniq"),
        count=alias_of("null"),
        when=lambda cond, val: cond,
    )
    monkeypatch.setitem(sys.modules, "pyspark",
                        types.ModuleType("pyspark"))
    sql_mod = types.ModuleType("pyspark.sql")
    sql_mod.functions = fake
    monkeypatch.setitem(sys.modules, "pyspark.sql", sql_mod)

    row = {"__uniq__id": 3, "__null__id": 1,
           "__uniq__name": 2, "__null__name": 0}
    df = FakeSparkFrame(["id", "name"], [], dtypes=[("id", "bigint"),
                                                    ("name", "string")])
    df.agg = lambda *a: types.SimpleNamespace(collect=lambda: [row])

    out = notebook_display.render(df, summary=True)
    assert [k for k, _ in asked] == ["uniq", "null", "uniq", "null"]
    assert "bigint" in out and "string" in out
    assert "3" in out and "1" in out


def test_a_spark_summary_of_a_frame_with_no_columns_still_renders_a_header():
    """A header-only answer is a real one; rendering nothing would look like
    the call failed."""
    import types
    df = FakeSparkFrame([], [])
    df.agg = lambda *a: types.SimpleNamespace(collect=lambda: [{}])
    out = notebook_display.render(df, summary=True)
    assert "column" in out and "missing" in out
