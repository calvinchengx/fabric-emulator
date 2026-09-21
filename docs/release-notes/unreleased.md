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

A OneLake shortcut under `Tables/` now reads as a table on a lakehouse's SQL analytics endpoint, from its target: it was not reflected at all. In user identity mode the **source's** security applies as Fabric documents: a caller the source refuses is refused, and the source's column allow-list and row filters narrow the read on top of the consumer's own roles, the intersection winning. Not built: delegated mode does not yet block a shortcut whose source is secured, ownership chaining is not modelled, and ADLS, S3 and Dataverse shortcuts are still not tables. Where a row filter reads a column that another reader is denied, that reader is refused the table, because SQL Server needs SELECT on a policy's predicate columns; this also affects a table with no shortcut when one role narrows columns and another filters rows. [docs/61](../61-sql-endpoint-shortcuts.md)
