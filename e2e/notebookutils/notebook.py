"""The "notebook": ordinary Fabric notebook code that imports notebookutils and
uses fs / credentials / lakehouse / runtime against the emulator family — the
exact surface a data engineer writes, running unchanged locally.

Its wiring (endpoints, identity, default lakehouse) comes entirely from the
environment the orchestrator injects, the same way real Fabric injects the
runtime context into the kernel.
"""
import json

import notebookutils
from notebookutils import (
    credentials,
    fs,
    lakehouse,
    runtime,
    udf,
    variableLibrary,
)
from notebookutils.common.exceptions import RunMultipleFailedException

ctx = runtime.context
ws = ctx["currentWorkspaceId"]
print(f"runtime.context: workspace={ws} lakehouse={ctx['defaultLakehouseId']}", flush=True)
assert ws and ctx["defaultLakehouseId"], "runtime context not populated"

# --- lakehouse control plane -------------------------------------------------
lakes = lakehouse.list()
names = [l["displayName"] for l in lakes]
print(f"lakehouse.list: {names}", flush=True)
assert "lake" in names, names
got = lakehouse.get(ctx["defaultLakehouseId"])
assert got["displayName"] == "lake", got

# --- credentials: tokens for two audiences -----------------------------------
storage_tok = credentials.getToken("storage")
fabric_tok = credentials.getToken("fabric")
assert storage_tok and fabric_tok and storage_tok != fabric_tok
print("credentials.getToken: storage + fabric tokens minted", flush=True)

# --- fs over OneLake: relative path AND abfss URI -----------------------------
rel = "Files/greeting.txt"
fs.put(rel, "hello from the notebook")
assert fs.exists(rel)
assert fs.head(rel) == "hello from the notebook", fs.head(rel)
fs.append(rel, " — and again")
assert fs.head(rel) == "hello from the notebook — and again", fs.head(rel)
print(f"fs relative round-trip: {fs.head(rel)!r}", flush=True)

uri = f"abfss://{ws}@onelake.dfs.fabric.microsoft.com/{ctx['defaultLakehouseId']}/Files/copy.txt"
fs.cp(rel, uri)
assert fs.read(uri).decode() == fs.head(rel)
listing = [f.name for f in fs.ls(f"abfss://{ws}@onelake.dfs.fabric.microsoft.com/{ctx['defaultLakehouseId']}/Files")]
print(f"fs.ls Files: {sorted(listing)}", flush=True)
assert "greeting.txt" in listing and "copy.txt" in listing, listing

# --- credentials.getSecret: brokered read from Key Vault ---------------------
secret = credentials.getSecret("db-vault", "db-password")
# The ASSERT is the witness, not the print. Echoing the value proved nothing the
# comparison does not, and it put a secret in a public CI log — a fixture one
# here, but this file is what a reader copies when they wire up their own vault.
assert secret == "s3cr3t-value", f"unexpected secret, {len(secret)} chars"
print(f"credentials.getSecret: matched the expected value ({len(secret)} chars)", flush=True)

# =============================================================================
# Phase 3 (docs/56): BEHAVIOUR, not shape.
#
# Contract 2 proves these members exist with the documented signatures. That
# says nothing about what they DO — its own heading is "the API shape is the
# contract, independent of behaviour". Everything below executes through the
# real path and confirms OUT OF BAND: the component that acted is never the one
# that answers.
#
# Ordered by blast radius, as docs/56 says: fs and credentials touch storage and
# identity, where a wrong answer is silent and expensive.
# =============================================================================

# --- fs.getProperties: metadata from the storage layer, not from us ----------
props = fs.getProperties(rel)
assert props, "getProperties returned nothing"
# Content-Length is the file's real size at rest. Compared against the bytes we
# wrote, read back through a DIFFERENT call — the properties response is not
# allowed to be the only witness to its own claim.
written = fs.read(rel)
assert int(props.get("Content-Length", -1)) == len(written), (props, len(written))
print(f"fs.getProperties: Content-Length matches the {len(written)} bytes at rest",
      flush=True)

# --- fs.mv: the source must be GONE, and that is the half that gets missed ---
src = "Files/move-me.txt"
dst = "Files/moved/here.txt"
fs.put(src, "moving")
fs.mv(src, dst)
assert not fs.exists(src), "mv left the source behind — that is a copy, not a move"
assert fs.head(dst) == "moving", fs.head(dst)
# create_path defaults True on the Python surface, so the parent was made.
listed = [f.name for f in fs.ls("Files/moved")]
assert "here.txt" in listed, listed
print("fs.mv: source removed, destination created under a new parent", flush=True)

# --- fs.mv: refusing to clobber is behaviour, not decoration -----------------
fs.put("Files/keep.txt", "original")
fs.put("Files/other.txt", "replacement")
try:
    fs.mv("Files/other.txt", "Files/keep.txt")
    raise AssertionError("mv overwrote a destination without being asked to")
except FileExistsError:
    pass
assert fs.head("Files/keep.txt") == "original", "the refusal must leave the target intact"
assert fs.exists("Files/other.txt"), "a refused move must not consume the source"
print("fs.mv: refused to clobber, and left both sides untouched", flush=True)

# --- fs.cp / fastcp: recursion is real, and the defaults differ --------------
fs.put("Files/tree/a.txt", "a")
fs.put("Files/tree/deep/b.txt", "b")
fs.fastcp("Files/tree", "Files/tree-copy")   # recurse defaults True here
assert fs.head("Files/tree-copy/a.txt") == "a"
assert fs.head("Files/tree-copy/deep/b.txt") == "b", "fastcp did not recurse by default"
print("fs.fastcp: recursed by default and copied the whole tree", flush=True)

# --- fs.mount: a LOCAL path that reaches remote data -------------------------
# The point of a mount is that code expecting a filesystem works unchanged, so
# the proof is a plain `open()` — not a notebookutils call.
fs.mount(f"abfss://{ws}@onelake.dfs.fabric.microsoft.com/{ctx['defaultLakehouseId']}/Files/tree",
         "/mnt-tree")
local = fs.getMountPath("/mnt-tree")
with open(local + "/a.txt") as fh:
    assert fh.read() == "a", "the mounted path did not reach the data"
assert any(m.mountPoint == "/mnt-tree" for m in fs.mounts()), fs.mounts()
print(f"fs.mount: read through a plain open() at {local}", flush=True)
assert fs.unmount("/mnt-tree") is True
assert not any(m.mountPoint == "/mnt-tree" for m in fs.mounts()), "unmount left the point"
print("fs.unmount: the mount point is gone", flush=True)

# --- lakehouse: update, listTables, delete -----------------------------------
made = lakehouse.create("phase3-lake", "created by the e2e")
assert made["displayName"] == "phase3-lake", made
renamed = lakehouse.update("phase3-lake", "phase3-renamed", "renamed by the e2e")
# OUT OF BAND: the rename is confirmed by a fresh list, not by the response of
# the call that performed it.
after = [x["displayName"] for x in lakehouse.list()]
assert "phase3-renamed" in after and "phase3-lake" not in after, after
print(f"lakehouse.update: rename visible in a fresh listing ({renamed['displayName']})",
      flush=True)

# listTables against an EMPTY lakehouse proves nothing: it cannot tell "works,
# and there are none" from "always returns empty". So a table folder is created
# first, and the assertion is that this one is found — the same non-vacuity rule
# contract 6 applies to its recognised statement.
tbl_dir = (f"abfss://{ws}@onelake.dfs.fabric.microsoft.com/"
           f"{ctx['defaultLakehouseId']}/Tables/e2e_events")
fs.mkdirs(tbl_dir)
fs.put(tbl_dir + "/_delta_log/00000000000000000000.json", '{"commitInfo":{}}')
tables = lakehouse.listTables(ctx["defaultLakehouseId"])
names_seen = [t.name for t in tables]
print(f"lakehouse.listTables: {names_seen}", flush=True)
assert "e2e_events" in names_seen, names_seen
# A loose FILE under Tables/ is not a table, and a listing that counted it
# would report folders and files alike.
fs.put(f"abfss://{ws}@onelake.dfs.fabric.microsoft.com/"
       f"{ctx['defaultLakehouseId']}/Tables/stray.txt", "not a table")
assert "stray.txt" not in [t.name for t in
                           lakehouse.listTables(ctx["defaultLakehouseId"])]
print("lakehouse.listTables: found the table, ignored a loose file", flush=True)

assert lakehouse.delete("phase3-renamed") is True
remaining = [x["displayName"] for x in lakehouse.list()]
assert "phase3-renamed" not in remaining, remaining
print("lakehouse.delete: gone from a fresh listing", flush=True)

# --- lakehouse.loadTable: the refusal is behaviour too -----------------------
try:
    lakehouse.loadTable({"relativePath": "Files/a.csv"}, "t")
    raise AssertionError("loadTable pretended to load something")
except NotImplementedError as e:
    assert "saveAsTable" in str(e), e
    print("lakehouse.loadTable: refused by name, and named the way through", flush=True)

# --- credentials: putSecret -> getSecret, read back by a separate call -------
credentials.putSecret("db-vault", "e2e-written", "written-by-the-e2e")
assert credentials.getSecret("db-vault", "e2e-written") == "written-by-the-e2e"
print("credentials.putSecret: round-tripped through a separate read", flush=True)

# --- credentials.isValidToken: on a REAL minted token ------------------------
assert credentials.isValidToken(storage_tok) is True, "a fresh token read as invalid"
assert credentials.isValidToken("not-a-jwt") is False
print("credentials.isValidToken: true for a freshly minted token", flush=True)

# --- notebook.run: schedule a child notebook through the control plane -------
# `run` returns the child's EXIT VALUE, per Fabric's reference — not the job
# status, which is what this asserted until the contract was checked. child-nb
# carries a markdown-only definition, so it never calls exit() and the
# documented answer for that case is the empty string.
exit_value = notebookutils.notebook.run("child-nb")
print(f"notebook.run(child-nb): {exit_value!r}", flush=True)
assert exit_value == "", exit_value

# --- runMultiple: a DAG through the real control plane -----------------------
# The children are markdown-only, so no Spark engine is needed to prove what is
# under test here: dependency ORDER, the RESULT SHAPE, and the failure
# contract. Cell execution is another suite's job.
results = notebookutils.notebook.runMultiple({"activities": [
    {"name": "second", "path": "dag-b", "dependencies": ["first"]},
    {"name": "first", "path": "dag-a"},
]})
print(f"runMultiple: {sorted(results)}", flush=True)
assert set(results) == {"first", "second"}, results
# Fabric's two documented keys, on every activity.
for name, r in results.items():
    assert "exitVal" in r and "exception" in r, (name, r)
    assert r["exception"] is None, (name, r)
    # No child called exit(), and the documented answer for that is "" — not
    # None, which is what a caller doing `int(v or 0)` would trip over.
    assert r["exitVal"] == "", (name, r)

# validateDAG refuses a cycle without running anything.
try:
    notebookutils.notebook.validateDAG({"activities": [
        {"name": "a", "dependencies": ["b"]},
        {"name": "b", "dependencies": ["a"]},
    ]})
    raise AssertionError("validateDAG accepted a cycle")
except notebookutils.notebook.NotebookError as e:
    assert "cycle" in str(e), e
    print(f"validateDAG refused a cycle: {e}", flush=True)

# A failing activity RAISES, and the partial results ride on the exception —
# Fabric's documented way to read a partly-failed DAG.
try:
    notebookutils.notebook.runMultiple({"activities": [
        {"name": "ghost", "path": "no-such-notebook"},
        {"name": "after", "path": "dag-b", "dependencies": ["ghost"]},
    ]})
    raise AssertionError("a failing DAG returned instead of raising")
except RunMultipleFailedException as ex:
    assert ex.result["ghost"]["exception"] is not None, ex.result
    # The dependent did not run, and says why.
    assert ex.result["after"]["status"] == "Skipped", ex.result
    print(f"runMultiple raised, partial results kept: {sorted(ex.result)}", flush=True)

# --- the reference-run lakehouse rule ----------------------------------------
# A referenced child bound to a DIFFERENT default lakehouse than its parent is
# blocked by Fabric. The emulator ran it happily until this was enforced, so a
# mis-bound child passed here and was refused in production.
try:
    notebookutils.notebook.run("other-lake-nb")
    raise AssertionError("a child on a different lakehouse was allowed to run")
except notebookutils.notebook.NotebookError as e:
    assert "lakehouse" in str(e).lower(), e
    print(f"reference run blocked as Fabric does: {e}", flush=True)

# ...and the documented bypass lets it through. Without this half the guard
# could be refusing everything and the assertion above would still pass.
bypassed = notebookutils.notebook.run(
    "other-lake-nb", 90, {"useRootDefaultLakehouse": True})
print(f"useRootDefaultLakehouse bypassed the check: {bypassed!r}", flush=True)
assert bypassed == "", bypassed

# =============================================================================
# Axis B: the remaining members, exercised rather than merely shaped.
#
# Contract 2 has always proven these EXIST with the documented signatures. The
# sections below are the other half — what they DO — and each confirms it OUT
# OF BAND, the same rule the phase 3 block above follows: the call that acted
# is never the call that answers.
# =============================================================================

# --- fs.rm: gone, and recursion is not the default ---------------------------
fs.put("Files/doomed.txt", "delete me")
assert fs.exists("Files/doomed.txt")
assert fs.rm("Files/doomed.txt") is True
assert not fs.exists("Files/doomed.txt"), "rm reported success and left the file"
# A non-empty DIRECTORY without recurse must be refused. Without this half, an
# rm that quietly deleted trees would pass every assertion above.
fs.put("Files/keepdir/inner.txt", "still here")
try:
    fs.rm("Files/keepdir")
    raise AssertionError("rm removed a non-empty directory without recurse=True")
except (OSError, ValueError) as e:
    assert "recurse" in str(e).lower(), e
assert fs.exists("Files/keepdir/inner.txt"), "a refused rm must leave the tree intact"
assert fs.rm("Files/keepdir", recurse=True) is True
assert not fs.exists("Files/keepdir/inner.txt"), "recursive rm left the child"
print("fs.rm: removed a file, refused a non-empty tree, then recursed", flush=True)

# --- lakehouse.getWithProperties: MORE than get, not the same answer ---------
plain = lakehouse.get(ctx["defaultLakehouseId"])
withprops = lakehouse.getWithProperties(ctx["defaultLakehouseId"])
assert withprops["id"] == plain["id"], (withprops, plain)
props2 = withprops.get("properties") or {}
assert props2, "getWithProperties returned nothing get() does not already give"
# The OneLake paths are what the properties are FOR, and they must point at
# this lakehouse rather than being any non-empty dict.
blob = json.dumps(props2)
assert ctx["defaultLakehouseId"] in blob, props2
print(f"lakehouse.getWithProperties: carries {sorted(props2)}", flush=True)

# --- notebook item CRUD, end to end ------------------------------------------
IPYNB = {"cells": [{"cell_type": "code", "source": ["print('v1')\n"],
                    "metadata": {}, "outputs": [], "execution_count": None}],
         "metadata": {}, "nbformat": 4, "nbformat_minor": 5}

made_nb = notebookutils.notebook.create("axis-b-nb", "created by the e2e", IPYNB)
assert made_nb.id, made_nb
# OUT OF BAND: a fresh list, not the create response.
assert "axis-b-nb" in [n.displayName for n in notebookutils.notebook.list()], \
    [n.displayName for n in notebookutils.notebook.list()]
print(f"notebook.create + list: {made_nb.displayName} is in the workspace", flush=True)

# get by NAME and by ID must resolve to the same item — Fabric's management
# APIs take either, and a shim that only handled one would strand a caller
# holding the id create() just returned.
assert notebookutils.notebook.get("axis-b-nb").id == made_nb.id
assert notebookutils.notebook.get(made_nb.id).displayName == "axis-b-nb"
print("notebook.get: same item by display name and by id", flush=True)

fetched = notebookutils.notebook.getDefinition("axis-b-nb")
assert "print('v1')" in fetched, fetched[:200]
print("notebook.getDefinition: the .ipynb came back with its cell", flush=True)

IPYNB["cells"][0]["source"] = ["print('v2')\n"]
assert notebookutils.notebook.updateDefinition("axis-b-nb", IPYNB) is True
after_def = notebookutils.notebook.getDefinition("axis-b-nb")
assert "print('v2')" in after_def and "print('v1')" not in after_def, after_def[:200]
print("notebook.updateDefinition: v1 replaced by v2, read back separately", flush=True)

renamed_nb = notebookutils.notebook.update("axis-b-nb", "axis-b-renamed", "renamed")
assert renamed_nb.displayName == "axis-b-renamed", renamed_nb
listed_nb = [n.displayName for n in notebookutils.notebook.list()]
assert "axis-b-renamed" in listed_nb and "axis-b-nb" not in listed_nb, listed_nb
print("notebook.update: rename visible in a fresh listing", flush=True)

assert notebookutils.notebook.delete("axis-b-renamed") is True
assert "axis-b-renamed" not in [n.displayName for n in notebookutils.notebook.list()]
print("notebook.delete: gone from a fresh listing", flush=True)

# --- variableLibrary: the ACTIVE value set decides ---------------------------
lib = variableLibrary.getLibrary("app-config")
# `endpoint` is overridden by the active set; `retries` is not. Both halves are
# the test — one proves the override applied, the other proves the defaults
# were not simply discarded.
assert lib.endpoint == "https://prod.example", lib.asDict()
assert lib.retries == 3, lib.asDict()
assert lib.getVariable("endpoint") == lib["endpoint"] == lib.endpoint
print(f"variableLibrary.getLibrary: active set won for endpoint, "
      f"default kept for retries ({lib.asDict()})", flush=True)

assert variableLibrary.get("$(/**/app-config/endpoint)") == "https://prod.example"
# Names are case-sensitive here, unlike the pipeline surface. A shim that
# smoothed that over would pass locally and fail on Fabric.
try:
    variableLibrary.get("$(/**/app-config/Endpoint)")
    raise AssertionError("a wrong-case variable name resolved")
except variableLibrary.VariableLibraryError as e:
    assert "case-sensitive" in str(e), e
# The `/**/` prefix is required by the reference.
try:
    variableLibrary.get("$(app-config/endpoint)")
    raise AssertionError("a reference without the /**/ prefix was accepted")
except variableLibrary.VariableLibraryError as e:
    assert "/**/" in str(e), e
print("variableLibrary.get: resolved by reference, and held both documented rules",
      flush=True)

# --- udf.getFunctions: the item's OWN code, actually run ---------------------
funcs = udf.getFunctions("pricing-udf")
detail = [d for d in funcs.functionDetails if d["Name"] == "add_tax"]
assert detail, funcs.functionDetails
assert [p["name"] for p in detail[0]["Parameters"]] == ["amount", "rate"], detail[0]
assert funcs.itemDetails["Name"] == "pricing-udf", funcs.itemDetails
# Keyword and positional must agree, and the answer must come from the seeded
# function_app.py rather than from anything this file computes.
assert funcs.add_tax(amount=100.0, rate=0.1) == 110.0
assert funcs.add_tax(100.0, 0.1) == 110.0, "positional args did not match declared order"
try:
    offered = funcs.no_such_function
    raise AssertionError(f"an undeclared function was offered as callable: {offered}")
except AttributeError as e:
    assert "add_tax" in str(e), e
print("udf.getFunctions: ran the item's real code by keyword and by position",
      flush=True)

# notebookutils.notebook.exit is the documented way a notebook returns a value.
try:
    notebookutils.notebook.exit("PASS")
except notebookutils.notebook._Exit as e:
    print(f"NOTEBOOKUTILS E2E: {e.value}", flush=True)
