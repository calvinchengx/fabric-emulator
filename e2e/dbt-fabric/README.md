# e2e: dbt-fabric

Microsoft's real [dbt-fabric](https://github.com/microsoft/dbt-fabric) adapter
runs a dbt project against **fabric-emulator's TDS warehouse surface** via the
**Microsoft ODBC Driver 18**. It is the *driver-diversity* witness for the TDS
front: everything else proves the wire against `go-mssqldb`, while dbt-fabric
drives it through pyodbc + ODBC Driver 18 — an independent TDS implementation
with its own FedAuth/prelogin handshake.

```
dbt-fabric (pyodbc + ODBC Driver 18) → fabric-emulator (TDS + FedAuth) → SQL Server sidecar
       ↑ Azure-SQL FedAuth token minted by entra-emulator
```

## Run

```
python3 e2e/dbt-fabric/run.py
```

Brings the stack up with docker-compose and asserts `dbt debug → seed → run →
test` all pass (`--exit-code-from dbt`). Linux weight class (SQL Server
container); the ODBC driver + SQL Server image are amd64-native.

## How it maps onto the emulator

- **Auth.** dbt-fabric's `ActiveDirectoryAccessToken` mode takes a pre-minted
  `access_token` and injects it into pyodbc's `SQL_COPT_SS_ACCESS_TOKEN`
  attribute — so the ODBC driver performs **FedAuth without ever running MSAL**
  (no authority redirect to work around). The token is forged from
  entra-emulator with audience `https://database.windows.net`, the FedAuth
  audience the TDS front validates. A separate control-plane token
  (`api.fabric.microsoft.com`) is used only to provision the workspace +
  warehouse over REST.
- **No TLS on the data path.** The TDS front terminates FedAuth without TLS
  (advertises `ENCRYPT_NOT_SUP`), so dbt connects with `encrypt: false` —
  mirroring the `go-mssqldb` `encrypt=disable` path. Whether ODBC Driver 18
  accepts FedAuth over an unencrypted connection is exactly the driver-diversity
  question this e2e settles.
- **Warehouse = its own database.** dbt connects with `Database=<warehouse id>`;
  the emulator's warehouse router enforces RBAC (the daemon SP is Admin →
  read-write) and ensures the per-item database exists, then relays T-SQL to the
  SQL Server sidecar.

## Scope note — vanilla SQL Server, not a Fabric Warehouse engine

The sidecar is stock SQL Server 2022, a stand-in for the Fabric Warehouse
engine. dbt-fabric's **table** materialization emits Fabric/Synapse
`CREATE TABLE AS SELECT`, which vanilla SQL Server rejects, so the model here is
a **view** (`CREATE VIEW`, standard T-SQL). This e2e proves the ODBC Driver 18 +
FedAuth + catalog/DML path end to end; it deliberately does not claim Fabric's
MPP T-SQL dialect (see `docs/17-parity.md`).
