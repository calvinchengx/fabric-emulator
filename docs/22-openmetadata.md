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

## Lineage (what the emulator can prove)

`govern-ingest` emits lineage only where the emulator holds an **exact**
fact — it never infers a graph:

| Edge | Source | Emitted? |
|---|---|---|
| target table → shortcut table | a OneLake **shortcut** *is* the data-flow edge; the shortcut is cataloged as a table carrying the target's Delta schema (that is the data it exposes) | ✅ exact |
| Copy source table → sink table | the pipeline executor persists the resolved workspace/item/path pair after successful byte movement | ✅ exact |
| Notebook cell → tables (observed) | the emulator's own data plane serves the I/O, and the runtime identifies the cell making it — via request headers (`notebookutils`) or claims inside the bearer (delta-rs/Sail, whose Rust object_store client cannot set headers). The touch is **witnessed**, not asserted; reads and writes pair within a cell | ✅ exact, observed |
| Notebook cell → tables (reported) | the engine that ran the cell reports the datasets it read and wrote (`notebookRunResult`); recorded verbatim, one edge per read×write pair, named `cell[N]` | ✅ exact, when reported |
| Script/SqlServerStoredProcedure → tables | would require parsing user T-SQL or engine query plans | ❌ not invented |

The CI witness seeds `lake.orders`, shortcuts it as `curated.orders_ref`, then
executes a Copy to `curated.orders_copy`. OpenMetadata must return both
independent edges and remain idempotent on a second ingestion.

## Sensitivity labels → classification tags

Purview's Data Map speaks the **Apache Atlas API**, and OpenMetadata ships an
Atlas connector — so a Purview → OpenMetadata migration carries assets,
classifications, glossary and lineage. The one thing it cannot carry is
**sensitivity labels**: those are Microsoft Purview *Information Protection*
objects, not Atlas entities, so there is nothing on the Atlas surface to read
them from.

The emulator models labels already (see [parity](parity.md), *Identity &
security*), so `govern-ingest` closes that gap offline:

| Fabric (emulator) | OpenMetadata |
|---|---|
| the label taxonomy (`GET /v1/admin/labels`) | Classification **`FabricSensitivity`**, `mutuallyExclusive: true` — an item carries at most one label |
| each label (`Public`, `General`, `Confidential`, `Highly Confidential`) | a Tag under it, description carrying the label's id and sensitivity order |
| an item's `sensitivityLabel.id` | that tag applied to the item's OM entity (`labelType: Automated` — the reference's own word for "a tool determined the label") |

Only **items** carry labels in Fabric, and the only labelled item kind this
ingest catalogs is the Lakehouse — so the tag lands on the Lakehouse's OM
**database schema**. Labels are not propagated down to the tables inside it:
Fabric's downstream-inheritance rules are Purview's, and guessing them here
would be inventing policy.

Two properties the CI witness (`e2e/governance/run.py`) asserts **through
OpenMetadata's API**, never the emulator's:

- **the tag tracks the source of truth.** Two lakehouses get two *different*
  labels, so a constant tag cannot pass; then the label is cleared with
  `bulkRemoveLabels` and a re-ingest must leave the schema with no
  `FabricSensitivity` tag at all.
- **the ingest does not stomp hand curation.** A tag applied by a person in
  the catalog, under a different classification, survives every re-ingest and
  survives a label being cleared: the ingest reads the entity's tags and
  carries through everything that is not its own.

That first property is the reason the tag is reconciled with a **JSON Patch**
rather than in the upsert. OpenMetadata's create-or-update *adds* tags but
never removes one that is absent from the payload — the negative control found
that the first time it ran, with a cleared label still showing `Confidential`
in the catalog. A `PUT` alone would have looked like it worked.

The API shapes (`PUT /v1/classifications`, `PUT /v1/tags`, the `tags[]`
`TagLabel` on a database schema, `?fields=tags` on read-back) are cited to
OpenMetadata's 1.13.x REST reference in comments in
[`scripts/govern_ingest.py`](../scripts/govern_ingest.py) — same rule as the
Fabric side: no field name without a page behind it.

## SSO: the catalog inside the family trust chain (optional)

By default OpenMetadata uses its own basic auth. Layer the SSO overlay and
its authenticator becomes **entra-emulator** — OM validates bearer JWTs
against entra's JWKS with the same issuer fabric-emulator and
azure-keyvault-emulator use, so the catalog stops being the one member with
its own login:

```bash
docker compose -f docker-compose.yml -f e2e/governance/sso-override.yml \
  --profile governance up
```

`e2e/governance/sso.py` witnesses it headlessly: entra mints a **user** token
(client-credentials tokens carry no `email`/`preferred_username`, which is
what OM maps a principal from), OM's API accepts it, and a token with a
broken signature is refused — so the trust edge is real, not "any bearer
accepted". The browser login flow (OIDC confidential client) rides the same
edge but needs a real browser, so it is not asserted.

## How it stays optional (and honest)

- The five services (`om-postgresql`, `om-opensearch`, `om-migrate`,
  `openmetadata`, `govern-ingest`) are tagged `profiles: [governance]` in
  [docker-compose.yml](../docker-compose.yml) — without the flag they are
  invisible to `docker compose up`, cost nothing, pull nothing.
- Definitions mirror OpenMetadata's own quickstart compose, pinned to
  **1.13.2** (same pin-for-reproducibility rule as everything else here) —
  **Postgres-backed** (OM's own Postgres image; the server image's MySQL
  defaults are explicitly overridden).
- **Search backend: OpenSearch, not Elasticsearch** — a deliberate departure
  from OM's `docker-compose-postgres.yml`, which defaults to Elasticsearch.
  OpenMetadata supports both through `SEARCH_TYPE`, but **semantic search works
  only on OpenSearch**: its docs state Elasticsearch "is not supported" for it.
  Defaulting to Elasticsearch would foreclose a feature class at the
  infrastructure layer without anything ever failing. OpenSearch binaries are
  also Apache-2.0, where the `docker.elastic.co` images stay ELv2/SSPL even
  after the source regained an AGPL option in 8.16. The service mirrors OM's
  `docker-compose-opensearch-standalone.yml`, heap tuned down from upstream's
  `-Xms2g -Xmx4g` to 1 GB for a dev-loop stack.
- Two traps if you touch that wiring. There is **no `OPENSEARCH_HOST`** — the
  connection variables stay `ELASTICSEARCH_*` for both backends, so an
  OpenSearch-prefixed one does nothing while the server quietly falls back to
  `localhost:9200`. And switching does **not** enable semantic search: that
  needs `SEMANTIC_SEARCH_ENABLED=true` plus an embedding provider (OpenAI,
  Bedrock, or DJL, which downloads and runs a HuggingFace model in-process).
  This removes the blocker; it does not turn the feature on.
- **CI witness**: `e2e/governance/run.py` (CI job `governance`) boots the
  profile, seeds a real Delta table, runs the ingest, and asserts the
  cataloged columns through OM's API on every push.
- **Weight warning:** this is a real Java server + OpenSearch (~1 GB heap)
  + Postgres. Expect ~2–3 GB RAM on top of the family, and a couple of
  minutes of first-boot migration.

## Where this goes next

- **Labels on more than lakehouses** — every Fabric item can carry a
  sensitivity label, but only Lakehouses are cataloged today, so only their
  labels reach OM. Cataloging notebooks/pipelines as OM entities would extend
  the tag to them.
- **Label-change history** — the emulator writes the documented
  `SensitivityLabelEventData` audit events on every apply/change/remove; the
  ingest reads only the current label, not that history.
- **Domains and ownership** — governance domains and workspace roles are
  modelled by the emulator and have OM counterparts (domains, owners); neither
  is exported yet.
- **Real-target symmetry** — under [`FABRIC_TARGET=real`](21-real-fabric-toggle.md)
  the same catalog pattern applies to real Fabric via OpenMetadata's native
  connectors; the emulator path exists so governance can be developed and
  tested offline like everything else.
