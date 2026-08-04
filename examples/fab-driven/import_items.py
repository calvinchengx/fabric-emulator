"""Upload real item DEFINITIONS with `fab import` — the step that makes this
example worth having.

`e2e/fabric-cli` creates items with `fab mkdir` and they are EMPTY: a Notebook
with no cells, a DataPipeline with no activities. Nothing can be run. `fab
import` uploads the definition itself, and from there the items are executable —
which is what run.py goes on to do.

WHY THERE IS A RENDER STEP. A pipeline definition names the workspace and
lakehouse it reads and writes by GUID, and those GUIDs do not exist until
provision.py has run. So `definitions/` holds templates and `build/` holds what
is actually imported. This is not a workaround for the emulator: Microsoft's own
fabric-cicd carries a `find_replace` parameter file for exactly this, because
the same definition has to land in dev, test, and prod with different ids.
"""
import pathlib
import shutil

import fabctl as fab

HERE = pathlib.Path(__file__).resolve().parent
SRC = HERE / "definitions"
BUILD = HERE / "build"

SUBSTITUTIONS = {
    "{{WORKSPACE_ID}}": fab.item_id(fab.WORKSPACE),
    "{{LAKEHOUSE_ID}}": fab.item_id(fab.LAKEHOUSE),
}

ITEMS = [
    ("bronze-ingest.DataPipeline", fab.BRONZE_PIPELINE),
    ("silver.Notebook", fab.SILVER_NOTEBOOK),
]


def render(folder: str) -> pathlib.Path:
    """Copy one definition folder into build/, substituting the ids."""
    src, dst = SRC / folder, BUILD / folder
    if dst.exists():
        shutil.rmtree(dst)
    shutil.copytree(src, dst)
    for path in dst.rglob("*"):
        if not path.is_file():
            continue
        text = path.read_text()
        rendered = text
        for token, value in SUBSTITUTIONS.items():
            rendered = rendered.replace(token, value)
        if rendered != text:
            path.write_text(rendered)
    # A template whose placeholders survived would import cleanly and fail much
    # later, as a Spark error about a workspace named {{WORKSPACE_ID}}.
    for path in dst.rglob("*"):
        if path.is_file() and "{{" in path.read_text():
            raise AssertionError(f"unsubstituted placeholder left in {path}")
    return dst


for folder, item in ITEMS:
    render(folder)
    fab.run("import", item, "-i", f"/work/build/{folder}", "-f")
    assert fab.exists(item), f"{item} imported but does not exist"
    fab.log(f"imported {folder} -> {item}")

# Round-trip the notebook back out and diff it. `fab import` reporting success
# says the request was accepted; only reading the bytes back says the definition
# STORED is the definition sent — and that is the claim this example makes about
# the emulator, not about fab.
EXPORTED = BUILD / "exported"
if EXPORTED.exists():
    shutil.rmtree(EXPORTED)
EXPORTED.mkdir(parents=True)
fab.run("export", fab.SILVER_NOTEBOOK, "-o", "/work/build/exported", "-f")

sent = (BUILD / "silver.Notebook" / "notebook-content.py").read_bytes()
back = next(EXPORTED.rglob("notebook-content.py")).read_bytes()
assert sent == back, (
    f"the notebook came back changed: sent {len(sent)} bytes, "
    f"got {len(back)} back"
)
fab.log(f"exported it back: {len(back):,} bytes, byte-identical")
