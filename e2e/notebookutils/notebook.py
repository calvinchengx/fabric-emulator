"""The "notebook": ordinary Fabric notebook code that imports notebookutils and
uses fs / credentials / lakehouse / runtime against the emulator family — the
exact surface a data engineer writes, running unchanged locally.

Its wiring (endpoints, identity, default lakehouse) comes entirely from the
environment the orchestrator injects, the same way real Fabric injects the
runtime context into the kernel.
"""
import notebookutils
from notebookutils import credentials, fs, lakehouse, runtime
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
print(f"credentials.getSecret: {secret!r}", flush=True)
assert secret == "s3cr3t-value", secret

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

# notebookutils.notebook.exit is the documented way a notebook returns a value.
try:
    notebookutils.notebook.exit("PASS")
except notebookutils.notebook._Exit as e:
    print(f"NOTEBOOKUTILS E2E: {e.value}", flush=True)
