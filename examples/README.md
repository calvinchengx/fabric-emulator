# examples

Working code that *uses* the emulator, kept deliberately apart from the code
that *is* the emulator.

Four medallions, on two axes. **Across:** one source system, or three that
share no key. **Down:** which engine builds the star — `dbt-fabric` over TDS
into a **Warehouse**, or `dbt-fabricspark` over Livy into the **Lakehouse**.
Fabric offers both and real shops pick one, so both are shown building the same
data from the same fixtures.

| Example | Sources | Gold built by | CI witness |
|---|---|---|---|
| [`medallion-pyspark/`](medallion-pyspark/) | one | `dbt-fabric` → Warehouse | [`e2e/medallion`](../e2e/medallion/) |
| [`medallion-advanced-pyspark/`](medallion-advanced-pyspark/) | **three** | `dbt-fabric` → Warehouse | [`e2e/medallion-advanced`](../e2e/medallion-advanced/) |
| [`medallion-dbt-fabricspark/`](medallion-dbt-fabricspark/) | one | `dbt-fabricspark` → Lakehouse | [`e2e/medallion-dbt-fabricspark`](../e2e/medallion-dbt-fabricspark/) |
| [`medallion-advanced-dbt-fabricspark/`](medallion-advanced-dbt-fabricspark/) | **three** | `dbt-fabricspark` → Lakehouse | [`e2e/medallion-advanced-dbt-fabricspark`](../e2e/medallion-advanced-dbt-fabricspark/) |

A fifth example is not a medallion at all. [`fab-driven/`](fab-driven/) does
the same bronze → silver work, but every control-plane action is performed by
**Microsoft's own `fab` CLI** — provision, upload, `fab import` of real item
definitions, `fab job run`, and read-back. It is the only example here whose
evidence comes from a client this project did not write, and building it found
two emulator defects nothing else had reached
([docs/34](../docs/34-fab-driven-example.md)).

| Example | Driven by | Gold built by | CI witness |
|---|---|---|---|
| [`fab-driven/`](fab-driven/) | **`fab`, Microsoft's CLI** | *(stops at silver)* | [`e2e/fab-driven`](../e2e/fab-driven/) |

Start with [`medallion-pyspark/`](medallion-pyspark/) — it is the code
[docs/28](../docs/28-tutorial-end-to-end.md) walks through. Go to
[`medallion-advanced-pyspark/`](medallion-advanced-pyspark/) for the problems a
second and third source create: identity that must be *resolved* rather than
joined, change over time, and conformance that is real.

The `contoso-fixtures*` directories are not examples. They are the **shared
seeded generators** every example builds from — one copy, so two examples can
never assert against different data.

## The conventions

**One directory per example, paired 1:1 with an `e2e/` harness of the same
name.** The harness owns the compose file and the CI plumbing; the example owns
every line of pipeline code. The harness runs the example *unmodified* — only
endpoints differ, passed as environment variables the example already reads. So
nothing can pass in CI that would fail for a reader typing the steps by hand.

**Each example owns its `pyproject.toml` and `uv.lock`.** Two reasons. It can be
copied out of this repo and run anywhere — `cp -r examples/medallion-pyspark ~/mine &&
cd ~/mine && uv sync && uv run python pipeline.py`. And its dependencies
(pandas, dbt, pyodbc, …) never enter the emulator's own dependency graph, which
stays about building and testing the emulator.

**The four medallion examples share one fixture package, and only that.** They
import `contoso-fixtures` for the seeded source-system generators and the target
resolver (`common.py`). That is a deliberate exception to copy-paste
independence, for a reason specific to these four: the comparison between them
asserts they produce *identical* silver, and two copies of a seeded generator
that drifted by a single RNG draw would compare two different datasets while both
still passed their own assertions. One generator makes that failure impossible
rather than merely unlikely. Everything else — every line of pipeline code, the
`pyproject.toml`, the lockfile, `definitions/` — each example owns.

**The target is resolved in exactly one file.** `contoso-fixtures/common.py`
turns `FABRIC_TARGET=emulator|real` into endpoints and a credential, through the
published `fabric-target` package. No step branches on which target is active;
they hold display *names* and receive resolved values, because item ids can never
match across targets. See [docs/21](../docs/21-real-fabric-toggle.md).

**Every example is executable end to end and asserts its own results.** An
example that merely *looks* right is a liability — these fail loudly instead.

## Your items are files, in Fabric's layout

Each example keeps its Notebook and DataPipeline definitions on disk, one
directory per item:

```
definitions/
  bronze-orders.Notebook/     .platform  notebook-content.py
  bronze-ingest.DataPipeline/ .platform  pipeline-content.json
```

`{display name}.{public facing type}` with the definition files plus
`.platform` is exactly what Fabric's Git integration writes when you connect a
real workspace to Azure DevOps or GitHub — so what an example commits is what
your repository holds in production, and the filenames (`notebook-content.py`,
`pipeline-content.json`) are the ones `updateDefinition` accepts. Ids that differ
per workspace are `{{TOKENS}}` substituted at deploy, the way Microsoft's
`fabric-cicd` uses a `find_replace` parameter file.

Definitions are versioned. The Delta tables they write live in OneLake and are
never in Git, and no deployment overwrites them.
[docs/46](../docs/46-artifact-persistence.md) is the contract, with a Microsoft
documentation link behind each claim.

## Secrets, and where the compute lives

**Secrets are read the way a notebook reads them.** `extract_load.py` calls
`notebookutils.credentials.getSecret(vault_uri, "contoso-pos-api-key")` — the
brokered path, where the workspace identity mints a vault-audience token and the
secret never appears in the notebook, a parameter, or a pipeline definition. It
works outside Fabric because this repo ships a working `notebookutils`
(`python/notebookutils/`): entra-emulator under `emulator`,
`DefaultAzureCredential` under `real`. Reading Key Vault's REST API directly
would reach the same secret and teach a shape that cannot move into a notebook.

`secret.py` covers the other half: the secret is stored in the vault and bound to
Fabric as an **AzureKeyVaultReference** connection, which Fabric resolves
server-side — so the connection's read shape never contains the value, and the
example asserts that.

**Compute and SQL addresses are discovered, never configured.** The SQL endpoint
of a warehouse or a lakehouse is assigned per item by the service, so every step
asks for it (`common.sql_endpoint()`) instead of naming a host; the enforcement
gate fails a step that names one. Spark is submitted, never dialled: Fabric
exposes no Spark endpoint, so the silver transform lives in
`definitions/silver.Notebook/` and `silver.py` submits it as a `RunNotebook` job
— the emulator's agent runs it locally, a Fabric pool runs it on a tenant, and
the file itself touches no Spark. `engine.py` skips under `real` because Fabric
runs the queued notebook on its own pool. That whole story,
including starter versus custom pools and the analytics-endpoint sync lag, is in
[docs/46](../docs/46-artifact-persistence.md).

## Where the fidelity proof lives — and it is not here

These four show what the emulator **does**. They do not show it is **right**,
and a reader deciding whether to trust the project is asking the second
question: *would my code notice the difference?*

Nothing below is exercised by any example. Each row names the suite that runs
it, and `test_every_fidelity_claim_names_a_suite_that_exists` fails when one of
these paths stops being real — so this table cannot rot into a promise nobody
keeps.

**Microsoft's own tools, pointed at the emulator.** The strongest evidence
available, because their expectations decide the outcome rather than ours:

| Claim | Suite |
|---|---|
| The `fab` CLI does workspace and item CRUD | [`e2e/fabric-cli`](../e2e/fabric-cli/) |
| `fab` imports real definitions and RUNS them, and the bytes check out | [`e2e/fab-driven`](../e2e/fab-driven/) |
| `fabric-cicd` deploys a git-format folder | [`e2e/fabric-cicd`](../e2e/fabric-cicd/) |
| The VS Code extension browses and runs | [`e2e/vscode-extension`](../e2e/vscode-extension/) |
| ADOMD.NET connects, on Linux, to a host we name | [`e2e/xmla`](../e2e/xmla/) |
| **Power BI Desktop** opens a model built from our TMSL | [`e2e/pbix-desktop`](../e2e/pbix-desktop/) |

**Fabric behaviours, not conveniences:**

| Claim | Suite |
|---|---|
| Deployment pipelines promote dev → test → prod | [`e2e/deployment-pipelines`](../e2e/deployment-pipelines/) |
| OneLake shortcuts resolve to external storage | [`e2e/external-shortcuts`](../e2e/external-shortcuts/), [`e2e/s3`](../e2e/s3/) |
| A notebook runs on the SERVICE, no runner attached | [`e2e/notebook-driven`](../e2e/notebook-driven/) |
| `notebookutils` behaves as it does inside Fabric | [`e2e/notebookutils`](../e2e/notebookutils/) |
| Livy sessions and statements run on a real engine | [`e2e/livy`](../e2e/livy/) |
| Real-Time Intelligence over a real Kusto engine | [`e2e/rti`](../e2e/rti/) |
| The same code runs against **real Fabric** | [`e2e/fabric-target`](../e2e/fabric-target/) |

**Other people's clients, unmodified:** [`e2e/delta-rs`](../e2e/delta-rs/),
[`e2e/duckdb`](../e2e/duckdb/), [`e2e/azcopy`](../e2e/azcopy/),
[`e2e/adls-sdk`](../e2e/adls-sdk/), [`e2e/airflow`](../e2e/airflow/),
[`e2e/great-expectations`](../e2e/great-expectations/), and
[`e2e/spark-jvm`](../e2e/spark-jvm/) — Apache Spark on the JVM as an oracle for
Sail.

[docs/12](../docs/12-e2e-matrix.md) is the full matrix of what each asserts;
[docs/parity.md](../docs/parity.md) records what is real, partial, or deferred
with cause.

### What no example touches, stated rather than left to be noticed

The controllable clock, fault injection, schedules, event triggers, and
put-if-absent (`If-None-Match` — the Delta concurrency primitive).

The fullest demonstration of those is not in this repository at all. The
[contoso-data-platform](https://github.com/calvinchengx/contoso-data-platform)
consumer drives the clock, creates schedules and Reflex triggers, and implements
the `FABRIC_TARGET` toggle. The emulator's own examples show a data platform; a
downstream consumer shows the emulator's fidelity. That is the wrong way round.

## Running one

Start the family from the repo root, then follow the example's README:

```sh
docker compose up -d
cd examples/medallion-pyspark && uv sync && uv run python pipeline.py
```

Or run its CI harness, which does the same thing in containers:

```sh
python3 e2e/medallion/run.py
```
