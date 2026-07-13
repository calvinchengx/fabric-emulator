# 16 — Warehouse: T-SQL over TDS with Entra FedAuth

**Status: T1–T5 shipped and verified against a real SQL Server — including a
second, independent driver family (Microsoft ODBC Driver 18 via dbt-fabric).** A
real SQL endpoint that unmodified SQL clients — `sqlcmd`, `pyodbc`/`pymssql`,
`go-mssqldb`, the JDBC mssql driver, SSMS, Power BI DirectQuery — connect to
over **TDS (port 1433)** authenticating with an **Entra token (FedAuth)**, and
run real T-SQL against lakehouse Delta data. The engine is a **SQL Server
sidecar**; the emulator reflects lakehouse Delta into it (§4) — *not* PolyBase,
which a spike proved is a dead-end on Linux.

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

So the query engine must be a **real backend sidecar** — **SQL Server on Linux**
(`mcr.microsoft.com/mssql/server`), one engine on every platform (see the
decision record for why we standardised on it over Babelfish). That is the same
"different weight class" as the Spark sidecar: a compose service, never embedded
in the binary. Everything *around* it — the TDS protocol and the Entra FedAuth
handshake — is pure Go and lives here.

## Where this lives — **in this repo, not a sibling**

An earlier note floated a sibling repo. That was wrong, and it contradicts
[14-real-compute.md](14-real-compute.md) ("Where this lives → **In this
repository**", listing *compose-level sidecar attachments*). The correct
precedent is **Livy**:

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
  SQL client (sqlcmd / pyodbc / SSMS / Power BI)
        │  TDS/1433 + FedAuth (Entra access token, audience database.windows.net)
        ▼
  ┌──────────────────────────── fabric-emulator (this repo) ─────────────┐
  │  internal/tds — TDS server                                            │
  │   • PRELOGIN → LOGIN7 → FEATUREEXT(FEDAUTH, SecurityToken)            │
  │   • validate token via internal/auth (entra JWKS)  ── reuse ──────────┼──▶ entra-emulator
  │   • map Fabric workspace/lakehouse → target database + SQL login      │
  │   • relay SQLBatch / RPC token streams both ways                      │
  │  internal/warehouse — Delta→SQL reflection (pure Go)                  │
  │   • read lakehouse Tables/<t> Delta (parquet-go) ── reads ────────────┼──▶ OneLake (this emulator)
  │   • CREATE TABLE + INSERT the rows into the sidecar                   │
  └──────────────────────────────────┬───────────────────────────────────┘
                                      │  go-mssqldb, SQL auth (fixed service login)
                                      ▼
                 SQL Server (Linux) sidecar  ◀── plain rows (no Delta read, no PolyBase)
```

**Read the arrows carefully — this is the crux.** The **emulator** reads OneLake
Delta (its own pure-Go reader); **SQL Server never touches OneLake**. It receives
ordinary `CREATE TABLE` + `INSERT` + runs plain T-SQL `SELECT`. This is *not*
PolyBase (SQL Server reading Delta itself) — that path is a proven dead-end on
Linux (see §4). The sidecar is a vanilla T-SQL engine on rows we hand it.

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

### 4. Data plane — two surfaces, one engine (resolved)

Fabric exposes **two** T-SQL surfaces, both over the same TDS front. The
emulator routes by the connection's `database` (a Fabric item id) to the right
strategy behind the same SQL Server sidecar:

| Surface | Item type | Strategy | Access |
|---|---|---|---|
| **Lakehouse SQL analytics endpoint** | `Lakehouse` | **Reflection** — the emulator reads each `Tables/<t>` Delta (pure-Go: replay `_delta_log` + `parquet-go`) and `CREATE TABLE`+`INSERT`s it into the sidecar on connect | **read-only** mirror of externally-written Delta |
| **Warehouse** | `Warehouse` | **Direct relay** — the client's own `CREATE`/`INSERT`/`SELECT` go straight to the sidecar, which owns the data | **read-write** T-SQL |

Reflection exists **only** for the lakehouse endpoint — it bridges Delta that
was written *outside* SQL Server (by Spark / delta-rs / notebooks) into the
query engine. The warehouse needs no reflection: its data is created *in* the
warehouse via T-SQL, so it is already native to the sidecar.

**Why reflection, not PolyBase — settled by a spike, not a hunch.** The
tempting alternative is to point SQL Server at the OneLake Delta *directly*
(`CREATE EXTERNAL DATA SOURCE` / `OPENROWSET(FORMAT='DELTA')`, i.e. PolyBase).
A full spike proved this is a **dead-end on the Linux `mssql/server`
container**, at the wire and package level:

- SQL Server 2022 Linux does not even register the `abs`/`adls` scheme
  processors (`111631: scheme not valid`).
- SQL Server 2025 Linux registers them (DDL parses), but the object-storage
  read routes through a Java `HdfsBridge.jar` + JRE that `mssql-server-polybase`
  **does not ship** on Linux (it installs only `libDMSNative.so` + gRPC + the
  `.sfp` bundle). A `tcpdump` confirmed the connector makes **zero** network
  calls — it fails in-process before any I/O, independent of DNS, TLS trust,
  and SAS validity. The components exist only on **Windows** PolyBase.

So reflection is the **permanent** design, not a v1 stopgap: the emulator reads
Delta (it already can, in pure Go) and hands the sidecar plain rows. (The spike
was a throwaway investigation; its finding and root cause are recorded here, not
kept as a harness.)

**Cross-engine oracle.** The same lakehouse Delta is queried independently by
**DuckDB** (R3, `e2e/duckdb/`) — two engines agreeing on the result is the
correctness proof for the reflection path.

*Since resolved (T4/T5):* per-item database isolation (each lakehouse/warehouse
gets its own SQL Server database — no collisions), per-column type fidelity
(native SQL types over the wire), RBAC → SQL permissions, and connect-by-name.
*Still genuinely deferred:* write-back of Warehouse DML to OneLake Delta (the
warehouse owns its own data in the sidecar; it is not mirrored back to Delta).

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
- **T2 — real engine. ✅ Done.** With `-warehouse-sql-url` set, the endpoint
  relays each authenticated SQLBatch to a real **SQL Server** over `go-mssqldb`
  and streams the result back (COLMETADATA/ROW/DONE; DDL/DML → bare DONE; engine
  errors → SQL ERROR). Unset → the T1 stub. The relay is validated against the
  real `go-mssqldb` client with a fake backend (multi-column/NULL round-trip,
  error surfacing) and the row-materialisation against in-memory SQLite; a
  gated e2e (`WAREHOUSE_MSSQL_DSN`, CI Linux with a SQL Server service) runs
  real DDL + DML + `GROUP BY` end to end: entra token → FedAuth login → real
  T-SQL on the engine. Result columns are currently all NVARCHAR (the client
  converts on scan); per-column type fidelity landed later in T4b/T5.
- **T3 — lakehouse data. ✅ Done.** On connect (database = lakehouse item id),
  the emulator reads each `Tables/<name>` **Delta table** from OneLake in pure
  Go (`internal/warehouse`: replay `_delta_log`, read Parquet via
  `parquet-go`) and reflects it into the engine (DROP/CREATE with inferred
  types + literal INSERT), so `SELECT` hits real OneLake data. The Delta reader
  + reflection are unit-tested (real Parquet round-trip, add/remove
  supersession, type inference, SQLite materialization); a gated e2e writes a
  Delta table into OneLake and a real client `GROUP BY`s it through the endpoint
  to the SQL Server engine — `us=90, eu=60`, matching DuckDB (R3): the
  cross-engine oracle. *Limitations:* reflected tables land in the engine's
  default database (per-item database isolation landed in T4a); re-reflects
  on each connect; `NVARCHAR(4000)`/no-checkpoint like T2's type caveat.
  Verified locally against a real `mcr.microsoft.com/mssql/server:2022`
  container (all three warehouse e2es pass), not just in CI.
- **T4a — both surfaces, isolated. ✅ Done.** Explicit item-type routing behind
  one TDS front (`warehouseRouter`): the connection's `database` is a Fabric
  item id, and **each item is its own SQL Server database** (`EnsureDatabase`
  per item id — no cross-item collision). A **Lakehouse** → reflect its Delta +
  **read-only** (writes rejected with a clear error, as real Fabric does); a
  **Warehouse** → **read-write** relay (its data is native to the engine, no
  reflection); unknown or non-SQL items reject the login. Per-database pools are
  opened lazily from one parsed base DSN (`msdsn` + `NewConnectorConfig`).
  Unit-tested (routing branches, per-db pool caching, read-only guard, name
  safety) + a gated two-surface e2e (`TestWarehouseTwoSurfaces`) proving
  warehouse read-write, lakehouse read-only rejection, and isolation against a
  real SQL Server.
- **T4b — RBAC + parity. ✅ Done.**
  1. **RBAC → SQL permissions. ✅** On connect, the token's principal is resolved
     and its **workspace role** is enforced (`warehouseRouter`): no role → login
     rejected; Viewer → read-only; Contributor/Member/Admin → read-write on a
     Warehouse (a Lakehouse endpoint is always read-only). Unit-tested (each role
     tier + deny) + a wire-level e2e (a principal with no role on the item's
     workspace is rejected).
  2. **`information_schema` parity. ✅** Reflected/warehouse tables are real SQL
     Server tables in the item's database, so `INFORMATION_SCHEMA.*` / `sys.*`
     relay natively — schema-introspecting tools (SSMS, Power BI) see the real
     shape. Covered by the two-surface e2e (`INFORMATION_SCHEMA.TABLES`).
  3. **Per-column type fidelity. ✅** Integer/float/bit columns carry their real
     type from the engine (`rows.ColumnTypes()`) into the TDS COLMETADATA + row
     encoding (INTN/FLTN/BITN, with NULLs); other types keep the NVARCHAR-text
     fallback (still converts on scan). A typed client reads `int64`/`float64`/
     `bool` directly. Round-trip-tested through the real `go-mssqldb` driver
     (typed scans + NULLs + reported column types) and end-to-end (the reflected
     INT column reads back as an integer type, not text).
- **T4c — connection by item name. ✅ Done.** Real Fabric connects with the
  lakehouse/warehouse *display name* as the database and the **workspace encoded
  in the server name** (`<workspace>.datawarehouse.fabric.microsoft.com`). The
  router (`resolveSQLItem`) now accepts both: a GUID resolves by item id
  (workspace-agnostic, back-compat); otherwise the database is a display name and
  the workspace is taken from the LOGIN7 server name's first DNS label (by id or
  name), then the item is looked up by name (Warehouse preferred, then Lakehouse).
  `OnConnect` returns the resolved item id so queries route to the item's own
  backend database regardless of how the client addressed it. Covered by the
  router unit test (name + workspace-by-id/by-name, missing workspace, unknown
  name, no workspace in the server name) and a wire-level e2e: a real `go-mssqldb`
  client connects with a `fixedDialer` that sends the Fabric server name in
  LOGIN7 while dialing the test listener, and reads back the same backend database
  as the GUID connection (a lakehouse-by-name write is still rejected read-only).
- **T5 — second real driver family (Microsoft ODBC Driver 18). ✅ Done.** The CI
  proof was a `go-mssqldb` test; the ODBC driver (pyodbc, and Microsoft's real
  **dbt-fabric** adapter) is a genuinely independent TDS implementation and a far
  stricter client. Making it work required two things:
  1. **Login-response fidelity.** go-mssqldb tolerated a lean login response; the
     ODBC driver's state machine did not. The PRELOGIN now reports a real server
     version (16.0 — the driver refuses a `0.0.0.0` "SQL Server 2000") and a
     FEDAUTH FEATUREEXTACK is emitted (without it the connection never becomes
     ready).
  2. **Session splice (the load-bearing change).** A re-encoding relay
     (run each batch through go-mssqldb, re-emit COLMETADATA/ROW/DONE) structurally
     can't reproduce the token stream a strict client depends on — transaction
     ENVCHANGEs, `sp_datatype_info` metadata, native column types — and the driver
     desynced on RPCs/`sp_executesql` and prepared statements. So after
     terminating the FedAuth login the emulator now **byte-forwards** the client's
     post-login session straight to a real per-item SQL Server connection
     (`internal/tds/splice.go`, `client.go`): SQL Server generates every response
     token itself. Crucially, the engine's **own login response is forwarded** to
     the client (with the FEDAUTH ack merged in) so the client's session state —
     collation, server identity, the begin-transaction ENVCHANGE that suppresses
     the driver's autocommit fallback — matches the engine it is about to talk to.
     go-mssqldb clients splice too (perfect type fidelity, real transactions);
     fake test backends keep the re-encode relay. The read-only guard peeks
     forwarded SQL batches and rejects writes before they reach the engine.

  **Proven:** Microsoft's real `dbt-fabric` adapter runs its full lifecycle —
  `dbt debug` → `seed` → `run` → `test` (all green) — through pyodbc + ODBC
  Driver 18 over the TDS front (`e2e/dbt-fabric/`), and a rich pyodbc suite
  (DDL, parameterized RPCs, commit/rollback, `INFORMATION_SCHEMA`) round-trips.
  The splice + client login are unit-tested in-process (a TDS client against our
  own server, the splice over pipes — no SQL Server needed).

## Borrowed oracles (the CI proof)

Two independent driver families exercise the surface in CI (Linux; the sidecar
is a container weight class, like `spark-a2`):

- **`warehouse-tds` job** — gated Go tests (`internal/server/tds_*_test.go`,
  behind `WAREHOUSE_MSSQL_DSN`) drive a real **`go-mssqldb`** client over TDS
  with an entra token: FedAuth login → DDL + DML + `GROUP BY` on the real engine,
  plus the two-surface / RBAC / type-fidelity / connect-by-name assertions. The
  lakehouse `SELECT` result matches what **DuckDB** (R3, `e2e/duckdb`) returns
  over the same Delta — two independent SQL engines agreeing.
- **`dbt-fabric` job** (`e2e/dbt-fabric/`) — Microsoft's real **dbt-fabric**
  adapter, over **pyodbc + Microsoft ODBC Driver 18** (a genuinely independent
  TDS implementation from go-mssqldb), runs a full project `debug → seed → run →
  test` against the warehouse: the FedAuth login is validated and the session is
  byte-spliced to the sidecar, so RPCs / prepared statements / transactions all
  flow through. This is the T5 second-driver witness.

## Non-goals

- A hand-written T-SQL engine (that's the sidecar's job).
- Interactive/browser FedAuth flows (service-principal / access-token only).
- Write-back to OneLake Delta from T-SQL DML (v1 is read-path).
- Full T-SQL surface fidelity — bounded by what the SQL Server sidecar
  supports (very high, but not the proprietary Fabric Polaris engine).

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

- **Engine: SQL Server on Linux (`mcr.microsoft.com/mssql/server`), one engine
  on every platform.** Hard constraint: no in-binary T-SQL under no-CGO, so it
  is always a sidecar, never embedded. We considered a per-platform split
  (Babelfish on macOS, SQL Server elsewhere) and **rejected it**:
    - *Fidelity is the product.* Babelfish is a T-SQL *reimplementation on
      PostgreSQL*, not the SQL Server engine — it diverges on collation, error
      numbers, `information_schema`/system-view shapes, and datatype edges. An
      emulator that sells fidelity shouldn't ship a *different* engine to Mac
      devs than to CI and real Fabric.
    - *The ARM win mostly evaporates.* There is no official arm64-native
      Babelfish image; community images are x86, so on Apple Silicon it runs
      under Rosetta/qemu emulation anyway — the same emulation SQL Server needs.
      Babelfish would only be *lighter* under emulation, not native.
    - *One engine = one set of quirks*, one CI oracle, no risk of the Mac path
      being the less-tested one.
  - **Cost accepted:** on Apple Silicon SQL Server runs under x86 emulation
    (slower, ~2 GB RAM), and the image requires `ACCEPT_EULA=Y` (Developer
    edition, free for dev/test; users pull Microsoft's image and accept the
    EULA themselves). Because the proxy's backend leg is just TDS + a SQL login,
    swapping in Babelfish later is a one-line `--warehouse-sql-url` change if
    anyone wants the lighter local loop — but the default is SQL Server.
- **Protocol + FedAuth:** pure Go, **in this repo** (`internal/tds`), following
  the Livy-proxy precedent — *not* a sibling repo.
- **Priority:** the real-client (pyodbc/ODBC-Driver-18/dbt-fabric-over-TDS)
  oracle **shipped** (`e2e/dbt-fabric`). Beyond re-proving SQL semantics already
  exercisable via DuckDB (R3), its marginal value — realized — is the TDS/FedAuth
  real-client surface with a second, independent driver family.
- **Extraction:** reconsider only if the TDS-FedAuth proxy proves independently
  reusable outside Fabric; `internal/tds` stays Fabric-import-free to keep that
  option cheap.
