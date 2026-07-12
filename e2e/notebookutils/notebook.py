"""The "notebook": ordinary Fabric notebook code that imports notebookutils and
uses fs / credentials / lakehouse / runtime against the emulator family — the
exact surface a data engineer writes, running unchanged locally.

Its wiring (endpoints, identity, default lakehouse) comes entirely from the
environment the orchestrator injects, the same way real Fabric injects the
runtime context into the kernel.
"""
import notebookutils
from notebookutils import fs, credentials, lakehouse, runtime

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
status = notebookutils.notebook.run("child-nb")
print(f"notebook.run(child-nb): {status}", flush=True)
assert status == "Completed", status

# notebookutils.notebook.exit is the documented way a notebook returns a value.
try:
    notebookutils.notebook.exit("PASS")
except notebookutils.notebook._Exit as e:
    print(f"NOTEBOOKUTILS E2E: {e.value}", flush=True)
