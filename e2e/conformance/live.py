"""Live contract-4 write-landing for sail and jvm.

writer() publishes a notebook, submits RunNotebook, and polls. Its claim is
the job status — Completed or not. It does not list OneLake and it does not
ask spark.table. Those would be the writer confirming its own write, which
is every false green in docs/38 §4.

reader() mints a fresh Storage-audience token and lists
`lake.Lakehouse/Tables/events` over DFS HTTP. That process is not Spark and
never submitted the job.
"""
from __future__ import annotations

import base64
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(DIR))

# The cited surface (Phase 0, docs/56). Loaded here rather than at the call
# site because the notebook body needs the MODULE LIST substituted into it:
# the names a session introspects have to come from Microsoft's own overview
# table, not from a list in this file, or the probe rebuilds the blind spot
# the citation removed.
REFERENCE = json.loads(
    (Path(__file__).resolve().parent / "notebookutils-reference.json")
    .read_text(encoding="utf-8"))
REFERENCE_MODULES = {m.split(".")[-1]: methods
                     for m, methods in REFERENCE["modules"].items()}
from probes import (  # noqa: E402
    CONTROL,
    Artifact,
    ContextClaim,
    CredentialClaim,
    FallThroughClaim,
    IsolationClaim,
    RuntimeClaim,
    SignatureClaim,
    WriteClaim,
    concurrent_isolation,
    context_chain,
    credential_lifetime,
    fall_through,
    record,
    runtime_floor,
    signature_shape,
    write_landing,
)

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://api.fabric.microsoft.com"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
# THE DFS SURFACE, ADDRESSED BY CONTAINER AND ROUTED BY HOST HEADER.
#
# The emulator decides OneLake-vs-control-plane from the Host header, so the
# hostname in the URL is not load-bearing — and it must not be, because what
# that alias points at is not settled. A TLS terminator was put behind it on
# the jvm leg (`abfss://` forces TLS, so hadoop cannot reach a plaintext
# stack), and pointing these plain reads at the alias broke every one of them
# at once: contract 1 reported `<urlopen error>`, and 2, 3, 5 and 6 reported
# "the session did not get as far as..." — one unreachable reader wearing five
# different failures.
#
# THE TERMINATOR IS NOT IN THE TREE — it regressed contract 4 and was reverted,
# see docs/38 §6. This decoupling stays regardless: it is what let the
# terminator be tried at all without taking five cells down with it, and it is
# what will let a working one land. A reader that had to be re-pointed every
# time the alias moved would make each attempt cost five false reds.
#
# This process is still an out-of-band reader: a different container, a token it
# minted itself, and no Spark. Which hostname it dials is not part of that claim.
ACCT = "fabric-emulator"
DFS_HOST = "onelake.dfs.fabric.microsoft.com"
TABLE_DIR = "lake.Lakehouse/Tables/events"
# Contract 1's findings, as a plain JSON file rather than a Delta table: the
# out-of-band reader is then a single DFS GET with no Parquet parser, and the
# file is what a Fabric notebook can write with one documented call.
FINDINGS_PATH = "lake.Lakehouse/Files/conformance/context.json"
# Contract 5's children write one file each, keyed by the marker they were
# given, so a child that wrote another child's file is visible as a mismatch
# rather than as an overwrite nobody can see.
FANOUT_DIR = "lake.Lakehouse/Files/conformance/fanout"
FANOUT_N = 3

# One cell, parameterised by the marker the harness bakes in. Its whole job is
# to say WHICH notebook it believes it is — the identity a shared agent could
# leak — alongside the marker only this child was given.
# The notebook `%run` pulls in. Deliberately more than one code cell, plus a
# markdown cell: `%run` must splice every CODE cell in order and skip the prose.
RUN_HELPER_BODY = """# Fabric notebook source

# MARKDOWN ********************
# MAGIC %md
# MAGIC ## helpers — prose, and must NOT be spliced

# CELL ********************
run_label = "from-helpers"

# CELL ********************
def run_scaled(n):
    return n * run_scale
"""

# --- cell languages -----------------------------------------------------------
#
# The four dispositions, through a REAL RunNotebook job. They already had Go
# tests, but those call `Disposition(language)` directly — and the defect this
# closed never lived there. The parser always classified the magic correctly;
# the RUN LOOP ignored the answer and sent everything that was not `sql` to the
# Python executor. So the bug sat in the gap between the classifier and its
# caller, and a unit test on the classifier passes on both sides of it.
#
# Every cell below is chosen so that EXECUTING IT AS PYTHON FAILS. That is what
# makes this non-vacuous: if the run loop regressed to the old behaviour, these
# notebooks would not quietly pass, they would break.
# --- notebook resources -------------------------------------------------------
#
# `builtin/` is the ROOT notebook's folder, never the running one. The child
# below carries a resource of the SAME NAME with DIFFERENT content, so the file
# it reads says which notebook it resolved against — a negative control rather
# than an assertion that something non-empty came back.
RES_ROOT_BODY = """# Fabric notebook source

# CELL ********************
import notebookutils as _nbu

print("root sees:", open(_nbu.nbResPath + "/data.txt").read())
_nbu.notebook.run("res-child-nb")
"""

RES_CHILD_BODY = """# Fabric notebook source

# CELL ********************
import json as _json

import notebookutils as _nbu

_seen = open(_nbu.nbResPath + "/data.txt").read()
_nbu.fs.put(
    "abfss://__WS__@onelake.dfs.fabric.microsoft.com/__DIR__/resources.json",
    _json.dumps({"seen": _seen}), True)
print("child sees:", _seen)
"""

CELL_LANGUAGES_BODY = """# Fabric notebook source

# CELL ********************
# MAGIC %%configure
# MAGIC {
# MAGIC   "driverMemory": "28g",
# MAGIC   "useStarterPool": false
# MAGIC }

# CELL ********************
# MAGIC %%html
# MAGIC <h1>this is markup, not code</h1>

# CELL ********************
# MAGIC %%markdown
# MAGIC ## a heading
# MAGIC and *emphasis*, which is not Python either

# CELL ********************
import json as _json

import notebookutils as _nbu

_nbu.fs.put(
    "abfss://__WS__@onelake.dfs.fabric.microsoft.com/__DIR__/languages.json",
    _json.dumps({"ran": True}), True)
print("the python cell after the magics ran")
"""

# `useStarterPool: false` is JSON, not Python — `false` is a NameError. The
# `<h1>` cell is a SyntaxError. Neither can pass by being executed.

SCALA_BODY = """# Fabric notebook source

# CELL ********************
# MAGIC %%spark
# MAGIC val rows = spark.range(5).count()
# MAGIC println(s"scala counted $rows")

# CELL ********************
import json as _json

import notebookutils as _nbu

# MUST NEVER RUN. An unsupported cell fails the run and stops it, so this
# marker's ABSENCE is the assertion — a run that carried on past a refused
# cell would leave it behind.
_nbu.fs.put(
    "abfss://__WS__@onelake.dfs.fabric.microsoft.com/__DIR__/after-scala.json",
    _json.dumps({"ran": True}), True)
"""


CHILD_BODY = """# Fabric notebook source

# CELL ********************
import json as _json

try:
    import notebookutils as _nbu

    _ctx = dict(_nbu.runtime.context)
    _nbu.fs.put(
        "abfss://__WS__@onelake.dfs.fabric.microsoft.com/__DIR__/__MARKER__.json",
        _json.dumps({
            "marker": "__MARKER__",
            "identity": _ctx.get("currentNotebookId", ""),
            "workspace": _ctx.get("currentWorkspaceId", ""),
        }), True)
    print("child __MARKER__ wrote its findings")
except Exception as _exc:  # noqa: BLE001
    print("child __MARKER__ NOT written:", type(_exc).__name__, _exc)
"""

# The notebook-driven write path, and only that path: unqualified
# saveAsTable("events"). No CTAS, MERGE, mount, or qualified name — those
# are later contracts. df.count() is the in-memory frame, not a catalog
# read-back; the harness must not confirm through spark.table either.
NOTEBOOK_BODY = """# Fabric notebook source

# CELL ********************
df = spark.createDataFrame([(1, "a"), (2, "b"), (3, "c"), (4, "d")], ["id", "name"])
df.write.format("delta").mode("overwrite").saveAsTable("events")
print("wrote", df.count(), "rows")

# CELL ********************
# Contract 1. Each link of the chain is asked SEPARATELY, in the order a
# framework probes it, and each answer is recorded even when it raises --
# "this link is broken" is the finding, not a reason to abandon the cell.
#
# THE WHOLE CELL IS GUARDED, imports included. Contracts 1 and 4 share one run
# to keep it cheap, and a partial guard is not a guard: the first version
# wrapped only the write, so `import notebookutils` failing on the JVM overlay
# still failed the job and turned contract 4 red -- with its table landed and
# the out-of-band listing seeing five paths. One contract's red must never be
# another's, and the absent artifact is contract 1's failure signal by itself.
import json as _json, os as _os, time

try:
    import notebookutils as _nbu

    def _try(fn):
        try:
            return fn(), ""
        except Exception as exc:  # noqa: BLE001 -- a broken link is a finding
            return "", f"{type(exc).__name__}: {exc}"

    _env_ws, _env_err = _try(lambda: _nbu.mssparkutils.env.getWorkspaceId())
    _ctx, _ctx_err = _try(lambda: dict(_nbu.runtime.context))
    _findings = {
        "env_workspace": _env_ws,
        "context_workspace": (_ctx or {}).get("currentWorkspaceId", ""),
        "context_lakehouse": (_ctx or {}).get("defaultLakehouseId", ""),
        # The fallback the emulator keeps for a kernel with no agent. Reported
        # so the harness can REFUSE a pass that only the fallback earned.
        "fallback_workspace": _os.environ.get("NOTEBOOKUTILS_WORKSPACE_ID", ""),
        "env_fallback_set": bool(_os.environ.get("NOTEBOOKUTILS_WORKSPACE_ID")
                                 or _os.environ.get("NOTEBOOKUTILS_LAKEHOUSE_ID")),
        "errors": [e for e in (_env_err, _ctx_err) if e],
        # Absent in the emulator today; recorded so the gap is visible in the
        # artifact rather than only in prose.
        "context_capacity": (_ctx or {}).get("currentCapacityId", ""),
    }
    # Contract 2, in the same artifact and the same run. Signature shape is a
    # property of the surface as the SESSION sees it, so it is read here rather
    # than from the client's own import -- the client runs a different
    # interpreter with a different package set, and asserting on that would
    # prove something about the harness.
    import importlib as _importlib
    import inspect as _inspect

    # EVERY DOCUMENTED MODULE, and the list comes from the reference rather
    # than from this file. `__MODULES__` is substituted by the client from
    # notebookutils-reference.json, whose module list is Microsoft's own
    # overview table -- so a module nobody here thought of still gets probed.
    # Hardcoding the names would rebuild the exact blind spot Phase 0 removed.
    _sigs = {}
    for _mod in __MODULES__:
        try:
            _m = _importlib.import_module("notebookutils." + _mod)
        except Exception as _mod_err:  # noqa: BLE001
            # A module that will not import is NOT an empty module. Recorded as
            # an explicit error so the probe can say "absent" rather than
            # reading zero members as a surface with nothing in it.
            _sigs[_mod] = {"__import_error__": [str(_mod_err)[:200]]}
            continue
        _members = {}
        for _name in dir(_m):
            if _name.startswith("_"):
                continue
            _fn = getattr(_m, _name, None)
            if _fn is None:
                continue
            if not callable(_fn):
                # A PROPERTY IS PART OF THE SURFACE. `runtime.context` is the
                # whole of its module and is not callable; skipping
                # non-callables would report that module as empty.
                _members[_name] = []
                continue
            try:
                _members[_name] = [p.name for p in
                                   _inspect.signature(_fn).parameters.values()
                                   if p.kind not in (p.VAR_POSITIONAL, p.VAR_KEYWORD)]
            except (TypeError, ValueError):
                # A builtin with no introspectable signature is not "absent";
                # say so rather than letting it read as a missing method.
                _members[_name] = ["<no signature>"]
        _sigs[_mod] = _members
    _findings["module_signatures"] = _sigs
    # Contract 3. Read from the SESSION's own interpreter, because that is the
    # one a notebook's imports resolve against — reading the client's would
    # describe the harness. `spark.version` is recorded, not asserted: whether
    # the engine behaves like Spark 3.5 is the engine matrix's question.
    import sys as _sys

    # Contract 6. Two statements against one small Delta table: OPTIMIZE, which
    # the agent's grammar recognises and routes to delta-rs, and a MERGE with
    # `WHEN MATCHED THEN DELETE`, which it deliberately does NOT intercept. The
    # first proves the interception is installed at all; the second is the
    # contract. Both outcomes are recorded, never asserted here — the harness
    # decides, and it decides differently for the control engine.
    # THE SCHEME IS `abfs://`, AND THAT IS THE WHOLE OF THE JVM STORY.
    #
    # This probe used to hardcode `abfss://`. Hadoop's ABFS driver forces TLS
    # for that scheme — `fs.azure.always.use.https=false` downgrades `abfs://`
    # and nothing else — so against a plaintext stack every request failed at
    # the socket with status 0, hadoop-azure retried forever, and the notebook
    # HUNG: five threads parked in AbfsRestOperation.completeExecute for
    # 324s-864s. That was read as "the JVM overlay cannot reach OneLake by
    # path", and a TLS terminator was built to fix it.
    #
    # It is not what was happening. The JVM overlay reaches OneLake by path on
    # every single run: cell 0's `saveAsTable` commits its Delta log to
    # `abfs://…@onelake.dfs.fabric.microsoft.com/…/Tables/events/_delta_log`,
    # which is what contract 4 confirms out of band. ONE STATEMENT IN THIS FILE
    # was spelling the scheme differently from everything else in the stack —
    # `bindDefaultLakehouse`, `livy_catalog` and `mlv.go` all emit `abfs://`,
    # commented there as "the same string a user would have written".
    #
    # So the fix is to stop being the outlier, not to terminate TLS. Real
    # Fabric serves `abfss://` and `e2e/livy` proves the path form against it;
    # `abfs://` is that same path spelled for a stack that serves plaintext,
    # and it is the spelling this emulator hands every engine.
    #
    # The gate stays. A HANG IS WORSE THAN A RED — these statements share a
    # notebook with contracts 1, 2, 3 and 4, so one hang takes five cells down
    # and reports the harness's own timeout as five separate defects, which
    # happened twice. A backend opts in when its compose says it can answer.
    if _os.environ.get("CONFORMANCE_FALL_THROUGH") == "1":
        # PATH-ADDRESSED, not by catalog name. The delta-rs interception resolves
        # a NAME through the emulator's registration, and a table written by
        # `saveAsTable` inside a notebook is not registered that way — `OPTIMIZE
        # events` fails with `cannot resolve 'events' to a table location: it was
        # not registered through the emulator`. Measured, twice, on this probe's
        # first two runs. The path form is what `e2e/livy` proves against a real
        # OneLake table, so it is the shape a notebook author is told to use.
        #
        # On `events`, the table cell 0 already wrote. The coupling with contract
        # 4 is one-way and harmless: OPTIMIZE compacts and the MERGE deletes at
        # most one row, while contract 4's reader asserts only that OneLake lists
        # paths under the table, which both leave true.
        _tbl = ("abfs://__WS__@onelake.dfs.fabric.microsoft.com"
                "/lake.Lakehouse/Tables/events")
        _rec, _rec_err = _try(lambda: spark.sql(f"OPTIMIZE delta.`{_tbl}`").collect())
        _unrec, _unrec_err = _try(lambda: spark.sql(
            f"MERGE INTO delta.`{_tbl}` t USING (SELECT 1 AS id) s "
            "ON t.id = s.id WHEN MATCHED THEN DELETE").collect())
        # MEASURED, NOT ASSERTED. docs/38 records that `OPTIMIZE <name>`
        # cannot resolve a table a notebook wrote, because the interception
        # resolves a name through the emulator's registration and
        # `saveAsTable` does not register it that way. That was measured on
        # sail only — and the fallback when there is no recorded location is
        # `DESCRIBE DETAIL`, which Sail does not implement and Spark does. So
        # the same statement may well behave differently per engine, and a
        # gap recorded as universal deserves a number from each. Recorded
        # here, graded by nothing: contract 6 is about fall-through, and this
        # is a separate limitation that happens to be cheap to observe from
        # the same cell.
        _name_ok, _name_err = _try(lambda: spark.sql("OPTIMIZE events").collect())
        _findings["fall_through"] = {
            "table_error": "",
            "recognised_ok": not _rec_err,
            "recognised_error": _rec_err,
            "unrecognised_ok": not _unrec_err,
            "unrecognised_error": _unrec_err,
            "name_form_ok": not _name_err,
            "name_form_error": _name_err,
        }
    else:
        _findings["fall_through"] = {"skipped": True}

    # Contract 7. Two OneLake reads with a wait between them longer than the
    # access-token lifetime the stack was configured with.
    #
    # THROUGH THE ENGINE, not the shim: the credential under test is the one the
    # emulator hands the engine (sail's resident launcher, the JVM's
    # EntraTokenProvider), and the shim mints its own. `spark.read` on the table
    # cell 0 wrote exercises exactly that.
    #
    # BY CATALOG NAME, deliberately — this contract is about a credential, and
    # addressing the table by path would drag in the scheme question the
    # fall-through gate above deals with. One contract, one variable.
    _life = int(_os.environ.get("CONFORMANCE_TOKEN_LIFETIME", "0") or 0)
    if _life > 0:
        # THE SECOND OPERATION IS A WRITE TO A FRESH TABLE, NOT A RE-READ.
        #
        # A re-read of `events` failed after the wait with
        # `AnalysisException: Table not found: [TABLE_OR_VIEW_NOT_FOUND]` while
        # the read BEFORE the wait succeeded — the catalog entry a `saveAsTable`
        # created was gone 75 seconds later, in the same session and the same
        # cell. That is worth its own investigation and is NOT what this contract
        # is about: §7 is a token minted at container start and every OneLake
        # operation answering 401 an hour later, not a catalog forgetting a name.
        #
        # A write to a new table needs the engine's OneLake credential and
        # nothing that has to survive the wait, so it measures the contract
        # rather than the mystery. The catalog observation is recorded in
        # docs/38 §7 instead of being routed around silently.
        _b, _b_err = _try(lambda: spark.read.table("events").count())
        _wait = _life + 15
        time.sleep(_wait)
        _a, _a_err = _try(lambda: spark.createDataFrame(
            [(1, "after")], ["id", "v"]).write.format("delta")
            .mode("overwrite").saveAsTable("cred_after"))
        # MEASURED, NOT GRADED. Re-reading the table cell 0 wrote is what
        # originally failed here, and docs/38 §7 now explains why: sail's
        # credential refresh is a process restart, and the restart takes the
        # engine's session catalog with it. The verbatim error is recorded
        # because the agent has to RECOGNISE this failure to report it as a
        # restart rather than let it read as a typo — and detection built on a
        # remembered error message is how the last three of these went wrong.
        _r, _r_err = _try(lambda: spark.read.table("events").count())
        # THE SAME DATA, BY PATH. This is what turns "the engine forgot the
        # table" from an inference into a demonstration: if the catalog read
        # above fails while this succeeds, the bytes are exactly where cell 0
        # put them and it is the ENGINE's session state that went away — which
        # is the condition session_recovery.forgotten_table_in() detects, and
        # the reason its oracle asks the lakehouse rather than the agent's
        # bookkeeping.
        _p, _p_err = _try(lambda: spark.read.format("delta").load(
            "abfs://__WS__@onelake.dfs.fabric.microsoft.com"
            "/lake.Lakehouse/Tables/events").count())
        _findings["credential"] = {
            "reread_ok": not _r_err,
            "reread_error": _r_err,
            "path_read_ok": not _p_err,
            "path_read_error": _p_err,
            "lifetime": _life, "slept": _wait,
            "before_ok": not _b_err, "before_error": _b_err,
            "after_ok": not _a_err, "after_error": _a_err,
        }
    else:
        _findings["credential"] = {"skipped": True}

    # `%run` — an AGENT-SIDE REWRITE, which is why it can only be proven here.
    # The notebookutils e2e runs its notebook as a plain script and
    # e2e/notebook-run's runner is itself the engine (it execs cells locally),
    # so neither ever reaches the agent's run_code where the rewrite happens.
    # This stack does: the emulator drives the real spark-agent.
    #
    # The proof is that what `helpers` defines is usable HERE, in this
    # namespace — that is the whole difference from notebook.run, which starts
    # a separate session and hands back an exit value.
    # A REAL LINE, not exec() of a string: the rewrite is applied by the agent
    # to the CELL SOURCE, so a `%run` hidden inside a string literal is exactly
    # what run_magic refuses to touch (and has a test saying so). Indented,
    # because this cell body is inside a try — which the rewrite preserves.
    _run_ok, _run_err = False, ""
    try:
        %run run-helpers {"run_scale": 10}
        _run_ok = (run_scaled(4) == 40 and run_label == "from-helpers")  # noqa: F821
    except Exception as _exc:  # noqa: BLE001
        _run_err = f"{type(_exc).__name__}: {_exc}"
    _findings["run_magic"] = {"ok": _run_ok, "error": _run_err}

    _findings["runtime"] = {
        "declared": _os.environ.get("FABRIC_RUNTIME", ""),
        "python": ".".join(str(n) for n in _sys.version_info[:3]),
        "spark": _try(lambda: spark.version)[0],
    }
    # An EXPLICIT abfss target, never a relative path: fs._resolve() reads a
    # relative one out of the very runtime context under test, so a relative
    # write would make the artifact's location depend on the answer measured.
    _nbu.fs.put("abfss://__WS__@onelake.dfs.fabric.microsoft.com/__FINDINGS__",
                _json.dumps(_findings), True)
    print("context findings written")
except Exception as _exc:  # noqa: BLE001
    print("context findings NOT written:", type(_exc).__name__, _exc)
"""

# Set by writer() so the readers can address the same workspace, and so the
# contract-1 comparison has the ids the CONTROL PLANE issued rather than the
# ones the session believes. Empty if the writer never got that far.
_ws = ""
_lake = ""
# What the notebook printed. A missing artifact says only "404"; the cell that
# failed to write it says WHY, and a red that does not point at its cause is
# most of the way back to a silent skip.
_said = ""
# Contract 2's half of the same findings file, kept beside the contract-1 half
# so one DFS read serves both.
_sigs_seen = None
_run_magic_seen = None
_runtime_seen = None
_fall_seen = None
_cred_seen = None


def log(msg: str) -> None:
    print(f"==> {msg}", flush=True)


def req(method, url, body=None, token=None, form=False, headers=None):
    data, headers = None, dict(headers or {})
    if body is not None:
        if form:
            data = urllib.parse.urlencode(body).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        else:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    r = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(r, timeout=60) as resp:
        raw = resp.read()
        return resp.status, resp.headers, (json.loads(raw) if raw else {})


# A token this process minted, re-minted before it can expire.
#
# CONTRACT 7 SHORTENS THE ACCESS-TOKEN LIFETIME FOR THE WHOLE STACK
# (TOKEN_LIFETIME_ACCESS_SECONDS), because the emulator has one setting and not
# one per audience. That is fine for the engine, which is the thing under test,
# and NOT fine for a client that minted once and then polled a job for minutes:
# it would 401 partway through and report a broken pipeline. Caching with an
# expiry is what makes a short lifetime safe to ask for — and it is what a
# correct client does anyway, which is the same argument docs/38 §7 makes about
# the engine.
_tokens: dict[str, tuple[str, float]] = {}


def _cached_token(scope: str) -> str:
    tok, good_until = _tokens.get(scope, ("", 0.0))
    if tok and time.monotonic() < good_until:
        return tok
    body = req("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials", "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET, "scope": scope}, form=True)[2]
    # Two thirds of the advertised life, floored at 10s: re-mint before expiry
    # rather than after a 401, because a 401 mid-poll is indistinguishable from
    # a job that failed.
    ttl = float(body.get("expires_in") or 3600)
    _tokens[scope] = (body["access_token"], time.monotonic() + max(ttl * 2 / 3, 10.0))
    return body["access_token"]


def fabric_token() -> str:
    return _cached_token("https://api.fabric.microsoft.com/.default")


def storage_token() -> str:
    """A fresh Storage-audience token, minted here — never the writer's."""
    try:
        req("POST", f"{ENTRA}/admin/api/apps", {
            "displayName": "Azure Storage", "appIdUri": "https://storage.azure.com",
            "isConfidential": False})
    except urllib.error.HTTPError as e:
        if e.code != 409:
            raise
    return _cached_token("https://storage.azure.com/.default")


def writer() -> WriteClaim:
    """Publish + submit + poll. The claim is the job status, nothing else."""
    global _ws
    ws = req("POST", f"{FABRIC}/v1/workspaces", {"displayName": "nb-ws"}, token=fabric_token())[2]["id"]
    lake = req("POST", f"{FABRIC}/v1/workspaces/{ws}/lakehouses",
               {"displayName": "lake"}, token=fabric_token())[2]
    _ws = ws
    global _lake
    _lake = lake["id"]
    log(f"workspace {ws}, lakehouse {lake['id']}")

    # Default lakehouse in the notebook's own metadata — what makes
    # saveAsTable("events") land in this lakehouse rather than nowhere.
    metadata = {
        "kernel_info": {"name": "synapse_pyspark"},
        "dependencies": {"lakehouse": {
            "default_lakehouse": lake["id"],
            "default_lakehouse_name": lake["displayName"],
            "default_lakehouse_workspace_id": ws}},
    }
    meta = "# METADATA ********************\n" + "\n".join(
        "# META " + line for line in json.dumps(metadata, indent=2).splitlines()) + "\n"

    # The notebook `%run` references. Published FIRST, and with the same
    # lakehouse metadata: a referenced child bound to a different default
    # lakehouse is refused by the emulator exactly as Fabric refuses it.
    req("POST", f"{FABRIC}/v1/workspaces/{ws}/items", {
        "displayName": "run-helpers", "type": "Notebook",
        "definition": {"parts": [{
            "path": "notebook-content.py", "payloadType": "InlineBase64",
            "payload": base64.b64encode(
                (RUN_HELPER_BODY + meta).encode()).decode()}]}},
        token=fabric_token())

    _, headers, _ = req("POST", f"{FABRIC}/v1/workspaces/{ws}/items", {
        "displayName": "etl-nb", "type": "Notebook",
        "definition": {"parts": [{
            "path": "notebook-content.py", "payloadType": "InlineBase64",
            "payload": base64.b64encode(
                (NOTEBOOK_BODY.replace("__WS__", ws)
                 .replace("__FINDINGS__", FINDINGS_PATH)
                 .replace("__MODULES__", json.dumps(sorted(REFERENCE_MODULES)))
                 + meta).encode()).decode()}]}},
        token=fabric_token())
    opid = headers.get("x-ms-operation-id")
    nb = None
    for _ in range(60):
        body = req("GET", f"{FABRIC}/v1/operations/{opid}", token=fabric_token())[2]
        if body.get("status") == "Succeeded":
            nb = req("GET", f"{FABRIC}/v1/operations/{opid}/result", token=fabric_token())[2]["id"]
            break
        time.sleep(1)
    if not nb:
        return WriteClaim(ok=False, error="the notebook item never finished creating")
    log(f"notebook {nb}")

    _, hdrs, _ = req(
        "POST", f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances?jobType=RunNotebook",
        token=fabric_token())
    jid = hdrs["Location"].rstrip("/").rsplit("/", 1)[-1]
    log(f"submitted RunNotebook job {jid}")

    base = f"{FABRIC}/v1/workspaces/{ws}/items/{nb}/jobs/instances/{jid}"
    status = None
    # GENEROUS ON PURPOSE. This notebook now carries contracts 1, 2, 3, 4 and 6,
    # and contract 6 runs OPTIMIZE and MERGE against an abfss table — seconds on
    # Sail, minutes on the JVM overlay. At 180s the jvm leg reported
    # `job status = InProgress` and every contract in the artifact failed for
    # one reason: the harness gave up first. A timeout that reads as five
    # defects is worse than a slow run.
    for _ in range(900):
        status = req("GET", base, token=fabric_token())[2].get("status")
        if status in ("Completed", "Failed", "Cancelled", "Deduped"):
            break
        time.sleep(1)
    if status != "Completed":
        # The run detail says which cell died. Logging it is not confirmation —
        # confirmation is the reader's listing.
        try:
            detail = req("GET", f"{base}/notebookRun", token=fabric_token())[2]
            for c in sorted(detail.get("cells", []), key=lambda c: c["index"]):
                log(f"cell {c['index']} {c['status']}: "
                    f"{c.get('error') or c.get('output', '')[:300]}")
        except Exception as exc:  # noqa: BLE001 — best-effort diagnostics
            log(f"could not fetch run detail: {exc}")
        return WriteClaim(ok=False, error=f"job status = {status}")
    log(f"job reached {status}")
    _remember_output(base, fabric_token())
    return WriteClaim(ok=True)


def _remember_output(base: str, token: str) -> None:
    """Keep the cells' stdout so a later contract can quote its own failure.

    Fetched on SUCCESS as well as failure: contract 1's cell is guarded so it
    cannot fail the job, which means its reason only ever appears here.
    """
    global _said
    try:
        detail = req("GET", f"{base}/notebookRun", token=fabric_token())[2]
        _said = "\n".join(c.get("output", "") for c in detail.get("cells", []))
    except Exception as exc:  # noqa: BLE001 — diagnostics, never the verdict
        log(f"could not fetch run detail: {exc}")


def reader() -> Artifact:
    """New Storage token, then a DFS listing. Not Spark, not the writer."""
    if not _ws:
        return Artifact(found=False, location=TABLE_DIR)
    # Fresh token, minted here — not the Fabric-audience token writer() used.
    sft = storage_token()
    try:
        listing = req("GET", f"http://{ACCT}/{_ws}?resource=filesystem&recursive=true"
                             f"&directory={urllib.parse.quote(TABLE_DIR)}",
                      token=sft, headers={"Host": DFS_HOST})[2]
    except Exception as exc:  # noqa: BLE001 — a missing table is found=False
        log(f"reader listing failed: {exc}")
        return Artifact(found=False, location=TABLE_DIR)
    names = [p["name"] for p in listing.get("paths", [])]
    log(f"OneLake listing under {TABLE_DIR}: {len(names)} path(s)")
    return Artifact(found=bool(names), location=TABLE_DIR)


def session_context() -> ContextClaim:
    """Read the findings the notebook wrote, over DFS. Not Spark, not the writer.

    This process never ran inside the session, so what it can testify to is
    what the session RECORDED. The comparison that makes that meaningful is
    against `_ws` / `_lake`, which the control plane issued to the harness
    before the notebook existed — see probes.context_chain.
    """
    if not _ws:
        return ContextClaim(ok=False, error="no workspace was created")
    try:
        sft = storage_token()
        r = urllib.request.Request(
            f"http://{ACCT}/{_ws}/{urllib.parse.quote(FINDINGS_PATH)}",
            headers={"Authorization": "Bearer " + sft, "Host": DFS_HOST})
        with urllib.request.urlopen(r, timeout=60) as resp:
            found = json.loads(resp.read())
    except Exception as exc:  # noqa: BLE001 — a missing artifact is the finding
        said = next((ln.strip() for ln in _said.splitlines()
                     if ln.startswith("context findings NOT written:")), "")
        why = f" — the session said: {said}" if said else ""
        return ContextClaim(
            ok=False,
            error=f"no context findings at {FINDINGS_PATH}: {exc}{why}")
    global _sigs_seen, _runtime_seen
    _sigs_seen = found.get("module_signatures")
    _runtime_seen = found.get("runtime")
    global _fall_seen, _cred_seen
    _fall_seen = found.get("fall_through")
    _cred_seen = found.get("credential")
    log(f"context findings: { {k: v for k, v in found.items() if k != 'module_signatures'} }")
    global _run_magic_seen
    _run_magic_seen = found.get("run_magic")
    log("module signatures reported: "
        + ", ".join(f"{m}={len(v)}" for m, v in sorted((_sigs_seen or {}).items())))
    return ContextClaim(
        ok=True,
        env_workspace=found.get("env_workspace", ""),
        context_workspace=found.get("context_workspace", ""),
        context_lakehouse=found.get("context_lakehouse", ""),
        fallback_workspace=found.get("fallback_workspace", ""),
        env_fallback_set=bool(found.get("env_fallback_set")),
        error="; ".join(found.get("errors", [])),
    )


def session_signatures() -> SignatureClaim:
    """Contract 2 reads the artifact contract 1 already fetched.

    Deliberately NOT a second notebook or a second read: the findings file
    carries both halves, so the two contracts describe one session and one
    round trip. `session_context()` must have run first; if it did not, the
    absent signatures are reported rather than silently read as an empty
    surface, which would fail every method for the wrong reason.
    """
    if _sigs_seen is None:
        return SignatureClaim(
            ok=False,
            error="no signatures in the findings artifact — "
                  "the session did not get as far as reporting them")
    return SignatureClaim(ok=True, seen=_sigs_seen)


def session_runtime() -> RuntimeClaim:
    """Contract 3 reads the same artifact contracts 1 and 2 already fetched."""
    if _runtime_seen is None:
        return RuntimeClaim(
            ok=False,
            error="no runtime in the findings artifact — "
                  "the session did not get as far as reporting it")
    return RuntimeClaim(
        ok=True,
        declared=_runtime_seen.get("declared", ""),
        python=_runtime_seen.get("python", ""),
        spark=_runtime_seen.get("spark", ""),
    )


def fan_out() -> dict[str, str]:
    """Publish N notebooks, submit them AT ONCE, and wait for all of them.

    Submitting serially would prove nothing: the leak contract 5 is about only
    exists while two sessions are live in the same agent at the same time. So
    every job is submitted before any is polled.

    Returns marker -> the notebook id the control plane issued, which is what
    the probe compares each child's own belief against.
    """
    expected, jobs = {}, []
    for i in range(FANOUT_N):
        marker = f"child{i}"
        body = (CHILD_BODY.replace("__WS__", _ws)
                .replace("__DIR__", FANOUT_DIR).replace("__MARKER__", marker))
        _, headers, _ = req(
            "POST", f"{FABRIC}/v1/workspaces/{_ws}/items", {
                "displayName": f"fanout-{marker}", "type": "Notebook",
                "definition": {"parts": [{
                    "path": "notebook-content.py", "payloadType": "InlineBase64",
                    "payload": base64.b64encode(body.encode()).decode()}]}},
            token=fabric_token())
        opid = headers.get("x-ms-operation-id")
        nb = None
        for _ in range(60):
            op = req("GET", f"{FABRIC}/v1/operations/{opid}", token=fabric_token())[2]
            if op.get("status") == "Succeeded":
                nb = req("GET", f"{FABRIC}/v1/operations/{opid}/result", token=fabric_token())[2]["id"]
                break
            time.sleep(1)
        if not nb:
            log(f"{marker}: notebook never finished creating")
            continue
        expected[marker] = nb
        _, hdrs, _ = req(
            "POST",
            f"{FABRIC}/v1/workspaces/{_ws}/items/{nb}/jobs/instances?jobType=RunNotebook",
            token=fabric_token())
        jobs.append((marker, nb, hdrs["Location"].rstrip("/").rsplit("/", 1)[-1]))
    log(f"submitted {len(jobs)} concurrent RunNotebook jobs")

    deadline = time.monotonic() + 300
    pending = {j: (m, nb) for m, nb, j in jobs}
    while pending and time.monotonic() < deadline:
        for jid, (marker, nb) in list(pending.items()):
            base = f"{FABRIC}/v1/workspaces/{_ws}/items/{nb}/jobs/instances/{jid}"
            status = req("GET", base, token=fabric_token())[2].get("status")
            if status in ("Completed", "Failed", "Cancelled", "Deduped"):
                log(f"{marker}: {status}")
                pending.pop(jid)
        if pending:
            time.sleep(1)
    return expected


LANGUAGES_DIR = "lake.Lakehouse/Files/conformance/languages"


def _publish(name: str, body: str, resources: dict | None = None) -> str:
    """Publish a notebook and return its id, following the 200-or-202 outcome.

    `resources` become `builtin/…` definition parts — which is what a notebook
    resource IS on Fabric, so the folder travels with the item rather than
    being staged by this harness.
    """
    parts = [{
        "path": "notebook-content.py", "payloadType": "InlineBase64",
        "payload": base64.b64encode(
            body.replace("__WS__", _ws)
                .replace("__DIR__", LANGUAGES_DIR).encode()).decode()}]
    for relative, content in (resources or {}).items():
        parts.append({
            "path": "builtin/" + relative, "payloadType": "InlineBase64",
            "payload": base64.b64encode(content.encode()).decode()})
    _, headers, _ = req(
        "POST", f"{FABRIC}/v1/workspaces/{_ws}/items", {
            "displayName": name, "type": "Notebook",
            "definition": {"parts": parts}},
        token=fabric_token())
    opid = headers.get("x-ms-operation-id")
    for _ in range(60):
        op = req("GET", f"{FABRIC}/v1/operations/{opid}", token=fabric_token())[2]
        if op.get("status") == "Succeeded":
            return req("GET", f"{FABRIC}/v1/operations/{opid}/result",
                       token=fabric_token())[2]["id"]
        time.sleep(1)
    raise RuntimeError(f"notebook {name!r} never finished creating")


def _run_to_completion(nb: str):
    """Submit RunNotebook, wait, and return (status, per-cell detail)."""
    _, hdrs, _ = req(
        "POST", f"{FABRIC}/v1/workspaces/{_ws}/items/{nb}/jobs/instances?jobType=RunNotebook",
        token=fabric_token())
    jid = hdrs["Location"].rstrip("/").rsplit("/", 1)[-1]
    base = f"{FABRIC}/v1/workspaces/{_ws}/items/{nb}/jobs/instances/{jid}"
    status = None
    for _ in range(900):
        status = req("GET", base, token=fabric_token())[2].get("status")
        if status in ("Completed", "Failed", "Cancelled", "Deduped"):
            break
        time.sleep(1)
    detail = req("GET", f"{base}/notebookRun", token=fabric_token())[2]
    return status, sorted(detail.get("cells", []), key=lambda c: c["index"])


def _read_marker(name: str):
    """A marker's bytes over DFS, or None. Not Spark, not the job record."""
    r = urllib.request.Request(
        f"http://{ACCT}/{_ws}/{urllib.parse.quote(LANGUAGES_DIR)}/{name}",
        headers={"Authorization": "Bearer " + storage_token(), "Host": DFS_HOST})
    try:
        with urllib.request.urlopen(r, timeout=60) as resp:
            return resp.read() if resp.status == 200 else None
    except Exception:  # noqa: BLE001 — absence is an answer here, not a failure
        return None


def _marker_exists(name: str) -> bool:
    return _read_marker(name) is not None


def cell_languages() -> dict:
    """The four dispositions, observed through a real run.

    NOT A CONTRACT CELL — it is not one of docs/38's seven, so it stays out of
    the matrix and fails the RUN instead, the same way `%run` does.

    Returns {"ok": bool, "error": str}.
    """
    try:
        nb = _publish("cell-languages-nb", CELL_LANGUAGES_BODY)
        status, cells = _run_to_completion(nb)
        if status != "Completed":
            return {"ok": False,
                    "error": f"the magics notebook ended {status}: "
                             + "; ".join(f"cell {c['index']} {c['status']} "
                                         f"{c.get('error') or ''}" for c in cells)}
        if len(cells) < 4:
            return {"ok": False, "error": f"expected 4 cells, got {len(cells)}"}
        # %%configure: ACCEPTED AND IGNORED, and it has to SAY so. Executed as
        # Python this cell is a NameError (`false`), so a pass here cannot come
        # from it having run.
        note = cells[0].get("output") or ""
        if "IGNORED" not in note or "not applied" not in note:
            return {"ok": False,
                    "error": f"%%configure did not report being ignored: {note!r}"}
        # %%html and %%markdown: rendered, never executed. The html cell is a
        # Python SyntaxError, so this too cannot pass by executing.
        for i in (1, 2):
            if cells[i].get("status") != "Succeeded":
                return {"ok": False,
                        "error": f"markup cell {i} did not succeed: {cells[i]}"}
            if "rendered, not executed" not in (cells[i].get("output") or ""):
                return {"ok": False,
                        "error": f"markup cell {i} did not report rendering: {cells[i]}"}
        # ...and the ordinary Python cell after them still ran. Confirmed OUT OF
        # BAND: the marker is read over DFS, not taken from the job record.
        if not _marker_exists("languages.json"):
            return {"ok": False,
                    "error": "the python cell after the magics left no marker"}
    except Exception as exc:  # noqa: BLE001 — the reason is the finding
        return {"ok": False, "error": f"magics notebook: {type(exc).__name__}: {exc}"}

    try:
        nb = _publish("scala-nb", SCALA_BODY)
        status, cells = _run_to_completion(nb)
        if status != "Failed":
            return {"ok": False,
                    "error": f"a Scala cell did not fail the run: status={status}"}
        err = (cells[0].get("error") or "") if cells else ""
        # NAMED, which is the whole point: this used to be a Python
        # SyntaxError pointing at correct Scala.
        if "scala" not in err.lower():
            return {"ok": False,
                    "error": f"the refusal does not name the language: {err!r}"}
        if "Real Fabric runs it" not in err:
            return {"ok": False,
                    "error": f"the refusal reads as 'your cell is invalid': {err!r}"}
        # THE RUN STOPPED. Absence is the assertion — a run that carried on
        # past a refused cell would have left this marker behind.
        if _marker_exists("after-scala.json"):
            return {"ok": False,
                    "error": "the run continued past a cell it refused"}
    except Exception as exc:  # noqa: BLE001
        return {"ok": False, "error": f"scala notebook: {type(exc).__name__}: {exc}"}
    return {"ok": True, "error": ""}


def notebook_resources() -> dict:
    """`builtin/` resolves to the ROOT notebook, through a real reference run.

    NOT A CONTRACT CELL — gated like `%run` and the cell languages: the matrix
    is unchanged, a regression fails the leg.

    THE NEGATIVE CONTROL IS THE POINT. Both notebooks ship `builtin/data.txt`
    with different content, so the child's answer names which folder it
    resolved against. Asserting merely that a file was found would pass on the
    wrong one.
    """
    try:
        root_text = "from the ROOT notebook"
        child_text = "from the CHILD notebook"
        _publish("res-child-nb", RES_CHILD_BODY, {"data.txt": child_text})
        nb = _publish("res-root-nb", RES_ROOT_BODY, {"data.txt": root_text})
        status, cells = _run_to_completion(nb)
        if status != "Completed":
            return {"ok": False,
                    "error": f"the resources notebook ended {status}: "
                             + "; ".join(f"cell {c['index']} {c['status']} "
                                         f"{c.get('error') or ''}" for c in cells)}
        # OUT OF BAND: the child's finding is read over DFS, not from the job
        # record of the run that produced it.
        raw = _read_marker("resources.json")
        if raw is None:
            return {"ok": False, "error": "the child notebook left no finding"}
        seen = json.loads(raw).get("seen", "")
        if seen == child_text:
            return {"ok": False,
                    "error": "the child resolved builtin/ to ITS OWN folder; "
                             "a referenced notebook must see the root's"}
        if seen != root_text:
            return {"ok": False, "error": f"the child read something else: {seen!r}"}
    except Exception as exc:  # noqa: BLE001 — the reason is the finding
        return {"ok": False, "error": f"resources: {type(exc).__name__}: {exc}"}
    return {"ok": True, "error": ""}


def session_isolation(expected: dict) -> IsolationClaim:
    """Read every child's artifact over DFS. Not Spark, not any of the writers."""
    if not expected:
        return IsolationClaim(ok=False, error="no children were created")
    sft = storage_token()
    seen = {}
    for marker in expected:
        try:
            r = urllib.request.Request(
                f"http://{ACCT}/{_ws}/{urllib.parse.quote(FANOUT_DIR)}/{marker}.json",
                headers={"Authorization": "Bearer " + sft, "Host": DFS_HOST})
            with urllib.request.urlopen(r, timeout=60) as resp:
                seen[marker] = json.loads(resp.read())
        except Exception as exc:  # noqa: BLE001 — a missing child is the finding
            log(f"{marker}: no findings ({exc})")
    log(f"fan-out artifacts read: {len(seen)}/{len(expected)}")
    return IsolationClaim(ok=True, seen=seen)


def session_fall_through() -> FallThroughClaim:
    """Contract 6 reads the same artifact contracts 1-3 already fetched."""
    if _fall_seen is None:
        return FallThroughClaim(
            ok=False,
            error="no fall-through results in the findings artifact — "
                  "the session did not get as far as running the statements")
    if _fall_seen.get("skipped"):
        return FallThroughClaim(
            ok=False,
            error=("not run on this backend: CONFORMANCE_FALL_THROUGH is not set "
                   "in its compose. The gate exists because a statement that "
                   "hangs takes contracts 1-4 down with it and reports the "
                   "harness's own timeout as five separate defects — measured "
                   "twice, when this probe still spelled the table `abfss://` "
                   "and hadoop forced TLS against a plaintext stack"))
    if _fall_seen.get("table_error"):
        return FallThroughClaim(
            ok=False,
            error=("the probe table could not be created "
                   f"({_fall_seen['table_error'][:160]}), so neither statement "
                   "says anything about the grammar"))
    return FallThroughClaim(
        ok=True,
        recognised_ok=bool(_fall_seen.get("recognised_ok")),
        recognised_error=_fall_seen.get("recognised_error", ""),
        unrecognised_ok=bool(_fall_seen.get("unrecognised_ok")),
        unrecognised_error=_fall_seen.get("unrecognised_error", ""),
        name_form_ok=(None if "name_form_ok" not in _fall_seen
                      else bool(_fall_seen.get("name_form_ok"))),
        name_form_error=_fall_seen.get("name_form_error", ""),
    )


def session_credential() -> CredentialClaim:
    """Contract 7 reads the same artifact contracts 1-3 and 6 already fetched."""
    if _cred_seen is None:
        return CredentialClaim(
            ok=False,
            error="no credential results in the findings artifact — "
                  "the session did not get as far as the two reads")
    if _cred_seen.get("skipped"):
        return CredentialClaim(
            ok=False,
            error="not run: CONFORMANCE_TOKEN_LIFETIME was not set, so the "
                  "session had no lifetime to outlive")
    return CredentialClaim(
        ok=True,
        lifetime=int(_cred_seen.get("lifetime") or 0),
        slept=float(_cred_seen.get("slept") or 0),
        before_ok=bool(_cred_seen.get("before_ok")),
        before_error=_cred_seen.get("before_error", ""),
        after_ok=bool(_cred_seen.get("after_ok")),
        after_error=_cred_seen.get("after_error", ""),
    )


def main() -> int:
    backend = os.environ.get("BACKEND", "")
    if backend not in ("sail", "jvm"):
        print(f"BACKEND must be sail or jvm, got {backend!r}", file=sys.stderr)
        return 2
    result = write_landing(
        writer=writer,
        reader=reader,
        expected_location=TABLE_DIR,
        backend=backend,
    )
    # Contract 1 rides the SAME run: the notebook that wrote the table also
    # recorded its context, so the two cells describe one session rather than
    # two, and the run costs no extra notebook.
    ctx = context_chain(
        session=session_context,
        expected_workspace=_ws,
        expected_lakehouse=_lake,
        backend=backend,
    )
    sig = signature_shape(session=session_signatures,
                          reference=REFERENCE_MODULES, backend=backend)
    runtimes = json.loads((DIR / "fabric-runtimes.json").read_text(
        encoding="utf-8"))["runtimes"]
    floor = runtime_floor(session=session_runtime, runtimes=runtimes, backend=backend)
    expected = fan_out()
    iso = concurrent_isolation(
        session=lambda: session_isolation(expected),
        expected=expected, backend=backend)
    # `control` comes from the applicability table, not from a backend name:
    # the jvm column is `control` for this contract and required for the rest,
    # and the doc is the authority on which is which.
    fall = fall_through(session=session_fall_through, backend=backend,
                        control=(6, backend) in CONTROL)
    rows = record(backend,
                  live={1: ctx, 2: sig, 3: floor, 4: result, 5: iso, 6: fall,
                        7: credential_lifetime(session=session_credential,
                                               backend=backend)})
    out = DIR / "out"
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{backend}.json"
    path.write_text(json.dumps(rows, indent=2) + "\n", encoding="utf-8")
    log(f"wrote {path} contract 1={ctx.status} {ctx.error}".rstrip())
    log(f"wrote {path} contract 2={sig.status} {sig.error}".rstrip())
    log(f"wrote {path} contract 3={floor.status} {floor.error}".rstrip())
    log(f"wrote {path} contract 5={iso.status} {iso.error}".rstrip())
    log(f"wrote {path} contract 6={fall.status} {fall.error}".rstrip())
    seven = next(r for r in rows if r["id"] == "7")
    log(f"wrote {path} contract 7={seven['status']} {seven.get('error', '')}".rstrip())
    log(f"wrote {path} contract 4={result.status} {result.error}".rstrip())

    # `%run` — NOT a contract cell, and gated anyway.
    #
    # It is not one of docs/38's seven, so it does not belong in the matrix.
    # But recording it without grading it is how a number becomes a memory —
    # which is exactly what happened to `OPTIMIZE <name>` and cost a session
    # and a half. So it fails the RUN instead: the matrix is unchanged, and a
    # regression still stops the leg.
    #
    # This is the only harness that can prove it. The notebookutils e2e runs
    # its notebook as a plain script, and e2e/notebook-run's runner is itself
    # the engine — neither reaches the agent's run_code, where the rewrite
    # lives.
    if _run_magic_seen is None:
        log("%run: NOT REPORTED — the cell never ran")
        return 1
    if not _run_magic_seen.get("ok"):
        log(f"%run FAILED: {_run_magic_seen.get('error') or 'no reason given'}")
        return 1
    log("%run: helpers spliced into this session — run_scaled/run_label usable")

    # Cell languages — also not a contract cell, and gated the same way.
    #
    # These four dispositions had Go tests, and those call
    # `Disposition(language)` directly. The defect they were written for did
    # not live there: the parser always classified the magic correctly and the
    # RUN LOOP ignored the answer, so the bug sat in the gap between the
    # classifier and its caller — where a unit test on the classifier passes on
    # both sides of it. This runs the loop.
    languages = cell_languages()
    if not languages["ok"]:
        log(f"cell languages FAILED: {languages['error']}")
        return 1
    log("cell languages: configure ignored out loud, markup rendered, "
        "Scala refused by name and the run stopped")

    # Notebook resources — same gate again.
    resources = notebook_resources()
    if not resources["ok"]:
        log(f"notebook resources FAILED: {resources['error']}")
        return 1
    log("notebook resources: a referenced child read the ROOT's builtin/, "
        "not its own")
    return 0


if __name__ == "__main__":
    sys.exit(main())
