# Example: end-to-end medallion

A complete analytics loop against the emulator family — Entra tokens, a Key
Vault secret referenced by a `Key` credential (`keyReference`), extraction into
**landing**, **bronze → silver** in OneLake Delta, **silver → gold with dbt**
in the Warehouse, and a **semantic model** queried over the Power BI
`executeQueries` wire.

This directory is the code the tutorial
[docs/28-tutorial-end-to-end.md](../../docs/28-tutorial-end-to-end.md) walks
through, and it is what [`e2e/medallion`](../../e2e/medallion/) runs in CI on
every push. Read the tutorial for the narrative; run the scripts here to watch
it work.

## Run it

Start the emulator family, with real compute attached:

```sh
docker compose up -d          # from the repo root
```

> **Running from a git checkout rather than a release?** That command starts the
> **published** `fabric-emulator:latest` image, which can lag `main` by hours.
> These steps assert against the emulator's current behaviour, so a stale image
> fails in ways that look like example bugs — a missing `rowsCopied` on the Copy
> activity, a 404 on a route that exists in your tree. Build the emulator from
> your checkout and point compose at it:
>
> ```sh
> docker build -t ghcr.io/calvinchengx/fabric-emulator:dev .
> FABRIC_EMULATOR_VERSION=dev docker compose up -d
> ```

Then, in this directory:

```sh
uv init --bare                # only if you copied this directory elsewhere
uv add requests pandas pyarrow deltalake pyodbc dbt-fabric
uv run python pipeline.py
```

Or run the steps one at a time, which is what the tutorial does:

```sh
uv run python provision.py
uv run python secret.py
…
```

You also need the **Microsoft ODBC Driver 18** for the dbt and TDS steps
(macOS: `brew tap microsoft/mssql-release && brew install msodbcsql18`), and a
Spark engine for `engine.py` — `docker compose up` starts Sail, and the step
reads `SPARK_REMOTE` (default `sc://localhost:50051`).

## How big is it

Both source systems are **seeded generators**, so the data is production-shaped
but byte-identical on every run — which is what lets each step assert exact
counts against it.

| | rows | columns |
|---|---:|---:|
| `bronze_customers` | 102,000 | 101 |
| `bronze_orders` | 255,000 | 17 |
| `silver_customers` | 100,000 | 101 |
| `silver_orders` | 247,500 | 17 |
| `bronze_web_order_lines` | ~226,000 | 8 |
| `bronze_erp_changes` | ~93,600 | 12 |
| `dim_customer_scd2` | ~92,400 | 13 |
| `bronze_fx_rates` | 132 | 3 |
| `bronze_product_hierarchy` | 8 | 6 |

A full `pipeline.py` takes **under two minutes** on a laptop; `pipeline.py`
runs all 17 steps in about the same. The landed files are ~75 MB of CSV, ~95 MB
of JSON Lines, ~30 MB of nested JSON and ~1.9 MB of Parquet.

Change the scale with `N_CUSTOMERS` / `N_ORDERS` in
[`source_system.py`](source_system.py) and `N_WEB_CUSTOMERS` / `N_WEB_ORDERS` in
[`web_store.py`](web_store.py). Every `EXPECTED_*` value is computed from those
constants and the defect ratios, so nothing needs updating alongside them.

Two things to know before raising them. The emulator keeps OneLake in an
**in-memory** store by default (`docker-compose.yml` sets `FABRIC_DATA_DIR`
empty), so every bronze and silver table is resident at once — the run above
peaks around 2 GB. And the lakehouse SQL endpoint re-reflects **every**
`Tables/` Delta into the SQL sidecar on each connect, which steps 07, 08 and 09
each pay; that load uses the TDS bulk-copy protocol, so the ~29M cells above
reflect in about 15s rather than the ~31 minutes the old literal-`INSERT` path
would have taken.

## The steps

| Script | What it does |
|---|---|
| `provision.py` | workspace + lakehouse + warehouse + workspace identity |
| `secret.py` | secret into Key Vault; a vault connection + a Key credential referencing it (asserts the secret never reads back) |
| `extract_load.py` | pull from the vendor API, land it verbatim under `Files/landing/` |
| `bronze.py` | a real **DataPipeline**: a Copy activity the emulator executes, plus a Notebook activity |
| `engine.py` | **Spark (Sail)** executes the parsed cells and reports the run + its read/write set |
| `silver.py` | dedupe, conform countries, quarantine the malformed row |
| `wrangle.py` | **interactive**: profile bronze vs silver in VS Code Data Wrangler |
| `reflect.py` | connect to the lakehouse database — reflection makes silver queryable T-SQL |
| `gold.py` | `dbt build`: the star as tables in the Warehouse, plus DQ tests |
| `dq_gate.py` | poison silver → dbt **fails** → restore → green again |
| `semantic_model.py` | publish TMSL + rows, query with DAX, assert a wrong audience is refused |
| `lineage.py` | assert the graph: a `Copy` edge and a `Notebook` edge, neither guessed from code |

`pipeline.py` runs all of them except `wrangle.py`, which is meant for the
VS Code Interactive Window.

## The advanced track (steps 20+)

Steps 00–11 are the tutorial and do not change. The **advanced track** picks up
from there with three more feeds. Each exists for one reason the others cannot
cover:

| Feed | Why it exists |
|---|---|
| **Contoso POS** (base) | flat batch export, `customer_id` on every row, dirty everything |
| **Contoso Web** | nested JSON, and **email is the only key** — one source can only ever be deduplicated against itself; two that share no key must be *resolved* |
| **Contoso ERP** | **change over time.** An append-only CDC log is the only shape from which "what was true on this date" can be reconstructed, which is what makes SCD2 and incremental loading real rather than aspirational |
| **Reference data** | **comparability.** FX rates and a product hierarchy, published by finance and merchandising — what makes numbers from the other three add up to something |

No key spans all three systems. POS↔Web join on email, POS↔ERP on phone, and
ERP↔Web share nothing at all — so identity is a graph, and an ERP account
reaches a Web account only by travelling through POS.

| Script | What it does |
|---|---|
| `web_extract.py` | second source: Contoso Web gets its own Key Vault secret, referenced through the same vault connection; nested JSON lands verbatim |
| `web_bronze.py` | flatten orders → line rows; pin the **designed overlap** with the POS customer set |
| `erp_extract.py` | third source + the reference publisher: two more secrets, two more AKV connections; **Parquet** lands verbatim |
| `erp_bronze.py` | three Copy activities reading a **columnar** source — the emulator's `ParquetSource` path, where 03 exercised its delimited-text one |
| `24_erp_scd2.py` | the change log becomes `dim_customer_scd2`: one row per version, `valid_from`/`valid_to`/`is_current`, plus a point-in-time lookup |
| `resolve.py` | resolve three customer sets **transitively**, and name the three cohorts that cannot be |

```sh
uv run python pipeline.py     # the basic pipeline, then 20+
```

`pipeline.py` reuses `pipeline.py`'s step list rather than restating it, so
the basic track cannot drift out from under the advanced one.
[`e2e/medallion-advanced`](../../e2e/medallion-advanced/) runs it in CI.

## Where your items live: `definitions/`

The notebook and the pipelines are **not** built as strings in Python. They are
committed files, in the layout Microsoft Fabric itself uses:

```
definitions/
  bronze-orders.Notebook/
    .platform                 ← item metadata: type, display name, logicalId
    notebook-content.py       ← the notebook, as Fabric stores it
  bronze-ingest.DataPipeline/
    .platform
    pipeline-content.json     ← the activities
```

**This is the same layout Fabric's Git integration writes.** Connect a real
workspace to Azure DevOps or GitHub and Fabric produces exactly this: one
directory per item named `{display name}.{public facing type}`, holding the
item's definition files plus a `.platform`. So the directory above is what your
repository looks like in production — not an emulator convention you would have
to unlearn.

Three things follow from that, and they are the point of the example:

**The filenames are Fabric's.** `notebook-content.py` for a Notebook,
`pipeline-content.json` for a DataPipeline. These become the `path` values in
the definition parts that `updateDefinition` accepts. Invent a filename and the
emulator will store it happily — it keeps parts verbatim by design — while real
Fabric refuses. Copy these names.

**`.platform` carries the identity.** Its `logicalId` is the cross-workspace
identifier that ties this item to its source-control representation and survives
renames and moves. It is how one branch can deploy to dev, test and prod and have
Fabric recognise the same item in each. Do not edit it.

**`{{TOKENS}}` are how ids travel.** A pipeline names the workspace and lakehouse
it reads by GUID, and those GUIDs differ in every workspace:

```json
"typeProperties": {"workspaceId": "{{WORKSPACE_ID}}", "artifactId": "{{LAKEHOUSE_ID}}"}
```

So the committed file is a template and the deploy substitutes. That is not a
local shortcut — Microsoft's own [`fabric-cicd`](https://learn.microsoft.com/en-us/fabric/cicd/tutorial-fabric-cicd-azure-devops)
ships a `find_replace` parameter file for the same reason. `bronze.py` deploys
with one call:

```python
nb = create_item_from_definition(
    "bronze-orders.Notebook",
    WORKSPACE_ID=ws, LAKEHOUSE_ID=lake,
    LANDING_DIR=landing_dir, LANDING_DATE=st["landing_date"])
```

The item **type comes from the folder name**, as it does in Git, and any
placeholder left unsubstituted is an error at deploy rather than a Spark failure
ten minutes later about a workspace literally named `{{WORKSPACE_ID}}`.

What is *not* in `definitions/` is your data. Definitions are versioned; the
Delta tables they write live in OneLake and are never in Git, and no deployment
overwrites them. [docs/46](../../docs/46-artifact-persistence.md) is the full
contract, with links to Microsoft's documentation for each claim.

## Files

- `common.py` — endpoints, tokens for all five audiences, the TDS connector, state
- `source_system.py` — the fictitious "Contoso POS" vendor API and the expected
  row counts each stage must produce (the oracle every step asserts against)
- `web_store.py` — the second source, "Contoso Web": nested JSON, no customer
  id, a product catalog POS does not have, and a **designed** overlap with the
  POS customer set (advanced track)
- `erp_system.py` — the third source, "Contoso ERP": an append-only **CDC change
  log** in Parquet, keyed by phone. The only feed that carries change over time,
  which is what makes SCD2 possible (advanced track)
- `reference_data.py` — the fourth feed, and the only one that is not a system:
  published **FX rates and a product hierarchy**, in Parquet (advanced track)
- `contracts/` — ODCS v3.1.0 data contracts over the layers; see
  [docs/30](../../docs/30-odcs-data-contracts.md)
- `gold/` — the dbt project (models, sources, schema tests, singular tests)
- `definitions/` — your items in Fabric's own source format, one directory
  per item; see the section above
- `state.json` — written by `provision.py`, read by everything after it

## Configuration

Endpoints default to the local developer stack on `localhost` with self-signed
TLS. Every one is overridable, which is how the CI harness points the same code
at compose service names over plain HTTP:

| Variable | Default |
|---|---|
| `ENTRA_URL` | `https://localhost:8443` |
| `KV_URL` | `https://localhost:8444` |
| `FABRIC_REST_URL` | `https://localhost:9443` |
| `TDS_SERVER` | *unset* — the SQL address is **discovered** from the item (`properties.connectionString`, or `sqlEndpointProperties.connectionString` for the lakehouse endpoint), which is the only form that also works on real Fabric. Set it only for a stack whose SQL port is remapped, since the emulator advertises the port it listens on |
| `KV_INTERNAL_URL` | `https://keyvault-emulator:8444` — the vault URI **Fabric** resolves, so it must be reachable from the emulator, not from you. That is why it is a service name and not `localhost` even when you run these steps on your machine |
| `SPARK_REMOTE` | `sc://localhost:50051` — the Spark engine `engine.py` drives the notebook run onto (Sail, as the root compose publishes it) |
| `PIPELINE_STATE` | `./state.json` |
| `GOLD_PROJECT` | `./gold` |

## Re-running

`provision.py` fails if `contoso-analytics` already exists — display names
are unique. Delete the workspace, or reset the stack with
`docker compose down -v && docker compose up -d`.

Bronze appends by design, so a second full run doubles bronze rows; silver's
dedupe absorbs it and the assertions still hold.
