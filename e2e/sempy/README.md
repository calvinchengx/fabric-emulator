# e2e/sempy — Microsoft's SemPy, driven against a listener we control

`e2e/xmla` drives a hand-written ADOMD.NET program. This drives **`sempy`**, the
library Fabric notebooks and `semantic-link-labs` actually use.

That difference is the point. A real client resolves a dataset name it does not
already know, so it issues

    POST /powerbi/databases/v201606/workspaces/{ws}/getDatabaseName
    {"datasetName": "...", "workspaceType": 1}

which appears **nowhere** in `e2e/xmla`, because a probe connects with a name it
was handed. Several months of this project's XMLA reasoning came from reading
sources; every correction came from running a client.

## What it pins

- sempy is **redirectable standalone** — no Fabric runtime, capacity or notebook
- `evaluate_dax` is **XMLA** and never touches `executeQueries`
- the metadata path is **two mechanisms in sequence**: `Discover`
  `DISCOVER_XML_METADATA` (`ObjectExpansion=ExpandObject`), then `Execute` of
  `$SYSTEM.TMSCHEMA_*` DMV queries
- nine transport gates, asserted individually so a regression names the gate

## Running

    python3 e2e/sempy/run.py          # skips politely without docker
    SEMPY_REQUIRE=1 python3 ...       # CI: every skip becomes a failure

**It cannot run natively on macOS arm64.** pythonnet finds no .NET runtime and
`Microsoft.Fabric.SemanticLink.XmlaTools` will not load, so the driver runs in a
`linux/amd64` container. The first run builds the image; later runs reuse it.

## What it does NOT establish

What the `TMSCHEMA_*` rowsets must CONTAIN. The stub answers the `Discover` and
stops at the first DMV query — that is the `internal/semanticmodel` half, and
another session's claim.
