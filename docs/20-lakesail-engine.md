# 20 — LakeSail default compute and the JVM compatibility oracle

**Decision: use [LakeSail's Sail](https://github.com/lakehq/sail) as the
default engine behind the emulator's compute surfaces.** Sail is a Rust Spark-Connect
server: unmodified `pyspark` clients connect over `sc://…`, and no JVM exists
in the default stack. It is a Spark-compatible engine, not Apache Spark and not
a complete emulation of the Microsoft Fabric runtime.

Grounded against Sail **v0.6.6** (local checkout audit; citations are that
repo's paths).

## Why

- **The JVM is our heaviest dependency.** The `apache/spark:3.5.5` stack costs
  gigabyte images, tens-of-seconds startup, ABFS driver jars, and a custom
  Java `EntraTokenProvider` — all for engines whose *client code* is plain
  PySpark. Sail is one Rust binary in a `python:slim` image; sessions start in
  seconds.
- **Storage-layer parity is already proven.** Sail reads/writes cloud storage
  through Rust `object_store` — the *same stack delta-rs uses*, and the
  delta-rs e2e (A1) already round-trips Delta against our Blob surface,
  including the `If-None-Match: *` conditional PUT that Sail's Delta commits
  require (`sail-delta-lake/src/delta_log/store.rs` uses `PutMode::Create`).
  Our R0 work is exactly the contract Sail needs.
- **Native Delta.** Sail implements Delta in Rust (`crates/sail-delta-lake`):
  snapshot reads, append/overwrite, partitioning, schema evolution, time
  travel, `MERGE INTO`/`DELETE`, deletion vectors. No delta-spark jars.
- **The seam was built for this.** The emulator's Go layer terminates the Livy
  protocol and drives a dumb statement-executor agent (`FABRIC_SPARK_AGENT_URL`).
  The agent's only Spark coupling is one line: `SparkSession.builder`. With
  Spark Connect that line becomes `.remote(os.environ["SPARK_REMOTE"])` — the
  agent body, the notebook runner, and every e2e driver stay as-is.

## The wiring (validated by `e2e/sail`, milestone S0)

```
pyspark client ──sc:// (Spark Connect, h2c gRPC :50051)──▶ sail server (Rust)
                                                             │ object_store (az://)
                                                             ▼
                            fabric-emulator Blob surface  /onelake/{ws}/{item}/…
                                                             │ validates bearer
                                                             ▼
                                                        entra-emulator (JWKS)
```

- **URL form matters.** Sail selects its Azure backend by URL shape
  (`sail-object-store/src/registry.rs`): `az://{workspace}/{item}/…` +
  `AZURE_STORAGE_ENDPOINT={fabric}/onelake` (the account-prefixed path form,
  azurite-style) or a host matching `*.dfs/blob.fabric.microsoft.com` (DNS
  alias). A bare `http://host:9443/…` URL silently degrades to the generic
  HTTP store — never use it.
- **Auth is per-process env, minted before start.** `MicrosoftAzureBuilder::from_env()`
  reads credentials once; there is no per-session channel and
  `spark.conf.set("fs.azure.*")` is stored but ignored. Our
  [`docker/sail`](../docker/sail) image therefore runs a launcher that
  performs a real client-credentials grant against entra-emulator and exports
  `AZURE_STORAGE_TOKEN` before exec'ing `sail`. (`AZURE_STORAGE_AUTHORITY_HOST`
  + client id/secret is the pure-object_store alternative — its token URL
  shape `{authority}/{tenant}/oauth2/v2.0/token` is exactly entra's — worth
  switching to once validated against TLS trust.)
- **No official Sail image exists** — `docker/sail/Dockerfile` follows
  upstream's quickstart (`pip install pysail`, prebuilt manylinux/macOS
  wheels; `pyspark-client` alongside because the server reads
  `pyspark.__version__` for its compat table). Containers must bind
  `--ip 0.0.0.0`; the Connect endpoint is plain h2c with no auth/TLS — keep it
  on internal networks.

## Migration plan

| Milestone | What changes | JVM removed |
|---|---|---|
| **S0** ✅ | `e2e/sail`: PySpark client → Sail → Delta write/read/append + SQL over our OneLake plane, entra-authenticated. CI job `sail`. | — (proof) |
| **S1** ✅ | `e2e/livy` + `e2e/dbt-fabricspark`: agents are `SPARK_REMOTE`-capable; composes swap `apache/spark:3.5.3` → `sail` + `python:slim`. Same Livy REST surface, same dbt project. | 2 suites |
| **S2** ✅ | `e2e/notebook-run`: `runner.py` connects via `SPARK_REMOTE`; the JVM image build is gone. The notebook fixture (incl. `createDataFrame`) runs **unmodified**. | 1 suite |
| **S3** ✅ | `e2e/spark` (A2) reborn on Sail: same job, same production-shaped `abfs://` URLs (endpoint override routes them). `EntraTokenProvider.java` + the JVM Dockerfile deleted. | last one |
| **S4** ✅ | user-facing compose: the auto-loaded `docker-compose.override.yml` **and** the explicit `docker-compose.compute.yml` both run Sail + the statement agent — `RunNotebook`, Livy sessions, and dbt work out of the box with no JVM. Docs + quickstart updated. | — |
| **S5** ✅ | restore an opt-in Apache Spark 3.5.5 + Delta 3.2 JVM oracle (`e2e/spark-jvm`) and run the representative notebook on both engines. Sail remains the default; JVM and real-Fabric qualification run on slower CI cadences. | compatibility only |

## What “parity” means

Compute claims are split into three evidence tiers:

| Tier | Engine | What a pass establishes |
|---|---|---|
| Default | Sail 0.6.6 + PySpark Connect 4.2 | Spark Connect DataFrame/SQL behavior, native Delta behavior, OneLake paths, auth, Livy and notebook integration |
| JVM oracle | Apache Spark 3.5.5 + Delta 3.2 + Java 11 | Fabric Runtime 1.3-aligned Spark Core/JVM behavior and Hadoop ABFS compatibility; scheduled/manual, not part of the default stack |
| Release oracle | real Microsoft Fabric | representative notebook and API conformance in the managed production runtime; secret-gated and run before releases |

Passing the Sail tier does not establish compatibility for SparkContext/RDD,
Py4J, Java or Scala libraries, Structured Streaming, table maintenance,
transaction semantics beyond the probed overwrite conflict, cluster
configuration, or performance.

## Notebook code compatibility (probed, not guessed)

`e2e/sail` probes the surface unmodified Fabric notebook code actually
touches; every claim below is CI-verified against the emulator:

| Notebook pattern | On Sail | Evidence |
|---|---|---|
| `abfss://ws@onelake.dfs.fabric.microsoft.com/…` (production URL form) | ✅ works unmodified — Sail parses the Hadoop form and the endpoint override routes it; **no path shim needed** | e2e probe |
| Delta write/read/append, SQL over temp views | ✅ | e2e |
| Time travel (`option("versionAsOf", n)`) | ✅ (SQL `VERSION AS OF` is a Sail gap) | e2e probe |
| `MERGE INTO` | ✅ registered table (`CREATE TABLE … USING delta LOCATION`); path-based ``delta.`…` `` through delta-rs on the Livy path. Bare Sail still cannot plan a local-path MERGE (`e2e/sail` is the engine witness) | e2e probe + engine-matrix |
| `sc` / RDD API / `spark._jvm` | ⚠️ **measured subset emulated** — Spark Connect has no SparkContext on any engine, so the agent binds a local facade for exactly the idioms Fabric code was measured to use: `setLogLevel`, the `parallelize` chain, and `org.apache.log4j`. Everything else raises a pointer. It is eager and LOCAL and refuses above 10,000 elements rather than pose as distributed. **Fidelity inversion vs real Fabric** remains for the rest of the API — `mapPartitions` and friends work in production and refuse here — and the **JVM overlay** (`docker-compose.spark-jvm.yml`) restores the whole thing. See [50-rdd-usage-capture.md](50-rdd-usage-capture.md) for the sweep that sized this | agent facade + unit tests; contract verified against real Spark in `e2e/spark-jvm` |
| `createDataFrame(local_rows)` | ✅ works — the agents/runners set `spark.conf.set("spark.sql.session.localRelationSizeLimit", <int>)` at session start, overriding the `'3GB'` string pyspark 4.2 chokes on; without that mitigation, use SQL `VALUES` or a 3.5 client | e2e (notebook fixture runs unmodified) |
| DML row-count results (`INSERT`/`MERGE` envelopes) | ⚠️ DataFusion reports counts as `uint64`, which Arrow conversion to Spark clients rejects — the statement HAS executed; the Livy SQL agent absorbs this specific error as an empty result | dbt e2e finding |
| Structured streaming, `OPTIMIZE`/`VACUUM`, Java/Scala UDFs | ❌ absent in Sail v0.6.6 (`OPTIMIZE`/`VACUUM` are intercepted and run through delta-rs, so those two work on the default stack). **Streaming and Java/Scala UDFs are restored by the JVM overlay** | executable negative probes |
| CDF options, `spark.jars` | ⚠️ on **bare Sail** CDF `readChangeFeed` is accepted but inert (`e2e/sail`). Through the Livy agent the notebook API (write enable + read) is intercepted via delta-rs, announced, and materialised. JAR config is stored but there is no JVM classloader. **JARs real on the JVM overlay** | executable divergence probes + engine-matrix |
| `read.json(multiLine=True)` | ✅ on the Livy / sail-delta path: the named option is wrapped, announced, and materialised (`createDataFrame`). Bare Sail is NDJSON-only. Native lazy read is the **JVM overlay** | engine-matrix |

## Known gaps to design around (Sail v0.6.6)

- **Concurrent overwrite conflicts are rejected** at the Delta-log commit
  boundary: the executable two-session probe observes one successful writer
  and one transaction failure through OneLake's conditional create contract.
  The loser fails **fast, not eventually** — worth stating because a hung
  `sail` job was once read as an unbounded commit retry, and it is not one.
  Sail commits under `for attempt_number in 1..=total_retries`
  (`crates/sail-delta-lake/src/transaction/mod.rs`, v0.6.6), and an overwrite
  is neither a table creation nor a blind append, so its
  `effective_max_retries` is **0**: one attempt, then `MaxCommitAttempts`.
  Measured at 50 ms from conflict to both sessions closed. The emulator's half
  of that contract is Azure's own code — a Put Blob carrying `If-None-Match: *`
  against an existing commit answers **409 `BlobAlreadyExists`**
  (`internal/onelake/blob.go`), which object_store maps to `AlreadyExists` and
  does not retry, 409 being a client error outside its retry policy. Terminal
  on both sides.
- **No streaming** (`readStream`/`writeStream` absent), `cache()`/`persist()`
  are no-ops, no Java/Scala UDFs (Python/Pandas/Arrow UDFs all work), some
  catalog calls missing (`cacheTable`, `refreshTable`, …).
- **One credential set per server process** — per-workspace identity means one
  Sail container per identity if we ever need isolation; today the daemon
  SP's Storage token covers everything the e2es do.
- Session timeout defaults to 900s — our composes set
  `SAIL_SPARK__SESSION_TIMEOUT_SECS=3600`.

## The 24-minute hang, and why one is legible now

The `sail` job once produced no output for 24 of its 25 minutes and was
reported as **CANCELLED**. That is worse than a failure: a cancelled check
reads as infrastructure noise, so it was rerun rather than investigated, the
rerun went green, and nothing was learned.

**The cause was in the client, not in Delta, Sail or the emulator.** The
concurrent-writer probe stops its two extra Spark Connect sessions, and
`SparkSession.stop()` calls `client.close()`, whose first act is
`ExecutePlanResponseReattachableIterator.shutdown()` — which is **process-wide,
not per session**. It takes a class-level lock, hands the class-level release
thread pool to `ThreadPoolExecutor.shutdown()` (`wait=True` by default), and
holds the lock until the pool drains. Every new query in the process builds one
of those iterators, and its `__init__` takes that same class lock. So one worker
thread's `stop()` blocks the main session's next query — which is precisely
where the CI log falls silent — for as long as the outstanding `ReleaseExecute`
RPCs take to retry, an interval pyspark's own retry policy is documented to let
run "at least 10 minutes". A **losing** writer is the case that leaves one
outstanding, because its execution ends in an error that submits `_release_all`
just as the session is torn down: hence intermittent, and hence only on the run
where the race actually collided.

Measured, not inferred: driving the shipped pyspark classes with no server at
all, one thread inside `shutdown()` blocks another thread's
`_get_or_create_release_thread_pool()` for the full drain. `e2e/sail/driver.py`
now calls `session.client.release_session()` instead — the same `ReleaseSession`
RPC, so Sail still logs the removal, and no shared state is touched.

Two things had to be untangled before that was findable, and they are worth
keeping regardless. Three guards, in the order they fire:

1. **The driver prints as it happens.** It did not before. Python
   block-buffers stdout to a pipe, so in the run that *passed* all twenty of
   the driver's lines carry one timestamp, flushed at exit — meaning a killed
   run prints nothing at all, and its silence locates nothing.
   `PYTHONUNBUFFERED=1` in `docker/python-runtime/Dockerfile` fixes this for
   every driver built on that image, not just this one.
2. **A stuck step ends the run and names itself.** Every Spark Connect call is
   a gRPC round trip with no deadline, so nothing in the driver is bounded on
   its own. `e2e/sail/driver.py` names each step, heartbeats every 30 s, and
   after `SAIL_E2E_STEP_BUDGET` (180 s; steps take under a second in practice)
   prints what it was waiting on, dumps **every thread's** stack, and exits 1.
   Set the budget to a fraction of a second to watch it fire — a watchdog that
   cannot be made to fire is not evidence of anything, and the first draft of
   this one could not be.
3. **A stack that hangs before the driver can speak** — a stalled build, a Sail
   that never listens — hits `SAIL_E2E_BUDGET` (900 s) in `e2e/sail/run.py`,
   which then dumps the driver, sail and emulator logs.

All three exist so the job **fails**, inside its budget, with the failing step
named. None of them changes what the suite asserts.

## Running it (user-facing)

```bash
# the family with Sail compute attached (override auto-loads):
docker compose up
# same stack with explicit files (naming -f skips the auto-override):
docker compose -f docker-compose.yml -f docker-compose.compute.yml up
# contract-only, no compute sidecars:
docker compose -f docker-compose.yml up
```

Then connect any PySpark client with no JVM installed:

```python
# pip install "pyspark-client==4.2.0"
spark = SparkSession.builder.remote("sc://localhost:50051").getOrCreate()
spark.sql("SELECT 1").show()
```


## Delta maintenance on Sail: `OPTIMIZE` and `VACUUM` via delta-rs

Sail's SQL planner has no `OPTIMIZE` or `VACUUM` — it answers `found OPTIMIZE
at 0:8 expected something else`. The statement agent closes that gap without a
JVM by recognising exactly those statements and running them through
**delta-rs**, a real Delta Lake implementation, against the same table.

Verified end to end: three appends produce three data files; `OPTIMIZE`
compacts them to one with every row preserved; `VACUUM` then removes the three
superseded files and the table still reads back correctly.

```sql
OPTIMIZE delta.`abfss://ws@onelake.dfs.fabric.microsoft.com/lh.Lakehouse/Tables/t`;
VACUUM   delta.`…` RETAIN 168 HOURS;   -- and DRY RUN
```

### What is real, and what differs

The outcome is genuine — real Parquet, a real `_delta_log`, files actually
compacted and actually deleted. **What differs is the executor**: the emulator
performs the operation, not the Spark engine, so nothing appears in a Spark
job listing. That is a deliberate, documented divergence. The alternative is an
honest failure, which helps nobody testing a notebook that calls `OPTIMIZE`.

### Credentials on the OneLake path

delta-rs reaches OneLake **on its own account**, not the Spark session's. A
Spark Connect client cannot read back the bearer the server holds — no such API
exists, in Sail or in Apache Spark's own Connect client — so `python/spark_agent/storage.py`
performs its own client-credentials grant against the same issuer with the same
Storage audience. delta-rs therefore authenticates as the same principal Sail
does, without either side reading the other's token.

The two sides take credentials in opposite shapes, and that drives the design:

| | Sail | delta-rs |
|---|---|---|
| Where credentials come from | process env, read once at startup (`MicrosoftAzureBuilder::from_env`) | `storage_options` on every call |
| When they can change | never — the launcher mints before `exec sail` | per statement |

So `install()` takes the resolver **as a callable**, not a dict: every
intercepted statement resolves a current bearer, and a refreshed token needs no
restart — the one thing the Sail side cannot do.

Verified end to end by `e2e/livy` against an `abfss://…onelake…` table, with a
negative control: the same OPTIMIZE with `storage_options={}` must be *refused*,
so a pass cannot mean "OneLake lets anyone in".

Getting there surfaced a real emulator bug worth recording, because it only
appears on this path. The Blob endpoint's `List Blobs` ignored **`startFrom`**,
the offset parameter object_store uses for `list_with_offset`. delta-rs's
`get_latest_version()` then received a log segment starting at version 0 when it
had asked for one starting at N, which the kernel rejects as `Invalid table
version: N`. Plain writes passed throughout — only the commit-conflict paths
(OPTIMIZE, VACUUM, MERGE) re-read the log from an offset, so only they broke.
Azure's `startFrom` is **inclusive**, unlike S3/GCP's exclusive `start-after`;
`TestBlobListStartFrom` pins that.

Deliberate limits:

- **`ZORDER` and `WHERE` are refused, not ignored.** They change *what* gets
  compacted; quietly running a bare compaction instead would be a silent
  semantic change. The error names the clause and points at the JVM overlay,
  which supports the full syntax.
- **`RETAIN` fractions round down.** delta-rs takes whole hours; retaining
  *less* than asked would delete files the user wanted kept, which is the
  unrecoverable direction.
- **Change Data Feed is intercepted on the notebook API**, announced on
  stderr. A Delta write with `delta.enableChangeDataFeed`, later appends to
  that table, and a read with `readChangeFeed` go through delta-rs. The
  result is a materialised DataFrame (`createDataFrame`), not a lazy scan —
  `.explain()` is a LocalRelation. `spark.delta_change_feed(uri)` remains as
  the named helper. Bare Connect without this module (`e2e/sail`) still
  accepts `readChangeFeed` and returns a snapshot with no `_change_type`;
  Sail's own writer still cannot enable the feature.
- **`read.json(multiLine=True)` is intercepted on the Connect reader**,
  announced on stderr. Sail's JSON reader is NDJSON-only; the wrap parses a
  JSON array (or object) on the driver and materialises. Plain `json()` and
  `multiLine=False` stay on the engine. Same LocalRelation caveat as CDF.

Interception is installed **only on the Sail/Connect path**. On the JVM overlay
Spark runs these natively with full syntax, so intercepting would be a
downgrade.

### Why these and not streaming

These are bounded operations that start and finish against a path, carrying
no Spark session state — so they can be lifted out and run elsewhere.
A streaming query is a long-running computation *inside* the engine; there is
no statement boundary to intercept. That gap needs an upstream Sail fix, which
is why [engine-matrix.md](engine-matrix.md) still lists the streaming sinks as
Sail's real remaining gaps.

The matrix has three columns. Bare Sail stays ❌ for `OPTIMIZE`/`VACUUM` —
Sail genuinely does not implement them. The **middle column** is what the Livy
agent installs, and those two plus MERGE, DESCRIBE, CDF, and JSON `multiLine`
are ✅ there. Java/Scala UDFs and the streaming sinks stay red in the middle.
