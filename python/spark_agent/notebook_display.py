"""`display()` and `displayHTML()` — the notebook builtins, absent until now.

MEASURED, NOT ASSUMED. docs/56 carried this row as "unverified"; searching the
tree found no definition and no caller. So `display(df)` — one of the most
common lines in any Fabric notebook — raised `NameError: name 'display' is not
defined`. Not a fidelity nuance: a notebook written the ordinary way did not
run at all.

WHAT IS EMULATED, AND WHAT IS NOT. On Fabric `display` renders an INTERACTIVE
table, with chart views and an inspect panel. There is no browser here and
nothing to be interactive with, so this renders TEXT. The data is the same, the
shape is the same, the interactivity is not — and that divergence is stated
here rather than left to be discovered when someone goes looking for a chart.

`summary=True` is honoured, because it is not decoration: Fabric documents it as
column name, type, unique values and missing values per column, which is a data
QUALITY read a notebook branches on. Ignoring it would answer a different
question than the one asked.
"""


def _is_spark_frame(obj):
    """A Spark DataFrame without importing pyspark to find out.

    Duck-typed on purpose: this module is imported by the agent on every
    session and must not drag pyspark in, and the two engines here (Sail over
    Connect, classic Spark) expose different classes for the same thing.
    """
    return hasattr(obj, "schema") and hasattr(obj, "show") and hasattr(obj, "columns")


def _is_pandas_frame(obj):
    return hasattr(obj, "to_string") and hasattr(obj, "columns") and not _is_spark_frame(obj)


def _spark_summary(df):
    """Fabric's documented summary: name, type, unique values, missing values."""
    from pyspark.sql import functions as F  # noqa: N812 - the conventional alias

    types = dict(df.dtypes)
    aggregates = []
    for column in df.columns:
        quoted = F.col(f"`{column}`")
        aggregates.append(F.countDistinct(quoted).alias(f"__uniq__{column}"))
        aggregates.append(F.count(F.when(quoted.isNull(), 1)).alias(f"__null__{column}"))
    row = df.agg(*aggregates).collect()[0] if aggregates else None
    lines = [f"{'column':<24} {'type':<14} {'unique':>10} {'missing':>10}"]
    for column in df.columns:
        unique = row[f"__uniq__{column}"] if row is not None else 0
        missing = row[f"__null__{column}"] if row is not None else 0
        lines.append(f"{column:<24} {types.get(column, ''):<14} "
                     f"{unique:>10} {missing:>10}")
    return "\n".join(lines)


def _pandas_summary(df):
    lines = [f"{'column':<24} {'type':<14} {'unique':>10} {'missing':>10}"]
    for column in df.columns:
        lines.append(f"{column!s:<24} {df[column].dtype!s:<14} "
                     f"{df[column].nunique():>10} {int(df[column].isna().sum()):>10}")
    return "\n".join(lines)


def render(obj, summary=False):
    """The text `display` would print. Separated from printing so it is
    testable without capturing stdout."""
    if _is_spark_frame(obj):
        if summary:
            return _spark_summary(obj)
        # `show` prints; `_jdf.showString` is not available on Connect. Ask the
        # frame for its own rows and format them, which both engines can do.
        rows = obj.limit(20).collect()
        header = " | ".join(obj.columns)
        body = "\n".join(" | ".join("" if v is None else str(v) for v in r)
                         for r in rows)
        return header + "\n" + body if body else header
    if _is_pandas_frame(obj):
        return _pandas_summary(obj) if summary else obj.to_string()
    # Anything else: an RDD, a list, a scalar. Fabric renders what it can and
    # falls back to the value; so does this.
    if hasattr(obj, "take") and not hasattr(obj, "columns"):
        return "\n".join(str(r) for r in obj.take(20))
    return str(obj)


def display(obj, summary=False):
    """Render `obj`. Returns None, like Fabric's — it is an output call."""
    print(render(obj, summary=summary), flush=True)


def displayHTML(html):  # noqa: N802 - the documented spelling
    """Emit raw HTML.

    Fabric renders it in the cell. There is nothing here to render into, so the
    markup is emitted as-is: a notebook's output still CONTAINS what it asked to
    show, and a harness reading that output can assert on it. Swallowing it
    would make the call look successful and produce nothing.
    """
    print(html, flush=True)
