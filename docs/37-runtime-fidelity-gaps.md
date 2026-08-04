# 37 — Runtime fidelity: four documented divergences, and what closing each takes

**Status: all four are stated in the code and none is implemented.** Each is a
place where the emulator's *runtime* — the thing a notebook actually sees — is
deliberately an analog of Fabric's rather than the thing itself. They were
written down at the moment they were created, which is why this document can
scope them instead of discovering them. This is the promotion of those comments
into capabilities we intend to finish.

They share one property worth stating up front: **each is honest today and wrong
tomorrow.** A consumer who reads the comment is not misled. A consumer who reads
only the parity map may have been: see
[the correction](#a-correction-applied-the-parity-map-overclaimed-environments)
at the end, which is the one item here that was not a gap but a mis-grade, and is
already fixed.

## Why these four, together

Three of the four are the same shape: the emulator runs **one long-lived agent
process with many session namespaces**, where Fabric gives each session its own
container. The Files mount, the Environment analog, and the `input_file_name`
shim all bend around that single fact. The fourth (pipelines) is control-plane
only and unrelated, but it is grouped here because it is the last job type that
does not follow the repo's own async pattern.

Naming that shared cause matters for sequencing: two of the three are cheap
*because* they accept the single-process model, and one is expensive *because* it
cannot.

---

## 1. Environment items: the parse exists, the effect does not

**What Fabric does.** An Environment item's custom libraries, public packages,
and Spark properties are installed and applied to the runtime before any user
code runs. It is where a framework package comes from.

**What the emulator does.** It parses the item correctly and then discards the
answer. `compute.ParseEnvironment` (`internal/compute/definition.go`) resolves
`requirements.txt`, `*.jar`, and the JSON `sparkProperties` / `pythonLibraries`
keys into `Environment{PythonPackages, JARs, SparkConfig}`.
`resolveComputeBinding` (`internal/api/notebooks.go`) calls it, errors correctly
on a missing or wrong-typed Environment item, and stores the result on the run as
`environment`.

**Nothing reads those three fields.** `PythonPackages`, `SparkConfig`, and
`JARs` have no consumer anywhere in `internal/` outside the parser and its own
test. The run *reports* an Environment; the session never *receives* one. The
working substitute is `_install_custom_wheels` in `python/spark_agent/agent.py`,
which installs whatever a consumer bind-mounts at `/opt/wheels` — generic on
purpose, and explicitly documented as the analog.

**The fix, which is plumbing rather than design.** The front half is built; what
is missing is the wire between it and the agent:

1. Carry the resolved `Environment` in the session bind, beside the `/mount`
   call `registerLakehouseTables` already makes (`internal/api/livy_catalog.go`),
   or as a sibling `/environment` endpoint on the same best-effort contract.
2. Generalise `_install_custom_wheels` to take a package list instead of only
   globbing `/opt/wheels/*.whl`. The installer, the `uv` path, and the
   loud-but-not-fatal failure handling already exist.
3. Apply `SparkConfig` at session setup, where the runtime presets are already
   applied.
4. `/opt/wheels` demotes from *the* mechanism to the fallback it should be: an
   Environment item drives installs when one is bound, the bind-mount serves
   consumers who do not model one.

**What must not be done.** Do not install into the shared agent on every bind
without deciding what happens when two sessions bind conflicting Environments.
Fabric isolates them per container; this emulator cannot, and silently letting
the last bind win would recreate divergence 2's worst property in a place where
it corrupts a dependency tree rather than a directory. The honest first version
refuses a conflicting bind (see 2).

**Size: S.** Four things that exist, connected. The value is in an e2e that binds
an Environment declaring a package the image lacks and proves a notebook can
import it.

---

## 2. The Files mount: one-way, sync-at-bind, one mount point

**What Fabric does.** FUSE-mounts the notebook's default lakehouse at
`/lakehouse/default`, live and read-write, per session.

**What the emulator does.** `python/spark_agent/files_mount.py` mirrors the
bound lakehouse's `Files/` tree to `/lakehouse/default/Files` over the OneLake
DFS API at bind time, skipping files whose local copy matches by size. Its
docstring states three divergences, and they are not equally hard.

### 2a. Write-back — **S**

A notebook write to `/lakehouse/default/Files` lands on container disk and never
reaches OneLake. `notebookutils.fs.put` already exists
(`python/notebookutils/fs.py`), so the missing piece is a flush: at statement
end, walk `MOUNT_ROOT` for files whose mtime beats the last sync and `put` them
back. Notebook code in this repo writes through `abfss://` paths, which is why
the read direction was the one that blocked, and why this stayed open.

### 2b. Live sync — **S**

Files uploaded to OneLake after a session bound appear at the next bind, not
immediately. Re-walking at statement boundaries closes the gap that actually
bites, and the existing size-skip makes a re-sync cheap. This is not true FUSE
liveness and should not claim to be; it is "fresh at every statement", which is
a bound a consumer can reason about.

### 2c. One mount point, last bind wins — **L, and not worth it yet**

`/lakehouse/default` is a single filesystem path in a single agent container.
Sessions bound to *different* lakehouses share it. No symlink scheme fixes a
global path shared by concurrent sessions; the real fix is one agent container
per session — a pool — which touches the whole compute model.

**Change the behaviour instead of the architecture.** Today a switching bind
logs loudly and proceeds, which still corrupts the reads of whichever session
bound first. **Refuse the second bind** with an error naming both lakehouses.
That converts silent data corruption into an explicit failure at bind time, for
the same reason the notebook-job reconciliation exists: silence is the outcome
that reads as success. **Size: XS**, and it makes 2a and 2b safe to build.

---

## 3. `input_file_name()`: the SQL path, and the non-file frame

`python/spark_agent/input_file.py` reconstructs the function on engines that lack
it (Sail rejects it outright) by tagging each file's rows with its own path at
read time, pointing `F.input_file_name` at the tag, and stripping the tag at
every write. Its docstring is emphatic that a stub returning `""` would be worse
than nothing, and that is the standard the two remaining gaps are measured
against.

### 3a. SQL-string usage bypasses the shim — **M, with real research risk**

`spark.sql("SELECT input_file_name() ...")` never touches the patched
`F.input_file_name`, so it fails on the engine as if the shim were not there.

**A UDF cannot fix this, and it is worth recording why.** Python UDFs *do* run on
Sail — `spark.python_udf` passes on both engines in
[engine-matrix.md](engine-matrix.md) — so registering
`spark.udf.register("input_file_name", …)` is mechanically available. But the
function takes no arguments, so a registered UDF has no way to see which row it
is evaluating or which tag column the frame carries. It could only return a
constant, which is precisely the silently-wrong-lineage failure the module
exists to refuse.

**The available lever is statement rewriting**, and this repo has already
accepted that technique: `internal/tsql` rewrites nested CTEs and CTAS on the
wire for T-SQL. The Spark-SQL analog intercepts `spark.sql()` in the agent and
rewrites `input_file_name()` to a reference to the tag column when the query's
relations carry one. The token is distinctive (zero-arg, unambiguous), which is
what makes it tractable.

The risk is the same one T-SQL faced and answered with a lexer: a blind string
replace would be *worse* than the honest gap, because it would corrupt queries
that merely mention the name. Rewriting must know which relations are tagged and
leave everything else alone.

### 3b. A non-file frame errors where Spark returns `""` — **do not fix**

`F.input_file_name()` is a frame-free factory in the PySpark API while the tag is
per-frame, so at call time there is nothing to inspect. The only fix that works
is tagging *every* frame — wrapping `createDataFrame`, `range`, `sql`, table
reads — so the column always resolves.

That trades a loud error for a bookkeeping column leaking into `df.columns`,
`printSchema`, `SELECT *`, and `toPandas`. The module already names that as the
thing not to do: an emulator that leaks bookkeeping into user data creates
exactly the parity drift it exists to prevent. Tagging only file reads is what
bounds the leak.

**The proportionate change is the error message**, not the behaviour: raise
something that names the shim and explains that provenance was requested on a
frame that never came from a file, instead of a bare `AnalysisException` on an
internal column name. **Size: XS.**

---

## 4. Pipeline jobs execute inline in the job POST

**What Fabric does.** `POST .../jobs/instances` returns `202` with a `Location`
header; the client polls. Execution outlives the request.

**What the emulator does.** The wire contract is already right — the handler
returns `202` and a `Location` (`internal/api/jobs.go`). But for
`it.Type == "DataPipeline"` the handler calls `runPipelineWith` *inline* and does
not return until the whole definition has executed. A long pipeline can outlive
a client's HTTP timeout, and nothing is pollable while it runs.

**The repo already solved this for every other job type.** A notebook run sets
`CompleteAt = math.MaxInt64` and launches `go a.driveNotebookRun(...)`; Airflow
does the same with `go a.runAirflow(...)`. `DataPipeline` is the last inline one.

Two facts bound the problem:

- **`Wait` does not sleep.** It records `waitTimeInSeconds` and returns
  (`internal/pipeline/activities.go`), so a ten-minute `Wait` costs nothing. Only
  real notebook and Copy work consumes wall-clock.
- **The notebook activity blocks on purpose and must keep doing so.** Fabric's
  notebook activity is synchronous, so `pipelineExecutor.Execute` drives the run
  in its own goroutine and gates on the outcome (`internal/api/pipelines.go`).
  That is correct. A fan-out of 21 notebooks is therefore genuinely long, which
  is exactly when the inline POST hurts.

**The fix.** `go a.runPipelineWith(...)`, `CompleteAt = math.MaxInt64`, and
`terminalStatusOf` must stop listing `DataPipeline` under `executesNow` — it
currently reports `Completed` at POST time, which after this change would be the
same lie the notebook reconciliation was built to kill.

**This does not contradict [24-parity-completion.md](24-parity-completion.md)'s
"won't do" row.** That row refuses to make long-running operations take *real
time* — the virtual clock is the emulator's most valuable testing property and
sleeping would be strictly worse. This change adds no sleep. The pipeline already
takes real wall-clock time because real notebooks execute; the only change is
that the client can poll during it instead of holding a socket open.

**Cost is test churn, not design risk.** There are 96 `jobStatus()` assertion
sites across the Go tests, ~50 of them on pipelines, and each becomes
`awaitJob()` — which already exists (`internal/api/notebookdrive_test.go`) and
whose comment already states the reasoning: submitting a job returns 202 and the
caller polls, so the test polls too rather than sleeping a guessed interval.

**Size: M**, almost entirely mechanical. Secondary benefit: pipeline runs become
observable mid-flight on the flow stream
([31-flow-observability.md](31-flow-observability.md)), which today emits
per-activity events no client can read until the POST returns.

---

## A correction, applied: the parity map overclaimed Environments

[parity.md](parity.md) graded **Environments** 🟢 portable subset / 🟠 and said:

> Python packages are provisioned per run; config is applied to the real session;
> JAR-bearing runs explicitly require JVM Spark

Per divergence 1, none of the three happens. The declarations are parsed and
reported; nothing installs a package, no Spark config reaches the session, and no
code refuses a JAR-bearing run — there is no JAR handling anywhere outside the
parser, so the 🟠 half was unbacked too.

The witness recorded for the claim was `go:TestParseEnvironment`, which tests the
parser and nothing downstream. That is the "a name that exists is not a test that
ran" failure one layer in: the name existed, the test ran, and it did not witness
what the row asserted. `check_witnesses.py` cannot catch this shape by
construction — its own docstring says so ("what it still does not prove: that a
witness ASSERTS the claim").

**Both are now fixed.** The row reads 🟡 Emulated (resolved and reported, not
applied), describes what actually happens, and points here; the
`environments` witness entry is removed, because a 🟡 row makes no supported
claim to witness. Supported claims went 76 → 75.

What remains is a rule for when divergence 1 *is* built: the witness must be an
e2e that imports a package only an Environment could have supplied, and the grade
moves to 🟢 then and not before. A parser test must never again stand behind an
applied-to-the-session claim.

## Order of work

| # | Capability | Size | Why this position |
|---|---|---|---|
| 2c | Refuse a conflicting lakehouse bind | XS | Makes 2a/2b safe; turns corruption into an error |
| 3b | Diagnostic error for a non-file frame | XS | Behaviour stays; the message stops being cryptic |
| ~~—~~ ✅ | ~~Correct the Environments parity row and witness~~ | XS | Done: 🟡, reworded, witness removed. Regrade to 🟢 only with an e2e |
| 1 | Environment items reach the session | S | Finishes a feature already built four-fifths of the way |
| 2a/2b | Mount write-back and per-statement refresh | S | Both unblocked by 2c |
| 4 | Pipelines async | M | Mechanical, but ~50 test sites |
| 3a | `input_file_name()` in SQL | M | The only item with genuine research risk |
| 2c′ | Agent-per-session pool | L | Deferred; revisit only if concurrent multi-lakehouse sessions become real |

## What must NOT be done

- **Do not stub `input_file_name()` to return `""`.** Every row then claims it
  came from nowhere and a lineage audit built on it is worse than one that
  failed.
- **Do not tag every frame** to smooth over 3b. Bookkeeping in user schemas is
  the drift this emulator exists to prevent.
- **Do not add sleeps to make jobs "really" long.** The virtual clock is the
  point; see [24-parity-completion.md](24-parity-completion.md).
- **Do not let a second Environment or lakehouse bind silently win.** Refuse it.
  A single-process runtime cannot pretend to per-session isolation, and pretending
  is how both of these become invisible.
