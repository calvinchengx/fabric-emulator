# 24 — Parity completion: what "100% real" would take

The [parity map](parity.md) grades every Fabric capability 🟢 Real / 🟡 Emulated
/ 🟠 BYO-engine / 🔴 Not implemented. This doc answers the obvious follow-up —
*what would it take to make everything 🟢?* — and gives the honest answer:

> **100% is not reachable, ~88–90% is, and three rows should deliberately never
> be closed.** Four proprietary engines have no open equivalent; that is a
> documented boundary, not a backlog.

Written before execution so three concurrent sessions plan against one baseline
(the LakeSail track ran this way — [20-lakesail-engine.md](20-lakesail-engine.md)
preceded its code).

## Baseline

Graded from `docs/parity.md` at the time of writing: **60 🟢, 7 🟡, 7 🟠, 14 🔴**
across 8 areas (a row may carry two marks). CI/CD, Data Warehouse, Platform, and
OneLake are effectively complete; Data Engineering holds the largest cluster of
non-green marks, and "item types, engine absent" holds the rest.

## Won't do — and why that's the right call

| Row | Grade | Why it stays |
|---|---|---|
| Long-running operations (202 → poll) | 🟡 | Clock-derived **on purpose**. Real Fabric makes you wait minutes; the virtual clock is the single most valuable testing feature the emulator has. Making it "real" means sleeping — strictly worse. |
| Generic item job status | 🟡 | Same reason. (DataPipeline jobs already run for real.) |
| Web / external-connector activity leaves | 🟡 | Executing arbitrary URLs would destroy the offline + deterministic guarantee. Stubbed success is the correct behavior for a hermetic emulator. |

## Tier 1 — Control plane only (no engine, no research risk)

Pure CRUD + RBAC in patterns the repo has executed a dozen times.

| Gap | Scope | Size |
|---|---|---|
| ~~Governance domains~~ ✅ | `/v1/admin/domains`: domain/subdomain CRUD, workspace assignment, bulk role assignment. Graded 🟢 mgmt with the missing tenant-admin gate called out as 🟡 | S |
| ~~Audit (`activityevents`)~~ ✅ | Real audit trail recorded as operations happen, documented vocabulary, same-day window + continuation paging. 🟢 | S |
| ~~Tenant settings~~ ✅ | Documented TenantSetting object, delegation flags, typed properties. 🟢 read; PATCH is a marked emulator affordance | S |
| Graph, Real-Time Hub, Embed, Workload Dev Kit | Typed items + definition round-trip. **Unblocked**: the authoritative `ItemType` enum is in the REST reference (core/items/list-items), which the fabric-docs clone does not carry | S–M |
| ~~Sensitivity labels~~ ✅ | bulkSetLabels/bulkRemoveLabels + documented label-change audit events. 🟢 (taxonomy is emulator-provided: Purview is not attachable) | M |
| Dataflow Gen2 **management** completeness | Finish the non-engine surface; execution stays 501 | S |

## Tier 2 — Attach a real engine that exists

The repo's established move: terminate the protocol ourselves, let a real engine
compute (SQL Server for TDS, Sail for Spark, MLflow for data science).

| Gap | Engine | Status |
|---|---|---|
| Spark: structured streaming, `OPTIMIZE`/`VACUUM`, Java/Scala UDFs, `sc`/RDD, `spark.jars`, CDF | `apache/spark` JVM as an **opt-in profile** beside Sail | **✅ Done.** The image (`docker/spark-runtime`, Spark 3.5.3 + Delta 3.2 + hadoop-azure) and the CI oracle (`e2e/spark-jvm`) exist, and the statement agent kept its classic-session path — but the root composes expose **only** the `governance` profile, so no user can attach it. `docker-compose.spark-jvm.yml` now rebuilds `spark-agent` on the JVM image with `SPARK_REMOTE: !reset null` (compose merges env maps — a plain override would leave it a Connect client). Verified live: RDD `sc` returns 20 and Delta's JVM classes resolve. Parity rows lifted 🔴 → 🟠. |
| KQL / Eventhouse execution | **`mcr.microsoft.com/azuredataexplorer/kustainer-linux`** — Microsoft's own KQL engine container | **Landed** ([25-rti-kusto.md](25-rti-kusto.md)). The emulator terminates the Kusto REST protocol on the eventhouse's published `queryServiceUri` (Kusto-audience bearer, workspace RBAC, one isolated engine database per Fabric KQL Database) and relays the KQL to the engine, attached by `--profile rti` + `docker-compose.rti.yml`. Witnessed by `e2e/rti` (CI job `rti`) with two client families — raw REST and Microsoft's `azure-kusto-data`. Parity row 🔴 exec → **🟠 exec**. One constraint to know: the engine needs **AVX2**, which Rosetta does not provide, so the default Docker setup on Apple silicon cannot run it — a QEMU x86-64 VM can, and the full suite passes there ([25-rti-kusto.md](25-rti-kusto.md#running-it-on-apple-silicon)). CI remains the witness of record. |
| Eventstream execution | — | **Not reachable via this engine.** The Kusto emulator has no streaming ingestion and no data-management service, so Eventstream stays 🔴 with cause — it needs a streaming pipeline, not a query engine. Split out of the row above rather than left implied. |
| Copy / shortcuts to external stores (S3, standalone ADLS Gen2) | **SeaweedFS** (Apache-2.0, Go) or **RustFS** (Apache-2.0); **Adobe S3Mock** if a pure test double suffices | Partly done (ADLS/S3 read-through exists); a local S3 sidecar makes the write paths real while staying offline. **Do not use MinIO or LocalStack — both repos were archived in 2026** (MinIO 2026-04, AGPL-3.0; LocalStack 2026-03). Verify the replacement's image and S3 surface before committing to it. |

## Tier 3 — Build the protocol ourselves

Precedent: `internal/tds` terminates TDS + Entra FedAuth and byte-splices to a
real SQL Server. These are the same class of work.

| Gap | Notes | Size |
|---|---|---|
| XMLA / ADOMD.NET (SemPy's transport) | Documented protocol (MS-XMLA / MS-SSAS); no harder than TDS was. Gated on real demand — no CI oracle today | L |
| Full DAX | The bounded engine works; this is open-ended surface growth, not a blocker | L, open-ended |
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
2. ~~**kustainer → RTI**~~ ✅ done — `--profile rti` + `docker-compose.rti.yml`, `e2e/rti`, CI job `rti`; Eventhouse/KQL exec 🔴 → 🟠. Eventstream stays 🔴: the engine has no streaming ingestion.
3. **Tier 1 sweep** — steady, no-risk points; good parallel lane.
4. **A live S3 sidecar → external-store Copy** — SeaweedFS/RustFS/S3Mock (not MinIO — archived). Completes the Copy/shortcut story.
5. **Tier 3 only on demand** — XMLA when a real SemPy user appears.

Ceiling after 1–4: **~88–90% real**, with the remainder documented as boundary.

## Coordination

Three sessions share this checkout. Rules that have worked:

- One lane per session; commit only your lane's paths; `pull --rebase` before push.
- Re-read this doc and `parity.md` before starting a tier — both move.
- Grade honestly: if a real engine computes it, 🟢; if it needs an attached
  engine, 🟠; if it 501s, 🔴. Never grade intent.
- Every 🟢 needs a real-client witness in CI, per the ecosystem-conformance
  table in [parity.md](parity.md).
