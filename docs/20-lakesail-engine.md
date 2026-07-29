# 20 — LakeSail: replacing JVM Spark with a Rust engine

**Decision: adopt [LakeSail's Sail](https://github.com/lakehq/sail) as the
Spark engine behind the emulator's compute surfaces, replacing JVM
PySpark/`apache/spark` everywhere it appears.** Sail is a Rust Spark-Connect
server: unmodified `pyspark` clients connect over `sc://…`, and no JVM exists
anywhere in the stack.

Grounded against Sail **v0.6.6** (local checkout audit; citations are that
repo's paths).

## Why

- **The JVM is our heaviest dependency.** The `apache/spark:3.5.3` stack costs
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

**Tradeoff accepted (S3):** deleting the JVM suite removes our only
*Hadoop-ABFS-driver* compatibility witness — a real signal for users pointing
JVM Spark at the emulator. If that matters later, resurrect `e2e/spark` as an
opt-in nightly rather than re-adopting the JVM in the default path.

## Notebook code compatibility (probed, not guessed)

`e2e/sail` probes the surface unmodified Fabric notebook code actually
touches; every claim below is CI-verified against the emulator:

| Notebook pattern | On Sail | Evidence |
|---|---|---|
| `abfss://ws@onelake.dfs.fabric.microsoft.com/…` (production URL form) | ✅ works unmodified — Sail parses the Hadoop form and the endpoint override routes it; **no path shim needed** | e2e probe |
| Delta write/read/append, SQL over temp views | ✅ | e2e |
| Time travel (`option("versionAsOf", n)`) | ✅ (SQL `VERSION AS OF` is a Sail gap) | e2e probe |
| `MERGE INTO` | ✅ **with a registered table target** (`CREATE TABLE … USING delta LOCATION`); a path-based ``delta.`az://…` `` target does not resolve (reads do) | e2e probe |
| `sc` / RDD API / `spark._jvm` | ❌ Spark Connect has no SparkContext. The Livy agent binds `sc` to a guide-rail stub whose every use raises a pointer to this doc — a clear error instead of a bare `NameError`. **Fidelity inversion vs real Fabric**: notebooks using `sc.parallelize` work in production but not here. | agent stub |
| `createDataFrame(local_rows)` | ✅ works — the agents/runners set `spark.conf.set("spark.sql.session.localRelationSizeLimit", <int>)` at session start, overriding the `'3GB'` string pyspark 4.2 chokes on; without that mitigation, use SQL `VALUES` or a 3.5 client | e2e (notebook fixture runs unmodified) |
| DML row-count results (`INSERT`/`MERGE` envelopes) | ⚠️ DataFusion reports counts as `uint64`, which Arrow conversion to Spark clients rejects — the statement HAS executed; the Livy SQL agent absorbs this specific error as an empty result | dbt e2e finding |
| Structured streaming, `OPTIMIZE`/`VACUUM`, CDF, Java/Scala UDFs, `spark.jars` | ❌ absent in Sail v0.6.6 | upstream docs |

## Known gaps to design around (Sail v0.6.6)

- **No transaction conflict detection** in Delta commits — two sessions
  writing one table can both "succeed". Fine for single-driver e2es; document
  for notebook users.
- **No streaming** (`readStream`/`writeStream` absent), `cache()`/`persist()`
  are no-ops, no Java/Scala UDFs (Python/Pandas/Arrow UDFs all work), some
  catalog calls missing (`cacheTable`, `refreshTable`, …).
- **One credential set per server process** — per-workspace identity means one
  Sail container per identity if we ever need isolation; today the daemon
  SP's Storage token covers everything the e2es do.
- Session timeout defaults to 900s — our composes set
  `SAIL_SPARK__SESSION_TIMEOUT_SECS=3600`.

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
