# e2e: the medallion tutorial, executed

The CI harness that runs [`examples/medallion`](../../examples/medallion/) — the
runnable witness for [docs/28-tutorial-end-to-end.md](../../docs/28-tutorial-end-to-end.md).

**This directory contains no pipeline code.** The example is the single copy of
it: the container runs `examples/medallion/run_all.py`, which executes the same
numbered scripts a reader runs by hand. Only the endpoints differ — supplied as
environment variables the example already reads — so nothing can pass in CI that
would fail when typed one line at a time.
A complete analytics loop — a fictitious SaaS source system whose API key lives
in **Key Vault**, extraction into **landing**, a **bronze → silver** medallion in
lakehouse Delta, **silver → gold with dbt** into the Warehouse (data-quality
tests included), and a **semantic model** queried over the Power BI
`executeQueries` wire — all against the emulator family, with every hop
authenticated by entra-emulator.

```
pipeline (pandas + delta-rs + ODBC Driver 18 + dbt-fabric)
  ├── REST      → fabric-emulator (control plane + OneLake)
  ├── TDS/1433  → fabric-emulator (FedAuth) → SQL Server sidecar
  ├── vault     → keyvault-emulator
  └── tokens    → entra-emulator (five audiences)
```

## Run

```
python3 e2e/medallion/run.py
```

Brings the stack up with docker-compose and asserts all nine steps pass
(`--exit-code-from pipeline`). Linux weight class (SQL Server container).

### Choosing the container engine (Apple Silicon in particular)

The suite runs on whatever Docker engine your context points at — it shells out
to `docker compose`, so `DOCKER_CONTEXT` (or `DOCKER_HOST`) selects the engine
without touching any file:

```sh
docker context ls                      # what you have
DOCKER_CONTEXT=orbstack python3 e2e/medallion/run.py    # run on a specific one
```

**Only `mcr.microsoft.com/mssql/server` is amd64-locked** — Microsoft ships no
arm64 manifest for it. Everything else in the stack (the three emulators,
Python, the Microsoft ODBC Driver 18) has native arm64 builds, and the
emulator's own `Dockerfile` cross-compiles with `--platform=$BUILDPLATFORM`, so
it builds at native speed for whatever architecture the host is.

That makes the fast configuration on Apple Silicon a **native arm64 engine**
(OrbStack, Docker Desktop, or `colima start --vm-type vz`), letting Rosetta
translate the single SQL Server container. Measured on this repo: the Go build
stage compiles `GOARCH=arm64` natively in a couple of minutes.

The slow configuration is a **fully emulated x86_64 VM** — e.g.
`colima start --arch x86_64` with the default `vmType: qemu` and
`rosetta: false`. There QEMU interprets the entire kernel and userland, the
build stage's "native" compile is itself emulated, and the same Go build takes
well over half an hour. If you want an x86 VM specifically, use Apple's
Virtualization.framework with Rosetta instead of QEMU:

```sh
colima start fabric-x86 --arch x86_64 --vm-type vz --vz-rosetta
```

CI runs `ubuntu-latest` (native amd64) and pays none of this.

## What each step proves

| # | Step | Assertion |
|---|---|---|
| 1 | Provision | workspace + lakehouse + warehouse created; workspace identity provisioned |
| 2 | Key Vault | secret stored; an `AzureKeyVaultReference` connection resolves for real (fabric fetches it with a vault-audience workspace-identity token) and the read shape **never returns the secret** |
| 3 | Extract → landing | the vendor refuses a wrong API key; the correct one (read back from Key Vault) yields an export landed verbatim in `Files/landing/` |
| 4 | Bronze | 8 customer rows + 8 order events appended to Delta with lineage columns — duplicates and the malformed row kept |
| 5 | Silver | 7 customers, 6 orders, 1 quarantined; countries conformed to `{US, GB, SG}`; `order_id` unique |
| 6 | Reflection | connecting to the lakehouse database reflects its Delta into SQL; `GROUP BY` over `silver_orders` returns 6 orders / 701.70 |
| 7 | Gold | `dbt build` green — 3 **table** models + 10 DQ tests over TDS via ODBC Driver 18, including dbt's **native `accepted_values` and `relationships`**, which compile to nested CTEs the emulator flattens on the wire ([docs/29](../../docs/29-tsql-parity.md)) |
| 8 | DQ gate | poisoning silver with a duplicate + negative-amount order makes `dbt build` **fail**, then restoring it makes gold green again |
| 9 | Semantic model | TMSL + rows published as a `SemanticModel` item; a DAX query over `executeQueries` returns the same 701.70; a wrong-audience token is rejected with 401 |

Step 8 is the point of the whole gold layer: a data-quality contract that never
fails is not a contract. The e2e proves the tests reject bad data, not merely
that they pass on good data.

## Layout

The harness is four files; everything it exercises lives in
[`examples/medallion`](../../examples/medallion/).

- `run.py` — brings the stack up, runs the example, tears it down.
- `docker-compose.yml` — entra, Key Vault, SQL Server, fabric (TDS on), pipeline.
- `Dockerfile.pipeline` — python + ODBC Driver 18, then `uv sync --frozen`
  against **the example's own lockfile** and `COPY examples/medallion/`. The
  example's dependencies never enter the emulator's dependency graph.
- `README.md` — this file.

## Documented adaptations

1. **Plain HTTP between services.** All three emulators run with TLS off, as the
   other containerized harnesses do, so none of the five TLS stacks in play (Go,
   Python/requests, rustls behind delta-rs, OpenSSL behind unixodbc, SQL Server)
   needs a CA distributed into it. The default developer stack
   (`docker-compose.yml`) keeps self-signed TLS **on** — mirroring production
   Azure trust is the product's point; it just isn't what this harness tests.

2. **Semantic-model rows are seeded, not Direct Lake.** The model's rows are
   exported from warehouse gold into a `data.json` definition part. Real Fabric
   would Direct-Lake them from OneLake Delta; the emulator's boundary here is
   recorded in [docs/18](../../docs/18-semantic-model-references.md), which also
   explains why `executeQueries` (not XMLA) is the contract served.
