# 55 — T-SQL security: RLS, CLS and dynamic data masking

**Decision: let SQL Server enforce it, and connect as the caller.** The
warehouse already relays to a real SQL Server, which implements all three
features natively. What is missing is not the enforcement — it is that every
client's SQL currently runs as the relay's own DSN account, so there is nobody
for a policy to restrict.

Grounded against `fabric-docs@0d63906a`.

## This is a different mechanism from OneLake security

Easy to conflate, and expensive to conflate, so it is stated first.

| | OneLake security ([54](54-onelake-security.md)) | T-SQL security (this doc) |
|---|---|---|
| Defined by | `dataAccessRoles` on the item | `CREATE SECURITY POLICY`, `GRANT`, `MASKED WITH` |
| Applies to | Lakehouse, Mirrored DB, Databricks Mirrored Catalog | **Warehouse and SQL analytics endpoint** |
| Enforced by | every engine — Spark, SQL endpoint, Direct Lake | the T-SQL engine only |
| Masking | **not supported** | the only place it exists |

The docs are explicit that RLS and CLS "only apply to queries on a Warehouse or
SQL analytics endpoint", and OneLake security's supported-items table has no
Warehouse in it. **A warehouse is secured by this and nothing else**, which is
why doc 54's five stages left a hole the parity map now names.

Dynamic data masking is the sharpest case: the SQL-endpoint mode comparison
lists it as "Not supported in OneLake security", so unlike RLS and CLS it has no
cross-engine counterpart at all.

## Why the emulator does not implement the features

SQL Server 2022 — the sidecar this repo already runs — implements
`CREATE SECURITY POLICY` with inline table-valued predicates, `GRANT SELECT ON
t(col)`, and `ALTER TABLE … ADD MASKED WITH`. Reimplementing them in
`internal/tsql` would be building a worse version of something already in the
process, and the repo's bet is that a real engine should do the engine's job.

So this increment adds **no policy engine**. It adds an identity.

## What is actually missing: the caller

`internal/tds/sqlserver.go` splices: each client connection gets its own backend
connection, and `Dial` logs that connection in with the DSN's own account —
`clientLogin(conn, b.base.User, b.base.Password, …)`. Every client's T-SQL then
runs as that one account, which is `sa` in the default compose.

That is why the features would appear not to work even after someone wrote a
policy. Nothing is broken; there is simply no principal:

- RLS predicates key on the connected identity — `USER_NAME()`,
  `SESSION_CONTEXT`, `SUSER_SNAME()`. With one identity for everyone there is
  nothing for the predicate to distinguish, so it either filters every caller
  identically or none of them. (Note that a sysadmin is **not** exempt from a
  filter predicate — measured; the problem is sameness, not bypass.)
- `GRANT`/`DENY SELECT ON t(col)` restricts a grantee, and sysadmin is not a
  grantee anything can be denied to
- masking is bypassed by anyone holding `UNMASK`, which sysadmin does

**Connect as the caller and all three start working at once**, because SQL
Server has been ready the whole time.

## Shape

The splice is what makes this tractable. A pooled connection would force
per-statement `EXECUTE AS … REVERT`, which is unsafe on a pool — an errored
statement can leave the connection impersonating someone, and the next borrower
inherits it. One client, one backend session, means the identity can be set once
at login and never touched again.

1. **Provision.** After `OnConnect` resolves the principal and the target
   database, ensure a database principal exists for it, and grant it the rights
   its workspace role implies: a Viewer gets `SELECT`, Contributor and above get
   read-write. Idempotent, and keyed on the Entra object id rather than a
   display name, which can change.
2. **Log in as it.** `Dial` takes the principal and authenticates the spliced
   connection as that database principal instead of the DSN account.
3. **Stop there.** No interception of `CREATE SECURITY POLICY` or `MASKED WITH`
   — those flow through the relay untouched and SQL Server applies them.

## Staging, and what each may claim

| Stage | Build | May claim |
|---|---|---|
| 1 ✅ | per-principal database users, provisioned on connect | nothing; no visible behaviour change |
| 2 ✅ | the spliced session authenticates as the caller | callers are distinguishable; `USER_NAME()` differs |
| 3 ✅ | RLS witnessed | `CREATE SECURITY POLICY` filters by caller |
| 4 ✅ | CLS witnessed | a denied column errors for one caller and not another |
| 5 ✅ | DDM witnessed | a masked column reads masked, and `UNMASK` reveals it |

## Witnesses

`ci:warehouse-tds` already drives the surface with a real ODBC/TDS client, so
each stage extends it rather than adding a suite. The load-bearing shape is the
same as doc 54's: **two callers, one query, different answers**. A single
restricted caller proves nothing — a policy that denied everyone would pass it.

Every stage needs the unrestricted caller asserted in the same run.

## Boundaries

- **Not Entra-backed principals.** Real Fabric maps Entra identities to
  database principals through its own control plane; we create contained
  database users named for the object id. The observable contract — different
  callers, different results — matches; the provisioning does not.
- **The sysadmin path stays.** Internal work (reflection, mirroring, catalog
  maintenance) keeps using the DSN account through the pooled `Query` path.
  Those are the emulator acting as the service, not as a user.
- **A grantee must have connected once.** Provisioning happens on connect, so
  an owner writing `DENY … TO [someone]` before that someone has ever opened a
  session gets "Cannot find the user". Real Fabric materialises the principal
  when workspace access is granted; we do it lazily. Measured — it is the second
  thing the e2e caught — and cheap to fix later by provisioning every workspace
  principal on any connect, at the cost of a query per login.
- **Lakehouse SQL analytics endpoint.** The same T-SQL features apply there in
  the product, and its OneLake-security interaction is mode-dependent (user
  identity vs delegated identity). This increment covers the Warehouse; the
  endpoint's dual-mode behaviour is its own question.
