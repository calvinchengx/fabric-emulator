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
requires. User identity mode is refused by name until OneLake security is synced
into the endpoint. [docs/60](../60-sql-endpoint-access-modes.md)
