# 16 — Warehouse: T-SQL over TDS with Entra FedAuth

**Status: design.** Plans the one R-track item left deferred: a real SQL
endpoint that unmodified SQL clients — `sqlcmd`, `pyodbc`/`pymssql`,
`go-mssqldb`, the JDBC mssql driver, SSMS, Power BI DirectQuery — connect to
over **TDS (port 1433)** authenticating with an **Entra token (FedAuth)**, and
run real T-SQL against lakehouse Delta data.

Follows the same principle as the rest of the R-track
([14-real-compute.md](14-real-compute.md)):

> **Never fake results. Either do it for real (attached real engine) or fail
> honestly (501).**

## The split: protocol we own, engine we attach

T-SQL-over-TDS is not one capability, it is three layers with different
feasibility under the project's `CGO_ENABLED=0`, pure-Go, distroless
constraint:

| Layer | Feasible in-binary? | Plan |
|---|---|---|
| TDS wire server (PRELOGIN / LOGIN7 / token streams) | Yes — pure Go, written here (no mature Go TDS *server* lib exists; mature *clients* do) | `internal/tds` |
| FedAuth termination (validate the Entra token) | Yes — reuse `internal/auth` against entra-emulator's JWKS | `internal/tds` |
| **T-SQL execution** (parse + run SELECT/JOIN/CTE/window over Delta) | **No** — no pure-Go/no-CGO T-SQL engine exists (DuckDB needs CGO; SQLite ≠ T-SQL) | **sidecar** |

So the query engine must be a **real backend sidecar** — SQL Server on Linux
(`mcr.microsoft.com/mssql/server`) or **Babelfish** (T-SQL on Postgres). That
is the same "different weight class" as the Spark sidecar: a compose service,
never embedded in the binary. Everything *around* it — the TDS protocol and the
Entra FedAuth handshake — is pure Go and lives here.

## Where this lives — **in this repo, not a sibling**

An earlier note floated a sibling repo. That was wrong, and it contradicts
[14-real-compute.md](14-real-compute.md) ("Where this lives → **In this
repository**", listing *compose-level sidecar attachments (Spark, DuckDB,
Babelfish)*). The correct precedent is **Livy**:

- the **Livy proxy** is pure Go **in this repo** (`internal/api/livy.go`);
- the **Spark engine** it fronts is a **compose sidecar**.

TDS is the exact same shape — a pure-Go protocol front-end in this repo
(`internal/tds`) in front of a SQL-engine sidecar in `docker-compose.yml`.
"The engine must be a sidecar" is a hard constraint; "it must be a separate
repo" never followed from it. Sidecars are containers regardless of which
repo's compose file launches them; the repo boundary in this family tracks
**service/trust surfaces** (STS = entra-emulator, vault = keyvault-emulator),
and the Fabric SQL endpoint is a Fabric surface — so it belongs here.

*The one condition that would justify extraction later:* the TDS-FedAuth proxy
is a **generic** primitive (Entra federated auth in front of any SQL Server),
useful outside Fabric — the same reason entra/keyvault are their own emulators.
Keeping it self-contained in `internal/tds` (no Fabric-specific imports in the
protocol layer) makes a future extraction cheap **if** that reuse ever
materializes. Until then, bundling is the consistent, lower-friction choice.

## Architecture

```
  SQL client (sqlcmd / pyodbc / SSML / Power BI)
        │  TDS/1433 + FedAuth (Entra access token, audience database.windows.net)
        ▼
  ┌──────────────────────────── fabric-emulator (this repo) ─────────────┐
  │  internal/tds — TDS server                                            │
  │   • PRELOGIN → LOGIN7 → FEATUREEXT(FEDAUTH, SecurityToken)            │
  │   • validate token via internal/auth (entra JWKS)  ── reuse ──────────┼──▶ entra-emulator
  │   • map Fabric workspace/lakehouse → target database + SQL login      │
  │   • relay SQLBatch / RPC token streams both ways                      │
  │  internal/warehouse — Delta⇄SQL reflection                            │
  │   • materialize lakehouse Tables/<t> (Delta) into sidecar tables      │
  └──────────────────────────────────┬───────────────────────────────────┘
                                      │  go-mssqldb, SQL auth (fixed service login)
                                      ▼
                          SQL Server / Babelfish sidecar  ── reads ──▶ OneLake Delta
```

### 1. TDS front leg (pure Go)
Terminate the client TDS connection: PRELOGIN (encryption negotiation),
LOGIN7 with the `FEDAUTH` feature extension. Support the **SecurityToken**
FedAuth library mode first (the client already holds an Entra access token and
presents it in the handshake) — the service-principal / `ActiveDirectory*`
driver path. Interactive/browser flows are out of scope.

### 2. FedAuth termination (reuse existing auth)
Validate the presented token against entra-emulator's JWKS with a **new
audience** — `https://database.windows.net/` (Azure SQL / Fabric SQL resource).
Seed the app in entra the way the Storage app is seeded
(`POST {entra}/admin/api/apps {"appIdUri":"https://database.windows.net"}`) so
client-credentials resolve `https://database.windows.net/.default`. The
validated principal → the workspace RBAC already enforced everywhere else.

### 3. Backend leg + auth bridge
The sidecar can't validate tokens against a *fake* entra issuer, so the proxy
**terminates FedAuth and re-authenticates to the sidecar with SQL auth** (a
fixed emulator service login). This is why it is a *FedAuth-terminating proxy*,
not a byte pipe: the two legs authenticate differently, so LOGIN7 must be
parsed and a fresh backend session opened, then SQLBatch/RPC token streams
relayed. `go-mssqldb` drives the backend leg.

### 4. Data plane — the interesting open problem
Fabric's **SQL analytics endpoint** exposes a lakehouse's Delta tables as
read-only SQL. The emulator needs those tables queryable in the sidecar. Two
approaches, start with the first:

- **Lazy materialization (v1):** on connect (or on first reference), reflect
  each `Tables/<name>` Delta table into a sidecar table — read the Delta log +
  Parquet (pure-Go reader, or via the sidecar's own Parquet ingest), infer the
  schema, `CREATE TABLE` + bulk load. Read-only, eventually-consistent, schema
  inferred. Enough for the real-client oracle.
- **External tables / PolyBase (later):** point the sidecar at the Parquet
  directly. Heavier setup and OneLake's custom endpoint complicates blob
  access; revisit only if materialization proves too lossy.

Full **Warehouse** read-write T-SQL (DDL/DML persisted back to OneLake Delta)
is a larger sync problem — later milestone; v1 targets the read path (analytics
endpoint semantics) that maps onto existing Delta data.

## Milestones

- **T1 — protocol oracle. ✅ Done.** Pure-Go TDS server (`internal/tds`):
  PRELOGIN → FedAuth `LOGIN7` (Entra token extracted from the SecurityToken
  FeatureExt, UTF-16LE) → token validated against entra's JWKS with the
  `database.windows.net` audience → `LOGINACK` → `SELECT 1` answered with a real
  result-token stream (COLMETADATA/ROW/DONE). Behind `-sql-tds-addr`
  (`FABRIC_SQL_TDS_ADDR`); off when unset. Proven against the **real Microsoft
  `go-mssqldb` driver**: LOGIN7 token capture, accept/reject by audience, and a
  full server e2e (real entra token → FedAuth login → `SELECT 1` = 1; a
  wrong-audience token is refused). No sidecar — the unique, in-family part.
- **T2 — real engine.** Attach the T-SQL sidecar (Babelfish or SQL Server —
  same proxy); relay arbitrary SQLBatch; real T-SQL over sidecar-native tables.
  `--warehouse-sql-url` (unset → honest 501, mirroring `--spark-livy-url`).
- **T3 — lakehouse data.** Delta→sidecar reflection; query real
  `Tables/<name>` data written by delta-rs/Spark elsewhere in the family — the
  cross-engine warehouse oracle (delta-rs writes, T-SQL reads).
- **T4 — RBAC + parity.** Map workspace roles → SQL permissions; connection
  string / `information_schema` shape parity.

## Borrowed oracles (the CI proof)

`e2e/warehouse-tds/` (Linux; the sidecar is a container weight class, like
`spark-a2`): bring up entra + fabric + the SQL sidecar; a real client
(`pyodbc` with `ActiveDirectoryServicePrincipal`, and `go-mssqldb`) connects
over TDS with an entra token and runs `SELECT … JOIN … WHERE` over a lakehouse
Delta table, results matching what DuckDB (R3) returns over the same data — two
independent SQL engines agreeing.

## Non-goals

- A hand-written T-SQL engine (that's the sidecar's job).
- Interactive/browser FedAuth flows (service-principal / access-token only).
- Write-back to OneLake Delta from T-SQL DML (v1 is read-path).
- Full T-SQL surface fidelity — bounded by whatever the chosen sidecar
  (SQL Server vs Babelfish) supports.

## Risks

- **No mature Go TDS *server*.** The handshake + token-stream codec is written
  here; bounded but real (weeks), like a Postgres wire server.
- **FedAuth sub-protocol detail.** The FEATUREEXT/FEDAUTH negotiation must
  match what real drivers send; SecurityToken mode first narrows this.
- **Sidecar weight + startup.** A SQL Server container is heavy; Linux-only CI,
  gated like the Spark job.
- **Materialization fidelity.** Schema inference from Parquet and
  read-only/eventual semantics differ from a native SQL-analytics endpoint;
  documented, not hidden.

## Decision record

- **Engine:** a real T-SQL **sidecar**, and **pluggable** — because the backend
  leg is just TDS + a SQL login, the proxy is identical for either engine, and
  only the compose service + the e2e's `--warehouse-sql-url` change. Selected by
  platform: **Babelfish on macOS** (Apache-2.0, ARM-native, lighter),
  **SQL Server on Linux/Windows and in CI** (highest T-SQL fidelity; the CI
  oracle runs on Linux). Hard constraint: no in-binary T-SQL under no-CGO, so it
  is always a sidecar, never embedded.
- **Protocol + FedAuth:** pure Go, **in this repo** (`internal/tds`), following
  the Livy-proxy precedent — *not* a sibling repo.
- **Priority:** deferred until there is demand for the real-client
  (SSMS/pyodbc/Power BI-over-TDS) oracle; it re-proves SQL semantics already
  exercisable via DuckDB (R3), so its marginal value is the TDS/FedAuth
  real-client surface specifically.
- **Extraction:** reconsider only if the TDS-FedAuth proxy proves independently
  reusable outside Fabric; `internal/tds` stays Fabric-import-free to keep that
  option cheap.
