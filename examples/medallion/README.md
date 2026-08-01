# Example: end-to-end medallion

A complete analytics loop against the emulator family — Entra tokens, a Key
Vault secret behind an `AzureKeyVaultReference` connection, extraction into
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

Then, in this directory:

```sh
uv init --bare                # only if you copied this directory elsewhere
uv add requests pandas pyarrow deltalake pyodbc dbt-fabric
uv run python run_all.py
```

Or run the steps one at a time, which is what the tutorial does:

```sh
uv run python 00_provision.py
uv run python 01_secret.py
…
```

You also need the **Microsoft ODBC Driver 18** for the dbt and TDS steps
(macOS: `brew tap microsoft/mssql-release && brew install msodbcsql18`), and a
Spark engine for `04_engine.py` — `docker compose up` starts Sail, and the step
reads `SPARK_REMOTE` (default `sc://localhost:50051`).

## The steps

| Script | What it does |
|---|---|
| `00_provision.py` | workspace + lakehouse + warehouse + workspace identity |
| `01_secret.py` | secret into Key Vault; AKV-reference connection (asserts the secret never reads back) |
| `02_extract_load.py` | pull from the vendor API, land it verbatim under `Files/landing/` |
| `03_bronze.py` | a real **DataPipeline**: a Copy activity the emulator executes, plus a Notebook activity |
| `04_engine.py` | **Spark (Sail)** executes the parsed cells and reports the run + its read/write set |
| `05_silver.py` | dedupe, conform countries, quarantine the malformed row |
| `06_wrangle.py` | **interactive**: profile bronze vs silver in VS Code Data Wrangler |
| `07_reflect.py` | connect to the lakehouse database — reflection makes silver queryable T-SQL |
| `08_gold.py` | `dbt build`: the star as views in the Warehouse, plus DQ tests |
| `09_dq_gate.py` | poison silver → dbt **fails** → restore → green again |
| `10_semantic_model.py` | publish TMSL + rows, query with DAX, assert a wrong audience is refused |
| `11_lineage.py` | assert the graph: a `Copy` edge and a `Notebook` edge, neither guessed from code |

`run_all.py` runs all of them except `06_wrangle.py`, which is meant for the
VS Code Interactive Window.

## Files

- `common.py` — endpoints, tokens for all five audiences, the TDS connector, state
- `source_system.py` — the fictitious "Contoso POS" vendor API and the expected
  row counts each stage must produce (the oracle every step asserts against)
- `gold/` — the dbt project (models, sources, schema tests, singular tests)
- `state.json` — written by `00_provision.py`, read by everything after it

## Configuration

Endpoints default to the local developer stack on `localhost` with self-signed
TLS. Every one is overridable, which is how the CI harness points the same code
at compose service names over plain HTTP:

| Variable | Default |
|---|---|
| `ENTRA_URL` | `https://localhost:8443` |
| `KV_URL` | `https://localhost:8444` |
| `FABRIC_REST_URL` | `https://localhost:9443` |
| `TDS_SERVER` | `localhost,1433` |
| `KV_INTERNAL_URL` | same as `KV_URL` — the vault URI **Fabric** resolves, which must be reachable from the emulator, not from you |
| `PIPELINE_STATE` | `./state.json` |
| `GOLD_PROJECT` | `./gold` |

## Re-running

`00_provision.py` fails if `contoso-analytics` already exists — display names
are unique. Delete the workspace, or reset the stack with
`docker compose down -v && docker compose up -d`.

Bronze appends by design, so a second full run doubles bronze rows; silver's
dedupe absorbs it and the assertions still hold.
