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
synced into the endpoint as `OLS_` database roles: a Viewer reads only the tables
and columns their roles grant, and T-SQL cannot grant tables around them.

The endpoint also accepts the SQL objects and security authored on it — views,
functions, roles, grants, security policies, masks — which the relay refused as
writes before; data writes stay refused, now including one after a leading
block comment or after another statement in the batch.
[docs/60](../60-sql-endpoint-access-modes.md)
