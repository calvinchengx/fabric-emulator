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

import pytest

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


@pytest.fixture
def fake_functions(monkeypatch):
    """`pyspark.sql.functions`, stubbed.

    THIS SUITE MUST RUN WITHOUT PYSPARK. The shim-test job installs the
    notebookutils group and nothing else, so a test that reached the real
    import passed on a laptop and failed on CI — measured, on macos-latest,
    exactly that way. `notebook_display` imports pyspark lazily inside
    `_spark_summary` precisely so the module stays importable in that
    environment; the tests have to honour the same constraint.
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
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    sql_mod = types.ModuleType("pyspark.sql")
    sql_mod.functions = fake
    monkeypatch.setitem(sys.modules, "pyspark.sql", sql_mod)
    return asked


def test_the_spark_summary_asks_the_engine_for_distinct_and_null_counts(fake_functions):
    """The summary is a real aggregation, not a guess. Fabric computes it;
    inventing plausible numbers would be worse than not having the flag.

    The engine is faked at the `functions` boundary so the SHAPE of the
    aggregation is asserted — one countDistinct and one null-count per column —
    without starting Spark.
    """
    import types

    row = {"__uniq__id": 3, "__null__id": 1,
           "__uniq__name": 2, "__null__name": 0}
    df = FakeSparkFrame(["id", "name"], [], dtypes=[("id", "bigint"),
                                                    ("name", "string")])
    df.agg = lambda *a: types.SimpleNamespace(collect=lambda: [row])

    out = notebook_display.render(df, summary=True)
    assert [k for k, _ in fake_functions] == ["uniq", "null", "uniq", "null"]
    assert "bigint" in out and "string" in out
    assert "3" in out and "1" in out


def test_a_spark_summary_of_a_frame_with_no_columns_still_renders_a_header(fake_functions):
    """A header-only answer is a real one; rendering nothing would look like
    the call failed."""
    import types
    df = FakeSparkFrame([], [])
    df.agg = lambda *a: types.SimpleNamespace(collect=lambda: [{}])
    out = notebook_display.render(df, summary=True)
    assert "column" in out and "missing" in out


# --- rich output --------------------------------------------------------------
#
# The agent has no kernel, so these exercise the branch e2e/notebook-display
# proves end to end, plus the fallbacks that e2e cannot reach without breaking
# the environment it runs in.

def test_without_a_kernel_display_still_prints(monkeypatch, capsys):
    """The agent's RunNotebook path has no IPython, and its output is what the
    conformance suite asserts on. A rich-output path that stopped printing
    there would take every stdout assertion with it."""
    monkeypatch.setattr(notebook_display, "_kernel", lambda: None)
    notebook_display.display(FakeSparkFrame(["id"], [(1,)]))
    assert "id" in capsys.readouterr().out


def test_with_a_kernel_display_publishes_html_and_text(monkeypatch):
    published = {}
    monkeypatch.setattr(notebook_display, "_publish",
                        lambda bundle: published.update(bundle) or True)
    notebook_display.display(FakeSparkFrame(["id"], [(1,)]))
    assert "<th>id</th>" in published["text/html"]
    # The plain alternative rides along: a front end without HTML still shows
    # the data, and it must be the SAME data rather than a second rendering.
    assert "id" in published["text/plain"]


def test_a_front_end_that_refuses_does_not_lose_the_output(monkeypatch, capsys):
    """publish_display_data raising must not swallow the cell's output. Losing
    it would make the call look successful and produce nothing.

    `_publisher` is patched, not `_kernel` alone: this environment has no
    IPython, so patching the kernel would leave the import failing and the test
    would pass on that instead of on the branch it names.
    """
    def boom(_bundle):
        raise RuntimeError("no front end")

    monkeypatch.setattr(notebook_display, "_kernel", lambda: object())
    monkeypatch.setattr(notebook_display, "_publisher", lambda: boom)
    notebook_display.display(FakeSparkFrame(["id"], [(1,)]))
    assert "id" in capsys.readouterr().out


def test_a_kernel_with_no_publisher_still_prints(monkeypatch, capsys):
    """A shell without IPython's display machinery is the agent's own case."""
    monkeypatch.setattr(notebook_display, "_kernel", lambda: object())
    monkeypatch.setattr(notebook_display, "_publisher", lambda: None)
    notebook_display.display(FakeSparkFrame(["id"], [(1,)]))
    assert "id" in capsys.readouterr().out


def test_the_bundle_reaches_the_publisher(monkeypatch):
    """The real path, with both halves patched: a kernel and a publisher."""
    seen = {}
    monkeypatch.setattr(notebook_display, "_kernel", lambda: object())
    monkeypatch.setattr(notebook_display, "_publisher", lambda: seen.update)
    notebook_display.display(FakeSparkFrame(["id"], [(1,)]))
    assert "<th>id</th>" in seen["text/html"]


def test_data_is_escaped_because_a_column_can_hold_markup():
    """Cell values are arbitrary. A table that emitted them raw would let a
    stored value execute in whatever renders this output."""
    html = notebook_display.render_html(
        FakeSparkFrame(["danger"], [("<script>x</script>",)]))
    assert "<script>" not in html
    assert "&lt;script&gt;" in html


def test_a_column_name_is_escaped_too():
    """A column name comes from user data as surely as a cell does."""
    html = notebook_display.render_html(FakeSparkFrame(["<b>id</b>"], []))
    assert "<b>id</b>" not in html
    assert "&lt;b&gt;id&lt;/b&gt;" in html


def test_a_scalar_renders_as_preformatted_text():
    """Nothing tabular to build a table from, so the value is shown as-is —
    escaped, because `str(obj)` is no safer than a cell value."""
    assert notebook_display.render_html("<i>hi</i>") == "<pre>&lt;i&gt;hi&lt;/i&gt;</pre>"


def test_displayhtml_publishes_its_markup_unescaped(monkeypatch):
    """The one place markup must NOT be escaped: the caller asked for HTML."""
    published = {}
    monkeypatch.setattr(notebook_display, "_publish",
                        lambda bundle: published.update(bundle) or True)
    notebook_display.displayHTML("<b>hi</b>")
    assert published["text/html"] == "<b>hi</b>"


def test_displayhtml_without_a_kernel_prints(monkeypatch, capsys):
    monkeypatch.setattr(notebook_display, "_kernel", lambda: None)
    notebook_display.displayHTML("<b>hi</b>")
    assert "<b>hi</b>" in capsys.readouterr().out


def test_the_summary_table_carries_fabrics_four_fields():
    headers, _rows = notebook_display.table(
        FakePandasFrame("x"), summary=True) or ([], [])
    assert headers == list(notebook_display.SUMMARY_HEADERS)


def _fake_ipython(monkeypatch, shell, publisher):
    """Install a stand-in `IPython` so the import branches run.

    This environment ships no IPython — the agent needs none — so without a
    stand-in `_kernel` and `_publisher` can only ever be observed FAILING to
    import, and the branch that matters under a real kernel goes unexecuted.
    """
    import sys
    import types

    root = types.ModuleType("IPython")
    root.get_ipython = lambda: shell
    display_mod = types.ModuleType("IPython.display")
    display_mod.publish_display_data = publisher
    monkeypatch.setitem(sys.modules, "IPython", root)
    monkeypatch.setitem(sys.modules, "IPython.display", display_mod)


def test_a_real_kernel_and_publisher_are_found_by_import(monkeypatch):
    sent = {}
    _fake_ipython(monkeypatch, shell=object(), publisher=sent.update)
    assert notebook_display._kernel() is not None
    assert notebook_display._publisher() is not None
    notebook_display.display(FakeSparkFrame(["id"], [(1,)]))
    assert "<th>id</th>" in sent["text/html"]


def test_no_kernel_running_means_no_publish(monkeypatch, capsys):
    """IPython importable but no shell — an ordinary `python -c` with IPython
    installed. Publishing there would raise, so it must print."""
    _fake_ipython(monkeypatch, shell=None, publisher=lambda _b: None)
    notebook_display.display(FakeSparkFrame(["id"], [(1,)]))
    assert "id" in capsys.readouterr().out


class FakeHtmlFrame:
    """A pandas-shaped frame that renders itself, as the real one does."""

    def __init__(self):
        self.columns = ["id"]

    def to_string(self):
        return "  id\n0  1"

    def to_html(self):
        return "<table><tr><td>1</td></tr></table>"


def test_pandas_renders_itself_rather_than_a_generic_grid():
    assert notebook_display.render_html(FakeHtmlFrame()) == FakeHtmlFrame().to_html()


def test_no_ipython_at_all_is_not_an_error(monkeypatch):
    """The agent's own case, asserted rather than assumed: it ships no IPython,
    and both lookups must answer None instead of raising into a user's cell."""
    import builtins

    real_import = builtins.__import__

    def refuse(name, *a, **k):
        if name.startswith("IPython"):
            raise ImportError("no IPython here")
        return real_import(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", refuse)
    assert notebook_display._kernel() is None
    assert notebook_display._publisher() is None


def test_a_shell_lookup_that_raises_is_not_an_error(monkeypatch):
    """get_ipython() itself can raise in a half-initialised environment. A
    display call must not become that traceback."""
    import sys
    import types

    root = types.ModuleType("IPython")

    def boom():
        raise RuntimeError("half a shell")

    root.get_ipython = boom
    monkeypatch.setitem(sys.modules, "IPython", root)
    assert notebook_display._kernel() is None
