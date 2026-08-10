# Example: the advanced medallion

Three source systems, resolved into one customer identity, joined into a star in
the Warehouse, and served to Power BI — **23 steps in about five
minutes, on a laptop, with no Azure subscription.**

`bronze → silver` is PySpark on a real engine; `silver → gold` is dbt-fabric
over real TDS, because gold is a Warehouse. The whole run asserts its own
results, so it is a test rather than a demo script.

```sh
make up                       # from the repo root (starts OpenMetadata too)
uv sync --frozen && uv run python pipeline.py
```

Open <https://localhost:9443/#flow> while it runs, and
<http://localhost:8585> for the catalog it publishes at the end.

## What this demo shows

### If you own the data product

You are watching a **complete data product get built and served**, end to end,
before anyone provisions a tenant or spends on capacity.

Three source systems — a POS batch export, a web store's nested JSON, and an
ERP change log — land, get conformed, get resolved into one customer across all
three, and end up as a star that a Power BI client queries over the real
`executeQueries` API. The Data flow view draws that path as it happens.

Two moments are worth waiting for:

- **The gates fail on purpose.** At step 21 the contracts reject an unconformed
  country and a duplicate `customer_id` — the run proves the guarantee is
  enforceable, not merely documented. `contract gates are enforced, and
  demonstrably able to fail`.
- **Provenance reaches the number.** The graph traces `Revenue` in the published
  model back through `fct_daily_revenue`, `fct_orders`, `silver_orders`,
  `bronze_orders`, to the vendor file that landed that morning.

That makes three product questions answerable that usually are not:

| Question | Where the answer is |
|---|---|
| Where did this number come from? | the lineage chain, source file → model table |
| What breaks if I change this table? | the edges downstream of it, before you ship |
| Is the guarantee real? | the DQ gate and the ODCS contracts, executed, failing when they should |

And the trust is **graded**, which matters more than having lineage at all: every
edge records *how* it is known. `Warehouse` means the emulator watched the
engine accept the statement. `Reported` means a step claimed it. You never have
to guess whether a hop is evidence or assertion.

The honest limit: an **import** semantic model gets no automatic edge, because
its rows arrive in the definition already detached from wherever they were
selected. It reports its own sources, or it has none.

### If you build the pipelines

This is the code you would write for real Fabric, running unmodified against a
local emulator. Nothing is stubbed where it counts:

- **PySpark** on a real engine (Sail — Rust Spark Connect, no JVM), writing
  Delta to OneLake
- **dbt-fabric** building a Warehouse over actual TDS, against a real SQL Server
  — including Fabric's `CREATE TABLE … AS SELECT`, which the emulator rewrites
  on the wire (see [docs/29](../../docs/29-tsql-parity.md))
- **Key Vault** secrets fetched through an `AzureKeyVaultReference` connection,
  resolved by the workspace identity
- **DataPipelines** whose Copy and Notebook activities the emulator really
  executes
- **Entra** tokens for all five audiences, validated as production would

The right-hand pane is the payoff: a live view of which step wrote which table,
with row counts and Delta versions, attributed to the activity or notebook cell
that caused it. You debug by watching rather than by reading container logs
afterwards — and a failing activity reaches the stream in red, with its error,
the moment it fails.

The same run is CI-able ([`e2e/medallion-advanced`](../../e2e/medallion-advanced/)),
so pipeline logic gets tested on every push without a tenant.

One cost worth knowing: the warehouse observer records only statements it fully
understands, whose response it can read as successful. It errs toward recording
nothing over recording something wrong — so an edge is trustworthy, but the
*absence* of an edge is not proof that nothing moved.

## How big is it

Every source is a **seeded generator**, so the data is production-shaped and
byte-identical on every run — which is what lets each step assert exact counts.
The figures below are measured from a real run, not estimated.

**Four feeds, three of them systems.** Reference data is the fourth: published
by finance and merchandising, and the only feed that is not a system of record.

| Feed | What it lands | Key it carries | Landed |
|---|---|---|---:|
| **Contoso POS** | customers as CSV, orders as JSON Lines | `customer_id`, email, phone | 169.8 MB |
| **Contoso Web** | customers, products and orders as nested JSON | email only — no customer id | 36.2 MB |
| **Contoso ERP** | an append-only CDC change log, Parquet | phone | 1.9 MB |
| Reference data | FX rates + product hierarchy, Parquet | — | &lt;0.01 MB |
| | | **total** | **207.8 MB** in 8 files |

**Tables and columns**, end to end:

| Layer | Tables | Columns | On disk |
|---|---:|---:|---:|
| Lakehouse (bronze + silver, Delta) | 15 | 329 | 174.7 MB / 129 files |
| Warehouse (gold star, T-SQL) | 8 | 53 | — |
| Semantic model | 2 | 8 | — |
| **Total** | **25** | **390** | **~383 MB** incl. landing |

Gold is deliberately narrow: 329 columns of raw and conformed detail resolve
down to **53** a consumer is entitled to depend on.

**Row counts:**

| | |
|---|---:|
| POS customers landed | 102,000 |
| silver customers (deduped, conformed) | 100,000 |
| silver orders / quarantined | 247,500 / 2,500 |
| ERP change-log rows → SCD2 versions | 93,571 → 92,371 |
| resolved identities (from 168,800 source records) | **129,526** |
| ├ known to more than one system | 35,376 |
| ├ web-only | 18,000 |
| └ ERP-only (current) | 11,526 |
| web fact grain, all resolved to a customer key | 213,562 |
| lineage edges recorded | 33 across 4 producers |

The widest table is `bronze_customers` at **101 columns** — deliberately, because
a 4-column fixture makes conformance look easy and hides the cost of carrying
detail through to silver.

Scale it with `N_CUSTOMERS` / `N_ORDERS` in
[`../contoso-fixtures/source_system.py`](../contoso-fixtures/source_system.py)
and the equivalents in
[`../contoso-fixtures-advanced/`](../contoso-fixtures-advanced/). Every
`EXPECTED_*` value is computed from those constants and the defect ratios, so
nothing needs updating alongside them. Note that OneLake is **in-memory** by
default (`FABRIC_DATA_DIR` is empty in `docker-compose.yml`), so every table is
resident at once — this run peaks around 2 GB.

No key spans all three systems: POS↔Web join on email, POS↔ERP on phone, and
ERP↔Web share nothing at all. Identity is therefore a **graph** — an ERP account
reaches a web account only by travelling through POS — which is the whole reason
this example exists alongside the single-source one.

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

## The steps

`pipeline.py` runs them in order and stops at the first failure. The basic track
is the same loop as [`../medallion-pyspark`](../medallion-pyspark/); the advanced
track is what a second and third source force.

| # | Step | What it does |
|---|---|---|
| 1 | `provision` | workspace, lakehouse, warehouse, workspace identity |
| 2 | `secret` | API key into Key Vault + an AKV-reference connection |
| 3 | `extract_load` | pull from Contoso POS, land verbatim under `Files/landing/` |
| 4 | `bronze` | a real DataPipeline: a Copy activity plus a Notebook activity |
| 5 | `engine` | Spark executes the queued notebook run and reports its read/write set |
| 6 | `silver` | PySpark: dedupe, conform countries, quarantine the malformed rows |
| 7 | `reflect` | reflect silver into the lakehouse SQL endpoint |
| 8 | `gold` | dbt-fabric builds the single-source star in the Warehouse |
| 9 | `dq_gate` | poison silver → dbt **fails** → restore → green again |
| 10 | `semantic_model` | publish TMSL + rows, query with DAX, refuse a wrong audience |
| 11 | `lineage` | assert the graph the emulator recorded |
| 12 | `web_extract` | second source: Contoso Web → its own Key Vault secret + landing |
| 13 | `web_bronze` | flatten the nested web export into line rows |
| 14 | `erp_extract` | third source: ERP change log + reference data, as Parquet |
| 15 | `erp_bronze` | three Copy activities reading a **columnar** source |
| 16 | `erp_scd2` | the change log becomes a dimension with history |
| 17 | `resolve` | resolve three customer sets transitively; name who cannot be |
| 18 | `star_silver` | materialise the resolution + the web order-line grain |
| 19 | `reflect` | reflect the resolved tables `star_silver` just wrote |
| 20 | `gold_star` | dbt-fabric: the multi-source star, joined in the Warehouse |
| 21 | `contract_gates` | run the ODCS contracts as gates at every layer |
| 22 | `tmdl_pbip` | serialise the model as TMDL; lay out a `.pbip` project |
| 23 | `govern` | catalog the medallion in OpenMetadata — domain, glossary, metrics, ODCS contracts, lineage (skips if OM is not running) |

`reflect` appears twice and neither is redundant: reflection happens on a
lakehouse **login**, so the first ran before `star_silver` existed.

`wrangle.py` is absent from the list on purpose — it is the interactive
profiling checkpoint for the VS Code Interactive Window, not a batch step.

## Watching the flow

The portal's **Data flow** view (<https://localhost:9443/#flow>) is a live
projection of work the emulator is already doing — no collector, no agent, and
nothing to instrument in your pipeline.

The graph comes from recorded lineage, so it is there before anything runs. The
log comes from the SSE stream, and nodes light up as writes land on them. Tail
it without a browser:

```sh
curl -N -k https://localhost:9443/_emulator/events
```

Four producers appear in this run, and the distinction is the point —
`Copy`/`Notebook` for the pipeline hops, `Reported` where an interactive step
declared its own derivations, and `Warehouse` where the TDS front watched dbt's
statements land. See [docs/31](../../docs/31-flow-observability.md).

## Requirements

- **Docker** for the emulator family (`docker compose up -d` at the repo root)
- **Microsoft ODBC Driver 18** for the dbt and TDS steps
  (macOS: `brew tap microsoft/mssql-release && brew install msodbcsql18`)
- **uv** for this example's own locked dependencies

> **Running from a git checkout rather than a release?** `docker compose up`
> starts the **published** image, which can lag `main`. These steps assert
> against the emulator's current behaviour, so a stale image fails in ways that
> look like example bugs. Build from your checkout:
>
> ```sh
> docker build -t ghcr.io/calvinchengx/fabric-emulator:dev .
> FABRIC_EMULATOR_VERSION=dev docker compose up -d
> ```

## Configuration and re-running

Endpoints default to the local stack and are all overridable — that is how the
CI harness points this same code at compose service names. See
[`../medallion-pyspark/README.md`](../medallion-pyspark/README.md#configuration)
for the full table; this example reads the same variables.

`provision.py` fails if the workspace already exists, since display names are
unique. Reset with `docker compose down -v && docker compose up -d`.
