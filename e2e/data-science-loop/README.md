# Data science loop witness

This composed E2E proves one physical OneLake Delta table across the phase:

1. PySpark sends SQL through Spark Connect; LakeSail's Sail executes it and
   writes Delta logs and Parquet files through the emulator's OneLake API.
2. A compatibility-level-1604 TMSL semantic model binds a Direct Lake entity
   partition to that Lakehouse. `executeQueries` evaluates DAX over the current
   Delta snapshot.
3. The real MLflow 3 client sends an experiment, run, metric, artifact, and
   registered model through the emulator's authenticated workspace proxy. The
   emulator synchronizes typed Fabric items and mirrors the artifact to OneLake.
4. dbt-duckdb loads the same table with its built-in `delta` plugin, builds an
   aggregate model, and passes four data tests.

## Engine boundary

OneLake is not a query engine. In this emulator it is the Go DFS/Blob protocol
service backed by the emulator store. Sail is the Spark execution engine and
ships its own Rust `sail-delta-lake`, DataFusion, `object_store`, and Parquet
components. No separate delta-rs service is required for Sail.

The Python `deltalake` package (delta-rs) is present in the witness because
dbt-duckdb's `delta` plugin imports it. It is an in-process client library, not
a daemon in the Fabric stack. Direct Lake and warehouse reflection use the
emulator's narrower pure-Go Delta-log/Parquet reader.

Dependencies are declared in the root `pyproject.toml`, locked in `uv.lock`,
and installed into the containers with the `mlflow` and `data-science-loop`
groups.

Run from the repository root:

```bash
python3 e2e/data-science-loop/run.py
```
