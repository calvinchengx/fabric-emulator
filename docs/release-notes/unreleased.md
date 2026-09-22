# Unreleased (after v0.40.0)

Draft of what landed on `main` after the `v0.40.0` tag. Rename this file to
`v0.41.0.md` (or whichever minor) when tagging. Open pull requests are not
here.

## SQL analytics endpoint access modes can be switched

A lakehouse's SQL analytics endpoint now has a data access mode, delegated
identity by default — what it always did. An Admin or Member switches it through
the emulator-native `…/sqlEndpoints/{id}/_emulator/dataAccessMode` (Fabric offers
no API), and the switch applies Fabric's documented effects: the workspace's SQL
sessions end, and SQL roles, security policies and functions change as the mode
requires. In user identity mode, the lakehouse's OneLake security roles are
synced into the endpoint as `OLS_` database roles and row policies: a Viewer
reads only the tables, columns and rows their roles grant, a role member of any
workspace role is held to its row filter, and T-SQL cannot grant tables around
them. Direct Lake on SQL over such an endpoint returns each caller the same
rows, and `directLakeOnly` fails there, as Fabric's always falls back.

The endpoint also accepts the SQL objects and security authored on it — views,
functions, roles, grants, security policies, masks — which the relay refused as
writes before; data writes stay refused, now including one after a leading
block comment or after another statement in the batch.
[docs/60](../60-sql-endpoint-access-modes.md)

## Shortcuts on the SQL analytics endpoint

A OneLake, ADLS Gen2, Amazon S3 or Dataverse shortcut under `Tables/` now reads as a table on a lakehouse's SQL analytics endpoint, from its target: it was not reflected at all. For a OneLake source, in user identity mode the source's security applies as Fabric documents — a caller the source refuses is refused, and its column allow-list and row filters narrow the read on top of the consumer's own roles, the intersection winning — and in delegated mode a shortcut whose source has row or column security is blocked for every reader, as a view that refuses with the reason. An external target carries no OneLake security, so neither applies to it, correctly. Not built: ownership chaining. Where a row filter reads a column that another reader is denied, that reader is refused the table, because SQL Server needs SELECT on a policy's predicate columns; this also affects a table with no shortcut when one role narrows columns and another filters rows. [docs/61](../61-sql-endpoint-shortcuts.md)

## Delegated mode blocks a shortcut whose source is secured

On a SQL analytics endpoint in delegated identity mode, a shortcut whose source table has row-level or column-level security is now **blocked**, as Fabric documents: the endpoint reads OneLake as the item owner, who can only read a table whole. Every reader is refused, an Admin or Member included, with a message naming the reason; it is reflected as a view that refuses every read. Switching the endpoint to user identity mode lifts the block, where the caller's own identity is checked at the source instead. Fabric documents that access is blocked but not what a client sees, so the message is ours. [docs/61](../61-sql-endpoint-shortcuts.md)

## Strict mode refuses two more of Fabric's unsupported T-SQL

`-tsql-strict` (`FABRIC_TSQL_STRICT`, off by default) now also refuses `FOR JSON` inside a subquery, derived table, CTE or call — Fabric allows it only as the last operator — and a `/` or `\` in the name of a schema or table being created. Of the T-SQL Microsoft lists as unsupported, strict mode now refuses 12 of 16, and together with the endpoint's write guard 14; the vector type is the one neither refuses, because the SQL Server 2022 sidecar has no such type. Nothing changes without the flag. [docs/29](../29-tsql-parity.md)

## A OneLake security role naming a missing column fails the sync loudly

A row filter or column allow-list that no longer matches its table's schema used to silently narrow that role's grant to nothing — a Viewer in the role simply read less than intended. Fabric documents a louder reaction: "Row-level security policy references a column that no longer exists. Database enters error state until policy is fixed." (the same sentence for column-level security). The sync now fails with that message when this happens, for a role's own constraint and for a OneLake shortcut source's role alike, so every read through the endpoint fails until the role is fixed at its source — not only the affected role's. An RLS filter that is merely invalid syntax (Fabric's separate, documented "no rows being shown" case) still narrows to nothing rather than failing the sync; the two are now kept distinct. [docs/60](../60-sql-endpoint-access-modes.md)
