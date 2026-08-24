#!/usr/bin/env python3
"""`display()` through a real Jupyter kernel, asserted on the notebook's outputs.

WHY THIS HARNESS EXISTS. Every other notebook suite here reads stdout, so the
strongest claim any of them can make about `display` is "some text came out".
Fabric's `display` produces a RICH OUTPUT that a front end renders, and the
difference between publishing a MIME bundle and printing a string is invisible
to a stdout reader — which is exactly how this row sat at "text, no evidence"
while the repository shipped a real JupyterLab against the same stack.

nbclient executes the notebook against the kernel from the SAME image
`make up-jupyter` gives a human, so the startup file that binds Fabric's
builtins is under test too.

WHAT THIS DOES NOT PROVE, stated here rather than left to be assumed: Fabric's
`display` is a proprietary widget with chart views, sorting and an inspect
panel. A correct `text/html` table is not that widget and nothing local can
show it is. This proves the data, its shape, and that it is published in the
form a front end renders — not equivalence with Fabric's UI.
"""
import sys

import nbformat
from nbclient import NotebookClient


def cell(source):
    return nbformat.v4.new_code_cell(source)


NB = nbformat.v4.new_notebook(cells=[
    # No import above `display` — on Fabric it is runtime-provided, and a
    # notebook that had to import it would not be a Fabric notebook.
    cell("from pyspark.sql import SparkSession\n"
         "spark = SparkSession.builder.getOrCreate()\n"
         "df = spark.createDataFrame(\n"
         "    [(1, 'north', None), (2, 'south', 'x'), (3, 'north', 'y')],\n"
         "    ['id', 'region', 'note'])\n"
         "print('frame ready')"),
    cell("display(df)"),
    cell("display(df, summary=True)"),
    cell("displayHTML('<b>from displayHTML</b>')"),
    # A column holding markup must render as CHARACTERS. Cell data is
    # arbitrary, and a table that injected it would be a real defect in
    # anything that renders this output.
    cell("display(spark.createDataFrame([('<script>x</script>',)], ['danger']))"),
])


def outputs(nb, index):
    return nb.cells[index].outputs


def rich(nb, index):
    """The display_data MIME bundle a cell published, or None."""
    for out in outputs(nb, index):
        if out.get("output_type") in ("display_data", "execute_result"):
            return out.get("data") or {}
    return None


def fail(message):
    print(f"NOTEBOOK-DISPLAY E2E: FAIL — {message}", flush=True)
    sys.exit(1)


print("==> executing the notebook against a real kernel", flush=True)
client = NotebookClient(NB, timeout=600, kernel_name="python3",
                        allow_errors=True, resources={"metadata": {"path": "/"}})
nb = client.execute()

for i, c in enumerate(nb.cells):
    for out in c.outputs:
        if out.get("output_type") == "error":
            fail(f"cell {i} raised {out.get('ename')}: {out.get('evalue')}")

# --- display(df): a MIME bundle, not a print ---------------------------------
# THE BUNDLE IS THE ASSERTION. A `print` lands as a `stream` output and would
# fail here — which is the whole difference this harness exists to see.
bundle = rich(nb, 1)
if bundle is None:
    fail("display(df) published no rich output — it printed, or did nothing: "
         + repr(outputs(nb, 1)))
if "text/html" not in bundle:
    fail(f"display(df) published no text/html: {sorted(bundle)}")
html = bundle["text/html"]
for column in ("id", "region", "note"):
    if f"<th>{column}</th>" not in html:
        fail(f"the rendered table is missing column {column!r}: {html}")
if "south" not in html or "north" not in html:
    fail(f"the rendered table is missing its rows: {html}")
# text/plain rides along, so a front end with no HTML still shows the data —
# and it must agree with the HTML rather than being a second rendering.
if "text/plain" not in bundle or "south" not in bundle["text/plain"]:
    fail(f"the plain-text alternative disagrees or is absent: {bundle.get('text/plain')!r}")
print("==> display(df): text/html table with its columns and rows, "
      "text/plain alongside", flush=True)

# --- summary=True: Fabric's four documented fields ---------------------------
summary = (rich(nb, 2) or {}).get("text/html", "")
for field in ("column", "type", "unique", "missing"):
    if f"<th>{field}</th>" not in summary:
        fail(f"summary is missing the {field!r} column: {summary}")
# `note` is null in one of three rows. A summary that reported 0 missing would
# be shaped right and wrong — so the NUMBER is asserted, not just the header.
if "<td>1</td>" not in summary:
    fail(f"summary did not count the missing value: {summary}")
print("==> display(df, summary=True): name, type, unique and missing, counted",
      flush=True)

# --- displayHTML: rendered as html, not shown as text ------------------------
emitted = (rich(nb, 3) or {}).get("text/html", "")
if "<b>from displayHTML</b>" not in emitted:
    fail(f"displayHTML did not publish its markup as text/html: {emitted!r}")
print("==> displayHTML: published as text/html", flush=True)

# --- data is data, not markup ------------------------------------------------
danger = (rich(nb, 4) or {}).get("text/html", "")
if "<script>" in danger:
    fail("a data value was injected into the rendered table as live markup")
if "&lt;script&gt;" not in danger:
    fail(f"the data value was not rendered at all: {danger}")
print("==> a column holding markup renders as characters, not tags", flush=True)

print("NOTEBOOK-DISPLAY E2E: PASS", flush=True)
