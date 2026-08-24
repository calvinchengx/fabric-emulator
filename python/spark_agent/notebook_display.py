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


# Fabric documents the summary as these four, per column.
SUMMARY_HEADERS = ("column", "type", "unique", "missing")


def _spark_summary_rows(df):
    """Fabric's documented summary: name, type, unique values, missing values."""
    from pyspark.sql import functions as F  # noqa: N812 - the conventional alias

    types = dict(df.dtypes)
    aggregates = []
    for column in df.columns:
        quoted = F.col(f"`{column}`")
        aggregates.append(F.countDistinct(quoted).alias(f"__uniq__{column}"))
        aggregates.append(F.count(F.when(quoted.isNull(), 1)).alias(f"__null__{column}"))
    row = df.agg(*aggregates).collect()[0] if aggregates else None
    return [[column, types.get(column, ""),
             row[f"__uniq__{column}"] if row is not None else 0,
             row[f"__null__{column}"] if row is not None else 0]
            for column in df.columns]


def _pandas_summary_rows(df):
    return [[column, df[column].dtype, df[column].nunique(),
             int(df[column].isna().sum())]
            for column in df.columns]


def table(obj, summary=False):
    """(headers, rows) when `obj` has a tabular shape, else None.

    ONE description of the data, because `render` and `render_html` both need
    it and two independent renderers drift — which matters here more than
    usual, since the text form is what a RunNotebook job's output is asserted
    on and the HTML form is what a front end shows.
    """
    if _is_spark_frame(obj):
        if summary:
            return list(SUMMARY_HEADERS), _spark_summary_rows(obj)
        # `show` prints; `_jdf.showString` is not available on Connect. Ask the
        # frame for its own rows, which both engines can do.
        return (list(obj.columns),
                [["" if v is None else v for v in r] for r in obj.limit(20).collect()])
    if _is_pandas_frame(obj) and summary:
        return list(SUMMARY_HEADERS), _pandas_summary_rows(obj)
    return None


def _grid(headers, rows):
    lines = [f"{headers[0]:<24} {headers[1]:<14} {headers[2]:>10} {headers[3]:>10}"]
    for row in rows:
        lines.append(f"{row[0]!s:<24} {row[1]!s:<14} {row[2]:>10} {row[3]:>10}")
    return "\n".join(lines)


def render(obj, summary=False):
    """The text `display` would print. Separated from printing so it is
    testable without capturing stdout."""
    shape = table(obj, summary=summary)
    if shape is not None:
        headers, rows = shape
        if summary:
            return _grid(headers, rows)
        header = " | ".join(headers)
        body = "\n".join(" | ".join(str(v) for v in row) for row in rows)
        return header + "\n" + body if body else header
    if _is_pandas_frame(obj):
        return obj.to_string()
    # Anything else: an RDD, a list, a scalar. Fabric renders what it can and
    # falls back to the value; so does this.
    if hasattr(obj, "take") and not hasattr(obj, "columns"):
        return "\n".join(str(r) for r in obj.take(20))
    return str(obj)


# --- rich output -------------------------------------------------------------
#
# "There is nothing here to render into" was true of the AGENT and false of the
# repository: the `jupyter` compose profile ships a real JupyterLab against this
# same stack. Under a kernel there IS somewhere to render, and printing text
# into it throws away the one thing a notebook front end can use.
#
# So the same call publishes a MIME BUNDLE when a kernel is present and prints
# when it is not. Both paths render from ONE description of the data (`table`),
# because two renderers drift and the text form is what the agent's RunNotebook
# output is asserted on.
#
# WHAT THIS STILL IS NOT. Fabric's `display` is a proprietary widget with chart
# views, sorting and an inspect panel. A correct `text/html` table is not that
# widget, and no local front end can prove it is. This narrows the gap from
# "text only" to "the data and its shape, in the form a front end renders" —
# and the interactivity remains a stated divergence, not a solved one.


def _kernel():
    """The running IPython shell, or None. Never raises, never imports Spark."""
    try:
        from IPython import get_ipython
    except Exception:  # noqa: BLE001 - no IPython is the ordinary agent case
        return None
    try:
        return get_ipython()
    except Exception:  # noqa: BLE001
        return None


def _publisher():
    """IPython's display publisher, or None.

    Separated from `_publish` so a test can supply one. The agent's own
    environment has no IPython at all, so a test that patched only `_kernel`
    would exercise this import failing rather than the path it meant to — and
    would pass for the wrong reason.
    """
    try:
        from IPython.display import publish_display_data
    except Exception:  # noqa: BLE001 - no IPython is the ordinary agent case
        return None
    return publish_display_data


def _publish(bundle):
    """Publish a MIME bundle through the kernel. True when it went out."""
    if _kernel() is None:
        return False
    publisher = _publisher()
    if publisher is None:
        return False
    try:
        publisher(bundle)
    except Exception:  # noqa: BLE001 - a front end that refuses is not a reason
        return False                     # to lose the output; fall back to text
    return True


def _escape(value):
    """Text into HTML. Cell data is arbitrary — a column holding `<script>` must
    render as characters, not run."""
    return (str(value).replace("&", "&amp;").replace("<", "&lt;")
            .replace(">", "&gt;").replace('"', "&quot;"))


def render_html(obj, summary=False):
    """The same content as `render`, as an HTML table where there is one."""
    shape = table(obj, summary=summary)
    if shape is None:
        # pandas renders itself, and better than a generic grid would.
        if _is_pandas_frame(obj) and hasattr(obj, "to_html"):
            return obj.to_html()
        return "<pre>" + _escape(render(obj, summary=summary)) + "</pre>"
    headers, rows = shape
    head = "".join(f"<th>{_escape(h)}</th>" for h in headers)
    body = "".join(
        "<tr>" + "".join(f"<td>{_escape(c)}</td>" for c in row) + "</tr>"
        for row in rows)
    return (f'<table class="fabric-display"><thead><tr>{head}</tr></thead>'
            f"<tbody>{body}</tbody></table>")


def display(obj, summary=False):
    """Render `obj`. Returns None, like Fabric's — it is an output call."""
    text = render(obj, summary=summary)
    if _publish({"text/html": render_html(obj, summary=summary),
                 "text/plain": text}):
        return
    print(text, flush=True)


def displayHTML(html):  # noqa: N802 - the documented spelling
    """Emit raw HTML.

    Under a kernel this is published as `text/html` and really is rendered.
    Without one the markup is emitted as-is: a notebook's output still CONTAINS
    what it asked to show, and a harness reading that output can assert on it.
    Swallowing it would make the call look successful and produce nothing.
    """
    if _publish({"text/html": html, "text/plain": html}):
        return
    print(html, flush=True)
