# 24 — Parity completion: what "100% real" would take

The [parity map](parity.md) grades every Fabric capability 🟢 Real / 🟡 Emulated
/ 🟠 non-default engine / 🔴 Not implemented. This doc answers the obvious follow-up —
*what would it take to make everything 🟢?* — and gives the honest answer:

> **100% is not reachable, ~88–90% is, and three rows should deliberately never
> be closed.** Four proprietary engines have no open equivalent; that is a
> documented boundary, not a backlog.

Written before execution so three concurrent sessions plan against one baseline
(the LakeSail track ran this way — [20-lakesail-engine.md](20-lakesail-engine.md)
preceded its code).

## Baseline

Graded from `docs/parity.md` at the time of writing: **60 🟢, 7 🟡, 7 🟠, 14 🔴**
across 8 areas (a row may carry two marks). **Those counts are historical.**
`check_witnesses.py --strict` now reports **113** supported (🟢) claims.
CI/CD, Data Warehouse, Platform, and OneLake are effectively complete; Data
Engineering holds the largest cluster of non-green marks, and "item types,
engine absent" holds the rest.

## Won't do — and why that's the right call

| Row | Grade | Why it stays |
|---|---|---|
| Long-running operations (202 → poll) | 🟡 | Clock-derived **on purpose**. Real Fabric makes you wait minutes; the virtual clock is the single most valuable testing feature the emulator has. Making it "real" means sleeping — strictly worse. |
| Generic item job status | 🟡 | Same reason. (DataPipeline jobs already run for real.) |
| ~~Web / external-connector activity leaves~~ | — | **Reversed, and the reasoning was wrong.** This row argued that executing arbitrary URLs would destroy the offline guarantee. It protected the wrong thing: the offline promise is that the **emulator** makes no calls of its own, never that a user's pipeline cannot reach the service it names. A pipeline branching on `@activity('Ping').output.status` got a fabricated success from a URL nothing contacted — green locally, different in Fabric, which is the exact failure class this emulator exists to prevent. The Web activity now makes the real call; `RestSource`/`RestSink` read, page and write; Salesforce runs both Bulk API 2.0 lifecycles. Hermetic runs stay available and **explicit** (`FABRIC_WEB_ACTIVITY=stub`), labelled in the output so such a run cannot be mistaken for one that called. See [40-rest-connector-plan.md](40-rest-connector-plan.md) and [41-salesforce-connector-plan.md](41-salesforce-connector-plan.md) |

## Tier 1 — Control plane only (no engine, no research risk)

Pure CRUD + RBAC in patterns the repo has executed a dozen times.

| Gap | Scope | Size |
|---|---|---|
| ~~Governance domains~~ ✅ | `/v1/admin/domains`: domain/subdomain CRUD, workspace assignment, bulk role assignment. Graded 🟢 mgmt with the missing tenant-admin gate called out as 🟡 | S |
| ~~Audit (`activityevents`)~~ ✅ | Real audit trail recorded as operations happen, documented vocabulary, same-day window + continuation paging. 🟢 | S |
| ~~Tenant settings~~ ✅ | Documented `TenantSetting` object, delegation flags, typed properties, and the **real** `POST /v1/admin/tenantsettings/{name}/update`. An earlier note here claimed Fabric had no public write API and that the emulator's was an affordance — both wrong; the API is documented | S |
| ~~Item-type fidelity~~ ✅ | The documented `ItemType` enum is enforced (`InvalidItemType`, case-insensitive canonicalisation); every enum type is creatable through the generic surface, and `GraphQLApis`/`variableLibraries` gained typed collections. Remaining typed collections are cheap but each needs its own reference page — the segments are not derivable | S–M |
| ~~Sensitivity labels~~ ✅ | bulkSetLabels/bulkRemoveLabels + documented label-change audit events. 🟢 (taxonomy is emulator-provided: Purview is not attachable) | M |
| Dataflow Gen2 **management** completeness | Finish the non-engine surface; execution stays 501 | S |
| **Runtime divergences** (Environment items, Files mount, `input_file_name()` in SQL, inline pipeline jobs) | Four analogs the code states rather than hides. Environments and the Files mount (write-back + per-statement refresh + refuse a second lakehouse) are done; `input_file_name()` in SQL remains. Pipelines are the last job type that does not use the repo's own async pattern. Scoped in [37-runtime-fidelity-gaps.md](37-runtime-fidelity-gaps.md) | XS–M remaining |
| **Framework conformance kit** | The runtime contracts a Fabric framework depends on and the REST reference does not describe: the context fallback chain, signature introspection, the runtime version floor, write-landing verification, concurrent session isolation, rewrite fall-through, credential lifetime. Every defect in this class was found by driving a real product while the parity map, the witness system and four medallion examples stayed green. Notebooks, assertions and one CI job — no engine work. Scoped in [38-framework-conformance.md](38-framework-conformance.md) | M |
| ~~Capacity **job queueing** and throttling~~ ✅ | Per-capacity concurrent-job ceiling (default 999). Manual submits against a full capacity are `430 CapacityNotAvailable` with `Retry-After`; scheduled and event-triggered jobs enter `Queued` and are admitted FIFO on the same clock/list levers that fire schedules. Same-item jobs are not serialised. [36-capacity-job-queueing.md](36-capacity-job-queueing.md) | M |
| **`runMultiple` full parity** (exit values, per-cell timeout, lakehouse inheritance, concurrency, retry) | The DAG semantics are real; what's missing includes two wrong answers shipping today — `exitVal` hardcoded `""`, and `timeoutPerCellInSeconds` applied as a whole-notebook deadline. Also a latent agent bug: catalog state is process-wide across concurrent Livy sessions. Scoped in [39-run-multiple-parity-plan.md](39-run-multiple-parity-plan.md) | S–M each |

## Tier 2 — Attach a real engine that exists

The repo's established move: terminate the protocol ourselves, let a real engine
compute (SQL Server for TDS, Sail for Spark, MLflow for data science).

| Gap | Engine | Status |
|---|---|---|
| Spark: structured streaming, `OPTIMIZE`/`VACUUM`, Java/Scala UDFs, `sc`/RDD, `spark.jars`, CDF | `apache/spark` JVM as an **opt-in profile** beside Sail | **✅ Done.** The image (`docker/spark-runtime`, Spark 3.5.5 + Delta 3.2 + Java 11) and the CI oracle (`e2e/spark-jvm`) exist, and the statement agent kept its classic-session path — but `make up-jvm` attaches it (`docker-compose.yml` + `override` + `spark-jvm.yml`) — an earlier revision of this row said no user could, which was true when written and stopped being true when that target landed. `docker-compose.spark-jvm.yml` now rebuilds `spark-agent` on the JVM image with `SPARK_REMOTE: !reset null` (compose merges env maps — a plain override would leave it a Connect client). Verified live: RDD `sc` returns 20 and Delta's JVM classes resolve. Parity rows lifted 🔴 → 🟠. |
| KQL / Eventhouse execution | **`mcr.microsoft.com/azuredataexplorer/kustainer-linux`** — Microsoft's own KQL engine container | **Landed** ([25-rti-kusto.md](25-rti-kusto.md)). The emulator terminates the Kusto REST protocol on the eventhouse's published `queryServiceUri` (Kusto-audience bearer, workspace RBAC, one isolated engine database per Fabric KQL Database) and relays the KQL to the engine, attached by `--profile rti` + `docker-compose.rti.yml`. Witnessed by `e2e/rti` (CI job `rti`) with two client families — raw REST and Microsoft's `azure-kusto-data`. Parity row 🔴 exec → **🟠 exec**, then regraded **🟢 Real (sidecar)** — the same opt-in-sidecar shape as Airflow; the profile is still opt-in. One constraint to know: the engine needs **AVX2**, which Rosetta does not provide, so the default Docker setup on Apple silicon cannot run it — a QEMU x86-64 VM can, and the full suite passes there ([25-rti-kusto.md](25-rti-kusto.md#running-it-on-apple-silicon)). CI remains the witness of record. |
| Shortcut reads from a **standalone ADLS Gen2 / Blob** endpoint | **Azurite** (Microsoft's own storage emulator, MIT) — already used elsewhere in this repo | **✅ Done** — `e2e/azurite-shortcut` (CI job `azurite-shortcut`) replaces the hand-written stub with Microsoft's own emulator: `azure-storage-blob` writes the blob, a real `generate_blob_sas` token drives the shortcut, an unauthenticated GET must 403 and a tampered SAS must be refused. **Scope is the read path only** — Azurite does not implement ADLS Gen2 (Microsoft documents this), so the endpoint is Blob; that is identical to DFS for an authenticated GET, but DFS-specific behaviour remains unwitnessed and no open emulator exists for it. |
| Eventstream execution | **`apache/kafka` KRaft** (ASF, Apache-2.0 — not Redpanda/BSL) + Fabric notebook API on **Sail and JVM** | **Landed** ([51-eventstream-kafka.md](51-eventstream-kafka.md)). Item create mints a DefaultStream `datasourceId` and a Kafka topic; `format("kafka")` + `eventstream.itemid` / `eventstream.datasourceid` — **never** `rate`. Sail consumes through the emulator (LocalRelation + local `foreachBatch`); JVM uses OSS `spark-sql-kafka`. Native OSS kafka on Sail (subscribe / pattern / assign, SASL PLAIN, kafka sink) is the same wrap — one micro-batch, not checkpointed. Witnessed by `e2e/eventstream`. Parity row 🔴 exec → **🟠 exec** (opt-in `--profile eventstream`), then regraded **🟢 Real (sidecar)** — the profile is still opt-in. Lakehouse, Reflex, and Eventhouse destinations are separate green rows (Custom HTTP → Delta append / item job / Kusto direct ingest). Operators (Filter, GroupBy, tumbling Window on the produce batch) are a separate green row. Fabric streaming ingest and queued `Kusto.Ingest` stay 🔴: kustainer has no streaming ingestion. |
| Shortcut reads from **Amazon S3 / S3-compatible** | **SeaweedFS** (Apache-2.0, Go) — health re-verified before adoption: not archived, pushed the same day | **✅ Done — and read-only is the finished state.** The gap was never the sidecar: the read-through sent header credentials, and a real S3 endpoint requires **SigV4**. `internal/awssig` implements it (validated against AWS's published example signature), and a Connection carrying the documented Access Key ID / Secret Access Key pair triggers signing. `e2e/s3` (CI job `external-s3`) witnesses it against SeaweedFS with anonymous access **denied**: boto3 writes the object, an unsigned GET must 403, the OneLake read-through must return the exact bytes, and a wrong secret must be refused. MinIO and LocalStack remain excluded — both archived in 2026. **Scope of this row is S3 only** — SeaweedFS is an S3 server and cannot witness a standalone ADLS Gen2 endpoint (different verbs, Entra/SAS auth); that target is a separate row below. **Read-only is correct here, not a gap:** Fabric documents that "S3 shortcuts are read-only. They don't support write operations regardless of the user's permissions" (onelake/create-s3-shortcut.md). Signing only GET/HEAD therefore matches the product; adding PUT would make the emulator *less* faithful. An earlier revision of this row listed S3 writes as unfinished work — that was wrong. The credential travels as `Basic` (username = Access Key Id, password = Secret Access Key): Fabric's S3 connector uses authentication kind "Access Key", and `Basic` is the only documented `CredentialType` carrying two secrets. An earlier implementation invented `accessKeyId`/`secretAccessKey` fields that exist in no Fabric credential object. |

### Which Sail gaps are real — measured, not asserted

[`engine-matrix.md`](engine-matrix.md) runs one probe per capability against
**both** Sail and the JVM and is regenerated in CI, so this list cannot go
stale the way the prose did. It already disproved one entry: SQL
`VERSION AS OF` was graded a Sail gap and works.

Candidates it leaves standing, with the engine's own error:

| Gap | Sail says |
|---|---|
| Streaming sinks — delta / parquet / memory | `unsupported extension node for streaming: DeltaWriteNode`; `cannot write streaming data to listing table`; `No table format found for: memory` |
| `OPTIMIZE` / `VACUUM` | `found OPTIMIZE at 0:8 expected something else` — absent from the SQL parser |
| Change Data Feed | `Table features must be specified: ChangeDataFeed` |

Not candidates: `sc`/`_jvm` fail with `JVM_ATTRIBUTE_NOT_SUPPORTED`, a Spark
**Connect protocol** limit rather than a Sail choice — no upstream fix exists.

## Tier 3 — Build the protocol ourselves

Precedent: `internal/tds` terminates TDS + Entra FedAuth and byte-splices to a
real SQL Server. These are the same class of work.

| Gap | Notes | Size |
|---|---|---|
| XMLA / ADOMD.NET (SemPy's transport) | **DELIVERED for the read path** (2026-08-10), and this estimate is re-priced against what it actually cost rather than retired quietly. The `L` was dominated by two things the note said no client had yet been asked to exercise: rowset serialisation and the `Discover` metadata subset. Microsoft's own SemPy has now exercised both, in CI (`ci:sempy`), driving `evaluate_dax` plus the whole `list_*` metadata family against the emulator. The `L` was **correct about the shape and wrong about the multiplier**: the work was indeed dominated by the rowset writer, but each gate cost one measured thing rather than a design, namely the `Version` column typed `xsd:long`, the `<root name>` TOM looks tables up by, the trailing payload status byte, and TOM's ~35-element `<Discover>` batch. Reading the client's own contract (capture, then reflection) is what collapsed it; guessing would have spent the `L`. `semantic-link-labs` 0.17.0 now reads the same model through TOM in the same job, and what it needed was not XMLA at all: it failed on the `notebookutils` shim, before reaching the transport. The WRITE path then landed too, and it was not TMSL JSON at all but row deltas in the 2014/engine namespace, so the estimate was for the wrong protocol as well as the wrong size. **Remaining: `S`** for structural writes (new tables, columns, relationships), `<Refresh>`, MDX, and the LRO continuation byte | ~~L~~ → **S** (read path done) |
| Full DAX | The bounded engine works; this is open-ended surface growth, not a blocker. The oracle is Power BI Desktop's `msmdsrv`, already reached in CI on **`windows-latest`** ([33-pbix-tooling.md](33-pbix-tooling.md) Phase 0b, bit-identical). A developer machine can attach the same process (UTM on a Mac you own, `dockur/windows` on Linux+KVM metal, Desktop on Windows) — [52-msmdsrv-hosts.md](52-msmdsrv-hosts.md). That host map is **not** `runs-on: macos-latest` / `ubuntu-latest` (nested Windows is not a PR job) and not a compose default. Every-push ubuntu/mac CI tests the Go subset against Desktop goldens. | L, open-ended |
| On-prem gateway **contract** | Emulate the protocol, not the proprietary binary | M |

## Tier 4 — Not reachable without Microsoft's source

| Gap | Blocker |
|---|---|
| **Power Query M engine** (Dataflow Gen2 execution) | No open implementation exists anywhere. The hardest gap on the board. |
| Power BI report rendering | Proprietary renderer |
| Dataverse shortcuts | Proprietary backend, no emulator |
| Copilot / IQ fidelity | An LLM can be attached; never *the* model |

These stay 🔴 with honest 501s. A clean-room emulator that is ~90% real with
loud failures at the edges is a better artifact than one that fakes the last
10% — the same stance [parity.md](parity.md) already takes.

## Sequence

1. ~~**Expose the JVM profile**~~ ✅ done — `docker-compose.spark-jvm.yml`, verified live, parity rows lifted.
2. ~~**kustainer → RTI**~~ ✅ done — `--profile rti` + `docker-compose.rti.yml`, `e2e/rti`, CI job `rti`; Eventhouse/KQL exec 🔴 → 🟠, then regraded 🟢 sidecar (profile still opt-in). Eventstream Kafka exec is a separate slice ([51](51-eventstream-kafka.md)), also regraded 🟢 sidecar (profile still opt-in). Lakehouse, Reflex, and Eventhouse destinations (direct ingest) and produce-batch operators are separate green rows; Fabric streaming ingest stays 🔴.
3. **Tier 1 sweep** — steady, no-risk points; good parallel lane.
4. ~~**External-store shortcut reads**~~ ✅ done — S3 (SeaweedFS + SigV4) and ADLS Gen2/Blob (Azurite + SAS). **S3 writes are deliberately absent**: Fabric documents S3 shortcuts as read-only. **ADLS Gen2 write-through is now done** — flush PUTs to the storage account, delete removes at the target, both witnessed by SDK reads against Azurite. Writes previously landed in the local store and silently never reached the target.
5. **Tier 3 only on demand** — XMLA when a real SemPy user appears.

Ceiling after 1–4: **~88–90% real**, with the remainder documented as boundary.

## Coordination

Three sessions share this checkout. Rules that have worked:

- One lane per session; commit only your lane's paths; `pull --rebase` before push.
- Re-read this doc and `parity.md` before starting a tier — both move.
- Grade honestly: if a real engine computes it, 🟢; if it needs an attached
  engine, 🟠; if it 501s, 🔴. Never grade intent.
- Every 🟢 needs a real-client witness in CI, per the ecosystem-conformance
  table in [parity.md](parity.md). **This is now enforced**:
  `docs/witnesses.json` maps every green claim to the CI job and/or Go test
  that witnesses it, and `scripts/check_witnesses.py --strict` (CI job
  `witnesses`) fails the build on a claim with no witness, or a witness that
  no longer exists. It also reports witnesses carrying many claims — that
  report already caught `fabric-cicd` being credited for warehouse item
  management, a suite that publishes Notebooks only.
