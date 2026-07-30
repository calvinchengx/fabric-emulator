# 22 — Governance: OpenMetadata (optional)

[OpenMetadata](https://open-metadata.org) is the family's optional **upstream
governance layer**: data catalog, schemas, ownership, glossary, lineage — over
the same state your pipelines write into the emulator. Optional by
**compose profile**: users who don't ask for it never pull or start it.

## The commands

```bash
# everything as usual — OpenMetadata NOT running:
docker compose up

# the full suite: family + compute + OpenMetadata
docker compose --profile governance up
# UI: http://localhost:8585  (seeded admin: admin@open-metadata.org / admin)

# catalog the emulator into OpenMetadata (idempotent — rerun to refresh):
docker compose run --rm govern-ingest
```

## What `govern-ingest` catalogs

[`scripts/govern_ingest.py`](../scripts/govern_ingest.py) walks live emulator
state and upserts it as a `fabric-emulator` database service:

| Fabric (emulator) | OpenMetadata |
|---|---|
| Workspace | Database |
| Lakehouse | Database schema |
| Delta table under `Tables/` | Table — **columns read from the real Delta log in OneLake** (delta-rs), not from the control plane |

Types map Delta→OM (`long`→`BIGINT`, `timestamp`→`TIMESTAMP`, …); nullability
carries over; the table description records the Delta version and the
`az://` path. Because the schema source is the actual `_delta_log`, whatever
Sail/dbt/delta-rs wrote is exactly what governance sees — end to end, no
declared-schema drift.

## How it stays optional (and honest)

- The five services (`om-postgresql`, `om-elasticsearch`, `om-migrate`,
  `openmetadata`, `govern-ingest`) are tagged `profiles: [governance]` in
  [docker-compose.yml](../docker-compose.yml) — without the flag they are
  invisible to `docker compose up`, cost nothing, pull nothing.
- Definitions mirror OpenMetadata's own quickstart compose, pinned to
  **1.13.2** (same pin-for-reproducibility rule as everything else here).
- **Weight warning:** this is a real Java server + Elasticsearch (~1 GB heap)
  + Postgres. Expect ~2–3 GB RAM on top of the family, and a couple of
  minutes of first-boot migration.

## Where this goes next

- **SSO against entra-emulator** — OpenMetadata supports OIDC (`OIDC_*` env in
  its config schema); pointing it at the family's own STS would put the
  catalog inside the same trust chain as everything else. Designed, not wired.
- **Lineage** — pipeline runs and notebook jobs are recorded by the emulator;
  emitting OM lineage edges from them is a natural follow-up to the ingest
  script.
- **Real-target symmetry** — under [`FABRIC_TARGET=real`](21-real-fabric-toggle.md)
  the same catalog pattern applies to real Fabric via OpenMetadata's native
  connectors; the emulator path exists so governance can be developed and
  tested offline like everything else.
