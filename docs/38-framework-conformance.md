# 38 — Framework conformance: what a Fabric product assumes, and how to test it

**Status: all seven contracts are live. 14 of 18 cells.** Contract 4 is ✅ on all three
backends — a write through the emulator path, confirmed out of band (OneLake
DFS on sail/jvm; a fresh TDS connection on warehouse). The engine that wrote
is never the one that confirms. **Contract 1 is ✅ on sail and ❌ on jvm**,
and the jvm red is a real defect rather than a missing assertion: the JVM
overlay image has no `notebookutils` installed at all, so a notebook cannot
import the surface this repo grades 🟢 Real. **Both are now ✅ on both
backends**: the seven missing `notebookutils.notebook` methods are implemented,
and the JVM overlay has an interpreter that can import the shim at all.
**Contract 3 is now asserted too**, and green: both images declare the Fabric
Runtime they target and both meet its Python floor. **Contract 5 is green on
sail and jvm** — three notebooks submitted at once, each writing its own
artifact, each knowing only its own identity — and stays ❌ on warehouse, where
concurrent TDS sessions need a Go leg this kit does not have yet. **Contract 6 is green on sail** and records a gap on jvm for a reason worth its
own paragraph below. **Contract 7 is green on both.** What remains is the
warehouse column for 5, 6 and 7, which needs a Go leg driving concurrent TDS
sessions, and contract 6 on jvm. The offline half (`docs/conformance-matrix.md`,
`check_conformance.py --strict`) still gates `make check`.

**Two defects came out of contract 1's first run, and neither was visible to
anything else in the repo.** Both are the class this document names: green
parity rows, green witnesses, green test suite, and a notebook that cannot do
the thing.

1. **`notebookutils` is unconfigured in the spark-agent.** The shim defaults
   to `http://127.0.0.1:19080`, which inside that container is nothing, so
   `fs.put` from a `RunNotebook` cell raised `Connection refused`. No shipped
   compose (`docker-compose.override.yml`, `.compute.yml`, `.spark-jvm.yml`)
   and no `e2e/*` compose sets `NOTEBOOKUTILS_FABRIC_URL` on the agent;
   `jupyter` in `docker-compose.yml` sets all three. So the shim works in a
   Jupyter cell and fails in a notebook job — while the jupyter service's own
   comment claims "a cell here and a cell in a RunNotebook job execute
   identically". The conformance composes set them so the contract can be
   measured at all; **the shipped composes are unchanged and still carry the
   gap.**
2. **The JVM overlay has no `notebookutils` at all** — `ModuleNotFoundError`,
   not a configuration problem. Contract 3 already flags that image's Python
   as below the framework floor; it also lacks the shim.

Neither is fixed here. A change that lands the harness and rewrites two images
is one nobody can review, and the matrix exists precisely so a gap can be
recorded red with a pointer instead of blocking the kit.
This document generalises a class of defect the emulator kept shipping: contracts
that real Fabric *frameworks* depend on, which no amount of reading Microsoft's
REST reference reveals, because they are not in the REST surface at all. They
live in the notebook runtime — what is importable, what is in scope, what a
signature looks like, what `runtime.context` answers, and where a write actually
lands.

Every defect in this class was found the same way: by driving a real data
product end to end and watching it fail. None was found by the parity map, the
witness system, or the test suite, all of which were green throughout. That is
the fact this document exists to fix.

## Why the REST reference does not cover this

The emulator's fidelity work has mostly been control-plane: an API surface with
a published schema, which can be read, implemented, and witnessed. Notebook
frameworks depend on something else — the **runtime contract**, which Microsoft
documents thinly and which frameworks probe rather than read:

- they resolve identity through a **fallback chain**, so satisfying one path is
  not enough;
- they **introspect signatures** to decide whether a runtime supports a feature,
  before calling anything;
- they assume the session is **theirs alone**, because on Fabric it is;
- they assume a successful write **landed where Fabric puts it**.

An emulator can satisfy every documented endpoint and fail all four.

## The seven contracts

### 1. Session context is a control-plane contract, not an environment variable

**What a framework does.** It resolves the workspace and lakehouse it is running
in by probing in order: `mssparkutils.env.getWorkspaceId()`, then
`mssparkutils.runtime.context`, then an environment fallback, then it raises.
Every published Fabric framework has some version of this chain. Satisfying only
the last link means a framework that never reaches it fails on its first call.

**Where the emulator stands.** `/mount` already sent workspace and lakehouse
ids at bind (the Files mount needs them). `runtime.context` ignored that and
was built **at module import** from `NOTEBOOKUTILS_WORKSPACE_ID` /
`NOTEBOOKUTILS_LAKEHOUSE_ID`, so it was process-global: not per-run, and in a
shared agent not even per-session. A statement request carried `session`,
`code` and `kind` — identity for lineage (`jobId`/`cellIndex`) but not the
notebook's workspace.

**The fix (landed for RunNotebook).** Every notebook `/statements` body now
carries `workspaceId` / `lakehouseId` / `notebookId` / `isForPipeline`. The
agent remembers them per session (including from `/mount`) and binds
`notebookutils.runtime.context` around the statement via a `ContextVar`, so
two concurrent notebooks cannot see each other's workspace.
`mssparkutils.env.getWorkspaceId()` reads that same context. The environment
remains the fallback for a kernel that has no agent. Fabric's context also
carries capacity; that field is still absent.

**This was the highest-value single item in this document**, because context
resolution is the first thing a framework does. Setting environment variables
out of band is no longer the only path that works.

### 2. The API shape is the contract, independent of behaviour

**What a framework does.** It inspects a function's signature and declines to run
if a parameter is missing — without ever calling it. A `notebookutils.notebook.
run` lacking `spark_environment` / `attach_lakehouse` is read as "this runtime
does not support notebook activities", and the framework stops.

**The generalisation.** For the `notebookutils` / `mssparkutils` surface,
accepting a parameter and ignoring it is *correct emulation* when there is
nothing to switch — the emulator has one session and attaches the notebook's own
binding. What is not correct is omitting the parameter, because omission is a
signal frameworks read.

**The fix, landed for `notebookutils.notebook`.**
`e2e/conformance/notebookutils-reference.json` pins the documented signatures
the way `test(schema)` pins REST payload fields, and **every entry cites the
Microsoft page it was read from plus that page's own last-updated date** — a
reference assembled from memory would be this same defect one tier up, a claim
about Fabric with nothing behind it. Its scope is declared rather than implied:
`fs`, `credentials`, `env`, `runtime`, `lakehouse`, `session`, `udf` and
`variableLibrary` are listed as not yet covered, so a partial reference cannot
be read as a complete one.

**What the probe found, and what was done about it.** The four orchestration
methods were correct — `run`, `runMultiple`, `validateDAG`, `exit` all carry
their documented parameters in the documented order. **Seven documented methods
were absent entirely**: `create`, `get`, `getDefinition`, `update`,
`updateDefinition`, `delete`, `list` — the whole notebook-management surface a
CI/CD framework introspects before it will run.

They are now implemented, and implementing them surfaced a third defect one
layer down. Microsoft's `create(content=…)` takes **`.ipynb`**; this emulator
executes from `notebook-content.py`. Nothing derived one from the other outside
the VS Code route, so a notebook created the documented way stored happily,
returned 201/202, and its `RunNotebook` job then failed with
`notebook-content.py is missing` — **a create that reports success and produces
something unrunnable**, which is §4's shape arriving one API call later.

The derivation is **server-side** (`notebookExecutableParts`, called from
`createItem` and `updateDefinition`), reusing the existing converter, so there
is one definition of it in the package that owns the parser and a Python client,
the VS Code route and a raw REST caller cannot drift apart. Doing it in the shim
would have been a second definition. Two refusals in it: an author who sends
both parts keeps theirs, and an undecodable payload is stored as sent rather
than rejected there.

**Missing fails, extra passes, order counts.** A framework declines on an
absent parameter without calling anything, so omission is the signal. Accepting
one and ignoring it is correct emulation when there is nothing to switch — this
shim already carries `spark_environment` and `attach_lakehouse`, which
Microsoft's current page does not document, and that is fine. Order is part of
the contract because Fabric's own examples are positional
(`run("Sample1", 90, {"input": 20})`): right names in the wrong order accept
that call and do something else with it.

### 3. The runtime is a versioned product, not "some Spark"

**What Fabric does.** A Fabric Runtime pins Spark, Delta, Python, and a
preinstalled library set together as one versioned unit. A framework declares
which runtime it targets and assumes that floor.

**Where the emulator stood, and what changed.** Two images with different
Python versions and no statement about which Fabric runtime either claims to be.
The JVM overlay was built on a Spark image shipping **Python 3.8.10**, and
[Fabric Runtime 1.3 is **Python 3.11**](https://learn.microsoft.com/en-us/fabric/data-engineering/runtime-1-3)
— everything else in that image already matched the runtime it claims to be
(Spark 3.5, Delta 3.2, Java 11, Scala 2.12) and the interpreter did not.

Not cosmetic: `notebookutils` requires `>= 3.9`, so a notebook on that overlay
could not import the surface **at all**, which is what held contracts 1 and 2 red
there. The overlay now carries Python 3.11 in a virtualenv with the shim
installed, and `PYSPARK_PYTHON` points at it. PySpark itself needed no change —
Spark ships it as `pyspark.zip` on `PYTHONPATH`, so it is interpreter-agnostic.

**The assertion now exists.** Both images carry `ENV FABRIC_RUNTIME=1.3`, and
`e2e/conformance/fabric-runtimes.json` holds that runtime's floor with the
Microsoft page and its last-updated date. Two failures are distinguished
because they are different problems: an image that declares NOTHING cannot be
asked the question at all, and an image that declares a runtime and ships below
its floor answers it wrongly — which is worse, and is what happened here.

**Only Python is asserted.** It is the floor that actually broke. Whether the
engine behaves like Spark 3.5 is the engine matrix's question, and it answers
that row by row rather than by trusting a version string; Spark's reported
version is recorded in the findings for drift, not asserted.

The comparison is numeric, not textual, and that is not fussiness: `3.8` sorts
above `3.11` as a string, so a string comparison would have passed the exact
image that failed.

**The fix.** Declare the Fabric Runtime version each image targets, make the
Python floor match it, and have the engine matrix assert it. A runtime that
cannot meet the floor should say so at startup, not at the first import.

### 4. A success claim must be witnessed by the artifact

**The recurring defect.** The single most repeated finding, in four unrelated
places:

| Reported | Actual |
|---|---|
| CTAS built the gold tables | Engine wrote to its own warehouse; the lakehouse stayed empty |
| `saveAsTable("schema.table")` succeeded | Schema folder unmodelled; the write vanished into the engine warehouse |
| RunNotebook job `Completed` | Every cell still Pending |
| A concurrent fan-out reported success | Exit values landed in another session's globals |

Each is a *false green*, and each was invisible to the caller. This is the
failure class the emulator exists to prevent, because a consumer meets it for
the first time in production.

**The generalisation.** A success claim must be checkable against the artifact
existing where Fabric would have put it. The emulator already has the OneLake
listing needed to check. This is the principle `bindDefaultLakehouse` applies to
unqualified table names, applied everywhere a write can be redirected.

### 5. Concurrency is the default case, not the edge case

**What a framework does.** A pipeline-driven orchestrator fans out to tens of
child notebooks at once. On Fabric each gets its own container.

**Where the emulator stands.** One long-lived agent with many session namespaces,
so everything process-global leaks across runs: the prelude's exit-value global
(fixed), `/opt/wheels` installs (Environments now refuse a second bind), and
`runtime.context` from item 1. The Files mount is two-way at statement
boundaries and refuses a second lakehouse rather than switching; the single
`/lakehouse/default` path remains, which is 2c′ in
[37-runtime-fidelity-gaps.md](37-runtime-fidelity-gaps.md).

**The rule.** Every piece of agent state is session-scoped unless it is proven
shared. The shared-agent model is a legitimate emulator choice; letting it leak
is not.

**Now proven, on both engines.** Three notebooks are published, **all submitted
before any is polled** — serial submission would prove nothing, because the leak
only exists while two sessions are live in the same agent at once — and each
writes its own file under `Files/conformance/fanout/`. The probe compares each
child's reported notebook id against the id **the control plane issued that
child**, not against what another child said: two children that had leaked into
each other would agree with each other and disagree with the harness.

Markers and ids both, because they answer different questions. A marker proves a
child ran; the id proves it knew WHICH child it was. A child reporting another
child's identity is the leak, and it is invisible to any assertion that only
counts successes.

**And on the warehouse, where there is no notebook.** Three TDS sessions are
opened at once, each dialed at its own Warehouse item and logged in as its own
Entra object id, and — as with the notebooks — **all three are connected before
any of them executes**. That ordering is the test: the relay gives each caller
its own backend connection and its own database principal *precisely because* a
shared or pooled one lets identity drift between callers
(`internal/tds/principal.go` says so in its own header), and drift is only
possible while two sessions are live at once. Connect, run and close them one at
a time and the same code runs while the property goes unexercised.

Each session then reports the identity **the engine attributes to it** —
`DB_NAME()` and `SUSER_SNAME()`, SQL Server's answer, not the emulator's — and
that is compared against what the control plane issued: the item id it was
dialed at and the oid its token carried. Measured, the three answer
`<item>/<oid>` for three different items and three different oids. Afterwards a
**fresh connection per warehouse** counts the tables, because a write that
landed in the wrong warehouse is a leak no session would report about itself.

### 6. Engine gaps need a bounded-rewrite escape hatch, with a stated contract

**The pattern, now used three times.** A strict grammar recognises a bounded
statement form, routes it to a correct implementation (delta-rs), and lets
anything it does not understand fall through to the engine untouched. It covers
`OPTIMIZE` / `VACUUM`, `MERGE`, and CTAS (`python/spark_agent/delta_ops.py`).

The reason it keeps recurring is that engine gaps are not evenly distributed:
they cluster on the statements a medallion pipeline writes constantly. An upsert
into a table carrying audit timestamps is not an exotic query; it is the normal
shape, and an engine that cannot plan it makes practically every silver notebook
unrunnable.

**The generalisation.** Name this as a mechanism with a published contract —
strict grammar, honest fall-through, no silent approximation — rather than three
ad-hoc interceptors. It is the Spark-side sibling of what `internal/tsql` does
for T-SQL on the wire, and it deserves the same treatment: a documented grammar,
and a test that an unrecognised shape reaches the engine unmodified.

**Asserted on both engines — after a retracted finding.** This section
previously said the jvm cell could not be run, and concluded: *"the JVM overlay
cannot reach OneLake by path today."* **That was wrong, and it is worth keeping
the correction visible, because the wrong version was more interesting than the
right one and that is exactly what made it stick.**

What was real is the hang. The probe addressed the table by `abfss://`, and
hadoop's ABFS driver forces TLS for that scheme — `fs.azure.always.use.https=false`
downgrades `abfs://` and nothing else. Against a plaintext stack every request
failed at the socket with status `0`, hadoop-azure treated that as retryable, and
the notebook **hung**: five threads parked in `AbfsRestOperation.completeExecute`
for 324s to 864s, one per statement, accumulating and never exiting.

What was not real is the conclusion drawn from it. **The JVM overlay reaches
OneLake by path on every run, and always has.** Cell 0's `saveAsTable` commits
its Delta log to
`abfs://…@onelake.dfs.fabric.microsoft.com/…/Tables/events/_delta_log` — that is
what contract 4 confirms out of band, on this same backend, in the same
notebook. A single statement in `live.py` was spelling the scheme differently
from the rest of the stack. `bindDefaultLakehouse`, `livy_catalog` and `mlv.go`
all emit `abfs://`, commented in the source as *"the same string a user would
have written"*. The probe was the only outlier in the tree.

**A TLS terminator was then built to fix the wrong problem, and its failure was
the clue.** It did make the statements run — 4.6s / 2.8s / 1.5s / 2.5s where they
had hung — and it also regressed cell 0's write into the local read-only Spark
warehouse, for a reason that was never established. That unexplained second
failure was read as *"the fix has a further problem"*, and the fix was correctly
held back. It should have been read as *"the diagnosis is wrong"*: a repair that
breaks something it has no business touching is usually aimed at the wrong
target. The terminator is not in the tree and is not needed; nothing about the
topology, the compose files, or TLS had to change.

The correction is one string. With `abfs://`, contract 6 on jvm runs in seconds
and passes, and the engine's own log names the witness:
`MergeIntoCommand: DELTA: MERGE operation - Rewriting 1 files` — **Spark's**
Delta planner, not the agent's interception, which is the control column doing
precisely what it is here for. Zero retries in `AbfsRestOperation`.

**The gate stays.** `CONFORMANCE_FALL_THROUGH` is still opt-in per compose, and
for the reason it always was: a hang is worse than a red. These statements share
a notebook with contracts 1, 2, 3 and 4, so one hang takes five cells down and
reports the harness's own timeout as five separate defects, which happened
twice. What changed is which backends can answer, not whether the protection is
warranted.

**`OPTIMIZE <name>` still cannot resolve a table a notebook wrote**, and that
gap is unaffected by any of the above — see below. It is the reason the probe is
path-addressed in the first place.

**Asserted on the warehouse too, by a different witness — because that surface
has only one engine.** The JVM column is what makes the lakehouse cell provable:
one statement, two engines, opposite outcomes. The warehouse relays to SQL
Server and there is no second engine to contrast against, so a contrast cannot
be the witness there. Instead the engine is made to **return the bytes it was
given**.

The recognised leg is CTAS. SQL Server has no `CREATE TABLE … AS SELECT` — it
spells that `SELECT … INTO` — and refuses the form outright, measured as
`Incorrect syntax near the keyword 'SELECT'` when sent to it directly. So a CTAS
that succeeds *through the relay* can only have succeeded through
`internal/tsql`'s rewrite. That is what stops the second leg being vacuous:
everything falls through when nothing is intercepting.

The fall-through leg is an echo. The session sends
`SELECT '<a nested CTE>' AS payload` — a string literal containing the *exact*
construct the rewriter does recognise, which is the most tempting possible input
for a tokenizer that does not respect quoting — and compares what came back
against what was sent. A rewriter that flattened the literal changes those
bytes; one that stayed out of the way cannot. The emulator has no way to forge
this short of reproducing its own input verbatim, which is the same thing as not
having touched it.

**`OPTIMIZE <name>` cannot resolve a table a notebook wrote — on sail, and only
there.** This was recorded as a flat product limitation. It is not one, and the
probe now measures it on both engines every run rather than remembering it:

| engine | `OPTIMIZE events` | |
|---|---|---|
| sail | **fails** | `DeltaOpError: cannot resolve 'events' to a table location: it was not registered through the emulator, and the engine refused DESCRIBE DETAIL (IllegalArgumentException: invalid argument: found DETAIL at 9:15 expected 'FUNCTION', 'CATALOG', …)` |
| jvm | **succeeds** | — |

The reason is not that one engine is better at resolving names. It is that
**`delta_ops` is only installed on the Sail/Connect path at all** —
`agent.py` returns early unless `SPARK_REMOTE` is set, deliberately, because "on
the JVM overlay Spark runs these natively and interception would be a downgrade
— the JVM supports the full syntax (ZORDER, WHERE) that the delta-rs path
refuses." On jvm the question never arises: Delta resolves the name through its
own session catalog, which is where `saveAsTable` registered it.

So on the one engine where the interception exists, its `resolve()` has two
routes and a notebook-written table defeats both. `known_location()` is
populated by `register()` — which enumerates the lakehouse's `Tables/` when the
session opens, *before* cell 0 has written anything — and by
`CREATE TABLE … USING delta LOCATION` statements passing through, which a
`saveAsTable` is not. The fallback is `DESCRIBE DETAIL`, which Sail's grammar
does not have.

**The failure is at least a good one**, and that is by design rather than by
luck: `resolve()` announces the fallback before attempting it and owns the
error, so what the user sees names the cause and the remedy instead of surfacing
Sail's parse error about column 9 of a statement they never wrote. The module's
own comment explains why — "everything that went wrong around this code went
wrong quietly".

**Now closed, by a derivation that was already half-built.** `_SCHEMA_LOCATIONS`
was in the module and CTAS placement already derived `<schema location>/<table>`
from it; `resolve()` simply never consulted it. And `bindDefaultLakehouse`
issues `CREATE DATABASE IF NOT EXISTS … LOCATION '<abfs tables path>'`, so the
lakehouse's location *was already being stated to the engine* and then dropped —
`remember_stated_delta_location` matches only `CREATE TABLE … USING delta
LOCATION`. Capturing the database form and deriving from it resolves `events`
with no DESCRIBE at all. Measured after the change: **`OPTIMIZE events` succeeds
on sail**, and the fallback is never even announced.

**The derivation is checked, not guessed**, which matters more than the feature.
A derived path that is not actually a Delta table is *not an answer* —
`is_deltatable` decides, so nothing is inferred from a name alone, and a miss
falls through to the engine exactly as before. And **ambiguity is refused rather
than ranked**: two lakehouses can both hold a `customers`, and picking one would
hand a notebook another lakehouse's data under its own table name — the
cross-lakehouse leak `catalog.Claims` guards the unqualified path against. Two
hits raise and tell the author to qualify.

The probe still records both engines' numbers every run, so the claim stays a
measurement rather than reverting to a memory.

### 7. Credentials must outlive the run

A token minted at container start expired an hour into a run, and every OneLake
read then failed `401` until someone restarted by hand — which reads as a storage
outage. Fixed for one engine by keeping the launcher resident and re-minting.

**The generalisation.** Any credential the emulator hands to an engine needs a
refresh path, because real runs outlive token lifetimes. This is a property of
the compute surface, not of one launcher.

**Asserted by making the clock cheap rather than the test slow.**
`TOKEN_LIFETIME_ACCESS_SECONDS=60` on the conformance entra-emulator, and the
notebook writes to OneLake through the engine 75 seconds after the session
started — past a lifetime the original defect took an hour to cross.

**The wait must actually exceed the lifetime, and the probe checks that.** A
probe that slept less than the token lived would pass on a runtime that never
re-mints, which is the exact defect. The session reports both numbers and the
harness refuses to grade a run where the gap was never opened. The read *before*
the wait is the control: without a working baseline the second operation says
nothing either way.

**One setting covers every audience**, so shortening it also shortens the
harness's own token — which is why `live.py` now caches and re-mints at two
thirds of advertised life. A client that minted once and then polled a job for
minutes would 401 partway through and report a broken pipeline instead of a
short token. That is the same argument this contract makes about the engine, one
tier up.

**On the warehouse, and with the leg that makes it mean anything.** A TDS
session logs in with an Entra token and then lives as long as the client keeps
it. The probe mints a token with an explicit 60-second lifetime, pins **one**
physical connection so the pool cannot answer the question by quietly opening a
second, runs a baseline query, waits 75 seconds, and writes. It succeeds.

But *"the session kept working past its lifetime"* and *"nothing ever checks a
credential"* produce the identical green, and the second is a security hole
wearing the first's result. So a final leg presents an **already-expired** token
on a fresh connection and requires it to be **refused**. Without it this cell
would pass just as happily against an endpoint that validated nothing.

That leg also has to be aimed outside the validator's clock skew, and the first
draft was not. `internal/auth` allows 60 seconds of skew, as every real JWT
validator does; a token expired by exactly 60 seconds therefore sits *on* the
boundary and is legitimately accepted. Reading that as *"nothing is checking
expiry at all"* was the probe's error, not the emulator's. Expired by ten
minutes it is refused, and the two measurements together — `-60` accepted,
`-600` refused — are what show the leg discriminates rather than merely
returning the answer that was wanted.

**A finding this contract surfaced, now explained — and it contradicts a claim
in the tree.** The second operation was originally a re-read of the table cell 0
wrote. It failed with `TABLE_OR_VIEW_NOT_FOUND` while the read *before* the wait
succeeded: a `saveAsTable` catalog entry gone 75 seconds later, in the same
session, on **sail**. That was recorded here as unexplained. The log says what
happens:

```
[07:50:47Z] creating session 30c97a1b-…
            launcher: restarting with fresh Storage token … (expires_in=60s, refresh in 59s)
[07:51:43Z] Starting the Spark Connect server on 0.0.0.0:50051…
[07:52:02Z] creating session 30c97a1b-…        <-- same id, new engine process
```

(The launcher line carries no timestamp of its own; it is printed by the
supervisor immediately before it re-execs sail.)

**Sail's credential refresh is implemented by restarting the engine.** The token
enters sail only through startup env, so re-minting requires a new process
(`docker/sail/launcher.py` explains exactly this, and the resident supervisor is
the right fix for the 401-after-an-hour defect it was written for). The restart
discards the engine's session state. Sail then re-creates the session under the
*same id* on the next statement, so the client never sees a lost session — it
sees a session that has forgotten its own table.

That last part is why it was hard to read, and why it matters more than a
missing table. `python/spark_agent/session_recovery.py` exists precisely to stop
this: it detects a dropped engine session and **refuses to rebind silently**,
because *"a 'transparent' reconnect hands the user a notebook that has quietly
forgotten its temp views — the same failure wearing a friendlier face, and
harder to diagnose than the error it replaced."* A launcher restart never trips
those markers, because nothing reports the session as not running. The loss goes
through unannounced, which is the outcome that module was written to prevent.

The two claims cannot both be true. `launcher.py` states that *"Sail holds no
state a restart loses that the control plane does not re-establish — the Spark
agent re-registers catalog entries per session"*; `session_recovery.py` states
that rebinding costs temp views, cached DataFrames and session-scoped conf. The
measurement is on the second module's side.

**Scope, honestly.** With the default 3600s lifetime this happens once an hour
rather than once a minute, which is why nothing had caught it; the conformance
stack's 60s token is what made it visible in a single run. Contract 7 still
passes and passes fairly — the probe writes a *fresh* table, which needs the
engine's credential and nothing that must survive the wait, so it measures the
credential rather than the catalog.

**The restart cannot be removed, so the fix makes it attributable.** Sail reads
its Storage bearer once, through startup env (`MicrosoftAzureBuilder::from_env`),
which [20-lakesail-engine.md](20-lakesail-engine.md) already records as "the one
thing the Sail side cannot do" — refresh without a restart. So the agent now
RECOGNISES the aftermath instead of passing it on as a missing table:
`session_recovery.forgotten_table_in()` matches the error **and** asks whether
that table actually exists. Both halves are required, and the second is the one
that matters: a typo is not in the lakehouse and is left completely alone, while
a table that IS in the lakehouse and which the engine cannot see can only mean
the engine went away.

**And the condition is demonstrated, not argued.** The probe now reads the same
table twice after the wait — once by catalog name, once by path — and records
both:

```
reread_ok     False    spark.read.table("events")
path_read_ok  True     spark.read.format("delta").load("abfs://…/Tables/events")
```

Same cell, same session. The bytes are exactly where cell 0 put them; only the
engine's session catalog went away. That is the whole finding in two numbers,
and it is what justifies the oracle below asking storage rather than reasoning
about registrations.

**Asking the lakehouse, not just the bookkeeping, is the part that took a
correction.** The first version consulted only what the agent had registered —
`delta_ops`'s recorded locations and the control plane's `/register` payload —
and that misses the exact case this fix exists for. A notebook's
`saveAsTable("events")` on a fresh lakehouse is in neither: `register()`
enumerated the lakehouse *before* the write happened, and a DataFrameWriter call
is not a statement the agent's `sql` wrapper ever sees. So it would have
answered "no" for precisely the table whose disappearance prompted the fix.
Storage is also the semantically right question — "the table is in the lakehouse
and the engine cannot see it" *is* the condition, stated rather than inferred. The session's registrations are
then replayed from the last `/register` payload, and the note says what did
*not* come back — temp views, cached DataFrames and session-scoped conf, which
lived in the process that exited.

The error text this matches is **measured, not recalled**. The contract-7 probe
records it verbatim on every run (`reread_error`), and a first attempt at the
matcher — written against a half-remembered message — silently extracted the
word `"The"` from Spark's phrasing. The two real shapes are:

```
sail   AnalysisException: Table not found: [TABLE_OR_VIEW_NOT_FOUND]
       Table or view not found: events
spark  [TABLE_OR_VIEW_NOT_FOUND] The table or view `events` cannot be found.
```

**What this fix does not have is an end-to-end witness**, and neither does the
lost-session recovery it sits beside: both are unit-tested only. A real one
would need a notebook cell that lets the failure propagate rather than catching
it, because the annotation is applied to the statement envelope and a cell that
handles its own exception never produces one. Said here rather than left to be
assumed from the fact that the behaviour is covered.

---

## The conformance kit

**Status: built; contracts 4 and 1 are live.** The harness, the committed
matrix, and the offline checker are in tree. Sail, JVM, and warehouse each
write through the emulator path and an out-of-band reader confirms the
artifact. Items 3 and 5–7 are still individually tractable. The reason
they existed for months is that nothing exercised them, and that is the
gap worth closing first — a new framework will find a new one next week
otherwise. Contract 1 found two on its first run, and contract 2 a third.

**Contracts share a run but must not share a failure.** Contract 1 rides the
same notebook as contract 4, which costs nothing and describes one session
rather than two. Twice while wiring it, a contract-1 failure failed the *job*
and turned contract 4 red — with the table landed and the out-of-band reader
seeing it. Its cell is guarded whole, imports included, and the absent
artifact is the failure signal. The rule generalises to every contract that
joins this run.

**A red must point at its cause.** A missing artifact says only `404`. The
notebook prints why it could not write one, and the harness quotes that line
into the cell, so the jvm red reads `ModuleNotFoundError: No module named
'notebookutils'` rather than leaving a reader to guess.

### What it is

A suite that asserts the **runtime contracts a framework depends on**, expressed
as a Fabric product would exercise them, not as unit tests of the emulator's
internals. It is deliberately *not* another medallion example: the examples prove
a pipeline runs, which is a different claim from proving the runtime answers
correctly when probed.

### What it asserts

| # | Contract | Assertion |
|---|---|---|
| 1 | Context chain | Each link resolves independently: `env.getWorkspaceId()`, `runtime.context`, and the env fallback each return the *running* notebook's identity, with no variable set out of band |
| 2 | Signature shape | Every parameter real Fabric accepts is present on the `notebookutils`/`mssparkutils` surface, pinned against the reference |
| 3 | Runtime floor | The image declares a Fabric Runtime version, and its Python meets that runtime's floor |
| 4 | Write landing | Every write path (`saveAsTable` qualified and unqualified, CTAS, MERGE, mount write-back) is followed by a OneLake listing proving the artifact is where Fabric puts it |
| 5 | Concurrent isolation | N sessions live at once, each reporting its own identity: notebook children each see their own context; TDS sessions each answer with their own warehouse and principal |
| 6 | Fall-through | A statement the rewrite grammar does not recognise reaches the engine unmodified — proven by a second engine that *can* plan it, or, where there is only one, by the engine echoing the bytes back |
| 7 | Credential lifetime | A run that outlives the token lifetime keeps working, on a surface that still refuses an expired credential |

### Every contract proves real execution, on a real backend

An assertion that passes against a stub proves nothing, so each contract is
proven by *executing* on the backend it concerns:

- **Warehouse** — a real SQL Server, reached over TDS.
- **Lakehouse** — Parquet/Delta in OneLake, written by **Sail** and by **JVM
  PySpark**, because the two engines fail differently and one is the other's
  control.

**The rule that makes it real: the engine that wrote must not be the one that
confirms.** Every false green in [the table above](#4-a-success-claim-must-be-witnessed-by-the-artifact)
happened because success was reported by the component doing the work — the
engine's own catalog said the table existed, in the engine's own warehouse. So
each assertion has two halves: execute through the emulator's real path
(RunNotebook for Lakehouse, TDS for Warehouse), then **verify out of band** —
read the Delta back through delta-rs / the OneLake DFS API, and read the
Warehouse table through a fresh TDS connection. Both readers already exist as CI
jobs (`e2e/delta-rs`, `e2e/warehouse-tds`); this composes them rather than
inventing anything.

<!-- APPLICABILITY:BEGIN (scripts/check_conformance.py parses this table) -->

| # | Contract | sail | jvm | warehouse |
|---|---|---|---|---|
| 1 | Context chain | required | required | n/a |
| 2 | Signature shape | required | required | n/a |
| 3 | Runtime floor | required | required | n/a |
| 4 | Write landing | required | required | required |
| 5 | Concurrent isolation | required | required | required |
| 6 | Rewrite fall-through | required | control | required |
| 7 | Credential lifetime | required | required | required |

<!-- APPLICABILITY:END -->

`n/a` is honest, not a hole: contracts 1–3 are properties of a notebook session,
and the Warehouse surface has none. `control` is the interesting verdict —
**the JVM column is what makes contract 6 provable at all.** The delta-rs
rewrites exist because Sail cannot plan `MERGE` against a temporal-columned
target; on JVM the engine *can*, so the grammar must be proven to stay out of the
way. The same holds for `input_file_name()`: shimmed on Sail, native on JVM. A
single-engine suite cannot tell "the rewrite worked" from "the rewrite was not
needed and fired anyway".

Which raises the obvious question about the warehouse, where contract 6 is
`required` and there is no second engine to be the control. The answer is that
the contrast is not the only possible witness, just the only one available on
the lakehouse: the warehouse instead makes the engine **echo back the statement
it was given**, with the recognised construct quoted inside it as a literal.
That is not a weaker form of the same test — it is a direct observation where
the lakehouse can only manage a differential one.

### CI realisation

One job, one matrix, three backends:

```yaml
conformance:
  name: Framework conformance (${{ matrix.backend }})
  strategy:
    fail-fast: false
    matrix:
      backend: [sail, jvm, warehouse]
  runs-on: ubuntu-latest
  timeout-minutes: 45
```

Three things keep this from being a new tax:

- **The JVM image is already built on every push.** `engine-matrix` runs all
  three engine profiles per push. Reusing the same `actions/cache` key on
  `docker/spark-runtime/jars` means the Maven fetch costs once per `jars.txt`
  change, not once per run. (Worth correcting a nuance: JVM *probes* do run per
  push. What has never run per push is the emulator composed **over** the JVM
  overlay — which is exactly what a conformance leg fixes.)
- **SQL Server is about ten seconds** on the ubuntu leg, and `warehouse-tds`
  already uses the `services:` form with a healthcheck.
- The workflow's `concurrency` group already cancels superseded runs.

### A committed matrix, not a pass/fail suite

Model the output on `engine-matrix`, not on an ordinary test job: regenerate
`docs/conformance-matrix.md` from the run and fail if it differs from what is
committed.

```yaml
- run: uv run --frozen --no-sync python e2e/conformance/run.py --backend ${{ matrix.backend }}
- run: git diff --exit-code docs/conformance-matrix.md
```

That buys three things a green/red suite cannot:

1. **The kit can land before every contract passes.** Contract 1 fails today.
   A pass/fail suite could not merge until it is fixed; a matrix lands with the
   cell ❌ and a pointer, which is this document's "record it as a known gap"
   rule made executable.
2. **No cell may regress silently** — the same mechanism that stops a stale
   engine-gap claim surviving an upgrade that closed it.
3. **A cell that starts passing forces the doc to change**, so the map cannot
   drift stale in the optimistic direction either.

### Gating, and what `make check` enforces

Two conventions the repo already uses:

- **Arm on capability, not on platform.** The coverage floor keys on
  `WAREHOUSE_MSSQL_DSN` being present precisely so it self-arms when a leg gains
  an engine. A backend that is not reachable records `gated` and emits a loud
  `::warning::` — never a silent pass. That is also how macOS is handled, where
  a containerised SQL Server is documented as unsolved.
- **Register witnesses.** `ci:conformance-sail`, `ci:conformance-jvm` and
  `ci:conformance-warehouse` belong in [witnesses.json](witnesses.json), so
  `check_witnesses.py --strict` catches a contract being deleted out from under a
  parity claim — the exact failure that let the Environments row claim
  provisioning it never performed.

The regeneration needs live backends, so it stays in CI. `make check` enforces
the half that is checkable offline, via `scripts/check_conformance.py`: the
applicability table above lists every contract this document defines and no
others, and — once the matrix exists — every `required`/`control` cell carries a
verdict, every ❌ carries a pointer, and every witness it names is real. That is
the same division the other invariant scripts use: the expensive proof runs in
CI, the correspondence between doc and artifact is enforced everywhere.

### Write the assertions once

Parameterise by backend rather than shipping three suites that drift, exactly as
`e2e/engine-matrix` runs one `probes.py` under different profiles. A divergence
then surfaces as a differing cell instead of as two suites that quietly stopped
testing the same thing.

### Shape

A notebook-driven suite, run through the emulator's own job API rather than
against the agent directly, because the contracts are about what a *notebook*
sees. It should read as the framework code it stands in for: probe, introspect,
fall back, assert. Each assertion names the contract it covers, so a failure says
which promise broke rather than which line threw.

Where a contract is not yet met, the suite records it as a **known gap with a
pointer** rather than being deleted or skipped silently — the same discipline
[the witness map](witnesses.json) applies to parity claims. A suite that only
tests what already works cannot tell you what a new consumer will hit.

### Size

**M.** No engine work and no research risk: it is notebooks, assertions, and
three CI jobs. The value is entirely in items 1, 4 and 5, which are the three
that produced false greens rather than loud failures.

**Contract 4 (write landing) is wired on all three backends.** It is the one
that produced silent wrong answers rather than loud failures, and it is the only
row where the out-of-band verification pattern has to be got exactly right. 5
and 6 are the same harness with different notebooks, and 1–3 are cheap
assertions inside a session that is already running.

### Why not extend the medallion examples instead

The examples answer "does a pipeline run end to end", and they answer it well
enough that four of them run in CI. They cannot answer "does the runtime respond
correctly to a framework that probes it", because a pipeline that works never
probes: it calls the paths that happen to be implemented. Every defect in this
document sat underneath four passing medallion examples.

## What must NOT be done

- **Do not name a consumer.** This repo is agnostic by design: it emulates
  Fabric, not anyone's product. A finding is stated as the Fabric contract it
  violates, and a fixture is the generic shape of the thing, never a customer's.
  The kit is worth building precisely because it turns "we fixed what one product
  hit" into "we know what any Fabric product needs".
- **Do not satisfy a contract only at the last link of a fallback chain.** A
  framework that stops at link one never reaches it, and the emulator looks
  broken in a way no test reproduces.
- **Do not delete or skip an assertion that fails.** Record it as a known gap
  with a pointer, or the suite becomes a list of things that already work.
- **Do not add a signature parameter that real Fabric rejects.** Being more
  permissive than the thing being emulated is the one direction that actively
  misleads: it passes here and fails there.
