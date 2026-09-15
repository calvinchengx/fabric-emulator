# 59 — Direct Lake on SQL: `Sql.Database` over a SQL analytics endpoint

**Status: all four stages built — a Direct Lake on SQL model is served through
the SQL analytics endpoint as the caller, and `directLakeBehavior` decides
whether what would fall back to DirectQuery fails.**

**Decision: serve a Direct Lake on SQL table by reading through the SQL endpoint
as the calling identity, so SELECT, column security, row security and masking
stay SQL Server's decisions; recognise and refuse by name everything that path
cannot answer.**

Grounded against Microsoft Learn as read on 2026-09-15: *Direct Lake overview*,
*How Direct Lake works* and *Integrate Direct Lake security*.

## What Fabric does

| Rule | Source |
|---|---|
| Direct Lake has two flavours, told apart by the shared expression: "*AzureStorage.DataLake* for Direct Lake on OneLake and *Sql.Database* for Direct Lake on SQL analytics endpoints" | Security integration |
| On SQL, a model "can use the data from a single Fabric data source" — "only lakehouse or warehouse tables (or views)" — and cannot mix Direct Lake with DirectQuery or Dual tables | Overview |
| The connection expression "must refer to the SQL analytics endpoint by GUID, not by friendly name, to use **Edit tables** and **refresh**" — so a name is accepted, but only for queries | Overview |
| The effective identity (the querying user, by default) needs "*read* access to the Fabric item … and SELECT permission on a table through its SQL analytics endpoint", and no OneLake file access | Security integration |
| Per query: semantic-model OLS → error; endpoint CLS or a denied table → error; endpoint RLS, DDM, OLS, or a view → **DirectQuery fallback**; otherwise in memory | Security integration; How it works |
| `DirectLakeBehavior`: `Automatic` (default) falls back silently, `DirectLakeOnly` fails, `DirectQueryOnly` always queries the endpoint. It "only applies to Direct Lake on SQL analytics endpoints" | How it works |

## What the emulator did

The expression parser matched only a OneLake URL. A `Sql.Database` model failed
with *shared expression must contain an onelake.dfs.fabric.microsoft.com
workspace/lakehouse URL*: a refusal, but one that blamed the model for not being
the other flavour.

## Staging, and what each stage may claim

| Stage | Build | May claim |
|---|---|---|
| 1 ✅ | Every shared expression classified: OneLake URL, `Sql.Database` with two text arguments, or neither — each named. `directLakeBehavior` parsed from TMSL and TMDL. Refused: both flavours in one model, more than one SQL source, `Sql.Database` options, and — until stage 2 — the SQL flavour itself | **A SQL-flavour model is named as one** |
| 2 ✅ | The database argument resolves to a SQL analytics endpoint or warehouse by GUID, or by display name in the model's workspace; Read on the source required; no SQL engine refused by name | Source resolution and item permission |
| 3 ✅ | Rows read through the SQL endpoint **as the caller's own login**, after the relay's membership sync, selecting the model's columns by name | SELECT and column security are the engine's |
| **4 ✅** | Endpoint RLS, masking and views detected per table: `directLakeOnly` errors naming the cause; `automatic` and `directQueryOnly` serve what the engine returns the caller | Fallback semantics |

### Stage 1 in detail

`semanticmodel.ParseDirectLakeSource` reads the expression without evaluating
it — Fabric does not use the M function to read either — so the query loader,
the datasources endpoint and lineage all classify a model the same way.
`Sql.Database` is matched case-sensitively, as M names are, and `Sql.Databases`
does not match. Its arguments must be text literals: a variable would need M
evaluated, and an options record changes connector behaviour nothing here
models.

The **server argument is not checked** (decided 2026-09-15). The emulator's SQL
address depends on the host a caller used, so no tenant host could match it;
the database argument alone decides, and a model exported from a tenant loads
unchanged. A stated divergence.

### Stage 2 in detail

`resolveDirectLakeSQLSource` looks the database argument up by id anywhere, else
by display name among the SQL analytics endpoints and warehouses of the model's
own workspace — which workspace a name means is not documented, and the model's
is the inference. A name both kinds carry is refused as ambiguous rather than
picked. An endpoint resolves to the lakehouse it serves (its
`parentLakehouseItemId`), because the lakehouse is what a share grants Read on.

**Read is decided before anything about the item is described.** A caller
without it — including for a GUID that names nothing — gets one message, *caller
cannot read the source*, so a stranger learns neither whether an id exists nor
what it is. A reader is told what is wrong with what they named: a lakehouse's
own id instead of its endpoint's (a mistake the two ids invite), or an item that
is no SQL source. With no SQL engine attached the source is refused by name, as
Direct Lake over a warehouse already is. What still follows, for everyone who
passes, is the stage 1 refusal: reading is stage 3.

### Stage 3 in detail

The read goes through `API.SQLDBAs`, which the server wires to `sqlDBAsFor`: the
item's database, logged into **as the caller**. It runs the same decision a
relayed TDS connection runs — `sqlAccess`, shared with the router, so Read on the
item and every rung the workspace gives the caller come from one place — then
prepares the database, reflects a lakehouse's Delta into its analytics endpoint,
and asks the backend's `DBAs` for a login. `DBAs` provisions through the same
`provision` the relay's splice now calls, so a caller holds identical rights
whichever door they use, and a failure refuses rather than falling back to the
service account. The handle is closed after each read: a pooled login kept open
would outlive a revoke.

Each Direct Lake table is read with `SELECT [col], … FROM [schema].[entity]`,
naming the model's source columns, so a column the endpoint denies the caller
fails the read (SQL Server's error 230) as a query touching it fails in Fabric.
Names are bracket-quoted with `]` doubled; a name no Fabric object can carry is
refused.

**What that makes true, and why it is the product's answer.** SQL Server applies
the caller's SELECT grants, column denials, row-level security predicates and
masks. Where the endpoint enforces none of those, the rows equal the Delta that
Direct Lake would load. Where it enforces RLS or masking, Fabric falls back to
DirectQuery — which is exactly a query to the endpoint as the caller — under the
default `automatic` behaviour. What stage 3 does not do is fail when fallback is
disabled: that is stage 4.

### Stage 4 in detail

Under `directLakeOnly`, after the caller's read succeeds — so a denied column or
table has already failed first, in Fabric's order — each table's endpoint catalog
is asked what would send it to DirectQuery: an enabled security policy predicate
on it, a masked column, or its being a view. Any of those fails the query, naming
the cause. `automatic` (the default) and `directQueryOnly` never ask: both return
what the endpoint gives the caller, which is the DirectQuery answer.

The catalog is read through the **service connection**, not the caller's. A
caller a policy restricts may lack the right to see the policy, and "no policy"
read from their view of the catalog would serve a table the author asked to
fail. Only the catalog is read that way; rows are always the caller's. The error
text is the emulator's own — Microsoft documents that the query "fails with an
error" but not the message.

Guardrails and framing, the other documented fallback causes, are not modelled,
so they cannot trigger it; `TMSCHEMA_DELTA_TABLE_METADATA_STORAGES` still
refuses `FallbackReason`, and `TABLETRAITS()` is not implemented.

## Boundaries

- **Fixed identity**: cloud connections are not modelled; the effective identity
  is always the caller, Fabric's default.
- **The whole table is read**: the emulator loads every model column of a Direct
  Lake table per query, so a column the endpoint denies fails every query over
  that table, where Fabric fails only queries touching it — a refusal in excess,
  never a leak.
- **Framing**: Direct Lake serves the Delta as of the last refresh; reading the
  endpoint serves its current rows.
- **The owner's framing check** and **capacity guardrails** are not modelled.
- `TMSCHEMA_DELTA_TABLE_METADATA_STORAGES` keeps refusing `FallbackReason`.

## Witnesses

Stage 1: `internal/semanticmodel/directlakesource_test.go` classifies Fabric's
own expression shape, a name with a doubled quote, and OneLake URLs, and names
every refusal; parses `directLakeBehavior` from both formats and refuses an
unknown value. `internal/api/directlake_sql_test.go` refuses, over
`executeQueries`, a SQL model by name, two SQL sources, mixed flavours, an
expression of neither flavour and a missing expression — never with the old
OneLake-URL message — and shows the datasources endpoint and lineage refusing
to report a SQL model as a OneLake source.

Stage 2: `internal/api/directlake_sql_test.go` resolves an endpoint and a
warehouse by GUID and by name, and refuses a lakehouse id, a notebook, a name
nothing carries and an ambiguous name; refuses a stranger identically for an
endpoint, a lakehouse and a notebook, admits them with Read on the lakehouse and
refuses them again on revoke, beside a Viewer who holds Read by role; refuses
each source type with no engine attached; and fails closed on an orphaned
endpoint, unreadable access, unreadable endpoint properties and an unknown
model. With the Read check, the lakehouse-id refusal, the ambiguity refusal or
the engine check disabled, a witness fails.

Stage 3, against a real SQL Server (gated on `WAREHOUSE_MSSQL_DSN`):
`internal/server/directlake_sql_test.go` runs `executeQueries` with real Entra
tokens over a warehouse whose endpoint carries a security policy and a column
DENY — two callers get their own row from one model while the owner gets both,
and the denied caller's model over the column fails while the other's is served
— and shows a principal shared the warehouse for Read alone refused by the
endpoint until the owner grants SELECT, then refused at the source once Read is
revoked. With the hook reading as the service account, or selecting `*` instead
of the model's columns, those witnesses fail. `TestSQLDBAsFor` covers the hook's
lakehouse reflection, rungs and refusals; `TestDBAsRefusesWhatItCannotLogInAs`
covers the backend refusing rather than falling back. Without an engine,
`internal/api/directlake_sql_test.go` reads positionally over SQLite for the
caller the hook was opened for, in a non-dbo schema, by column name, beside an
import table, closing each handle, and names every read refusal.

Stage 4: `TestDirectLakeOnlyFailsWhereTheEndpointWouldFallBack` builds a
security policy, a masked column, a view and a plain table on a real SQL Server;
under `directLakeOnly` the first three fail naming their cause and the plain
table is served, while under `automatic` all four are served as the caller —
the policy's row only, the mask applied. `TestDirectLakeOnlyRefusesWhatWouldFallBack`
shows `automatic` and `directQueryOnly` never consulting the catalog, the
catalog read through the warehouse's and the lakehouse's service connections,
and a catalog or connection failure failing the query. Dropping any one cause,
refusing under `directQueryOnly`, or reading the catalog as the caller fails a
witness.
