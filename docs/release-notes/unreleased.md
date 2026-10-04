# Unreleased (after v0.40.0)

Draft of what landed on `main` after the `v0.40.0` tag. Rename this file to
`v0.41.0.md` (or whichever minor) when tagging. Open pull requests are not
here.



## A WITH clause no longer hides a write from Warehouse versioning

`WITH c AS (…) INSERT/UPDATE/DELETE/MERGE …` was read as a query, so a Warehouse
table changed that way got no time-travel version, and an `INSERT` or `MERGE`
was recorded in lineage as `SELECT INTO`. The clause is now read through to the
verb after it; a write through a CTE (`WITH c AS (SELECT … FROM dbo.t) DELETE
FROM c`) is attributed to the base table. [docs/35](../35-warehouse-time-travel.md)

## Direct Lake applies OneLake row filters

A Direct Lake on OneLake query over a table a OneLake security role filters by
rows used to be refused, because nothing evaluated the predicate. It is now
applied: the caller gets the rows the filter admits. The filter is parsed by the
same code the SQL analytics endpoint renders into SQL Server predicates, and a
witness against a real SQL Server requires the two to return the same rows —
text compared case-insensitively, NULL comparisons admitting nothing. A filter
the emulator cannot apply blocks the table by name rather than serving it
unfiltered. [docs/54](../54-onelake-security.md)

## Row and column security from different roles no longer grants everything

A principal in one OneLake role that filters a table's rows and another that
narrows its columns read the whole table: consolidating the roles by union
opened both restrictions. Fabric does not support that combination and gives a
query error. Direct Lake and direct OneLake reads now give that error, by name;
`principalAccess` gives an engine both restrictions; the SQL analytics endpoint
shows such a reader no rows. [docs/54](../54-onelake-security.md)
## Fabric IQ MCP

The emulator now serves Microsoft's Fabric IQ MCP server at
`POST /v1/mcp/fabriciq`: the six read-only tools an agent uses to find a Power BI
report or semantic model, read its pages, visuals, filters and schema, look up an
exact stored value, and run DAX. Each tool runs as the signed-in user and needs
only Read on the item, and the model's row- and object-level security decide what
they see. Service-principal tokens are refused, as Microsoft documents. Report
definitions in PBIR and PBIR-Legacy form are now read for this, and the DAX
evaluator gained `ORDER BY`. Microsoft does not publish the tools' schemas; the
argument names and response shapes follow its own Fabric IQ skill.
[docs/07](../07-control-plane-api.md#fabric-iq-mcp)

## The REST surface ledgers are explained

Three evidence files CI rewrites on every run — `docs/surface-ledger.json`,
`docs/route-coverage.json` and `docs/undocumented-routes.json` — were named by
no prose page, so their numbers were reviewable only by reading the checker that
wrote them. A new chapter states what each one counts, what its denominator is,
how it ratchets, the exact command that regenerates it, and which gates need a
recording and so cannot answer before a push. It also reconciles the two
operation denominators in the tree: the parity map's ~880 from Fabric's
published reference against the ledger's 1002 from the vendored swagger, which
differ because the ledger also counts Power BI's surface and can only count
specs committed here.

Writing it found two ledgers publishing counts no gate compares.
`docs/undocumented-routes.json` records 613 registered routes where the tree now
has 1059 — one honest snapshot that aged, and its six-route invariant still
holds. `docs/route-coverage.json` is the sharper case: its `exercised`,
`registered` and `notYetExercised` cannot all be true, since its own writer
defines `exercised` as `registered` minus the listed routes, which is 113 and
not the 116 recorded. The gate reads only the route names, so the file has been
internally inconsistent and green at once. Both are documented rather than
repaired — regenerating a reviewed artifact belongs in a change that can show
the diff. [docs/65](../65-api-surface-coverage.md)

## Fabric Data Warehouse MCP

The emulator now serves Microsoft's Data Warehouse MCP server at both of its
endpoints: `POST /v1/mcp/dataPlane/sqlEndpoint`, and one scoped to a single
item. Its one tool, `execute_query(workspaceId, itemId, query)`, runs a T-SQL
batch on a Warehouse or a lakehouse's SQL analytics endpoint as the signed-in
user, and returns the last result set as CSV. It takes the same path a TDS
client's batch does, so a Viewer and the endpoint's data are read-only, Fabric's
dialect applies, and SQL Server enforces the caller's grants and row-level
security. A write it runs is recorded for lineage as one sent over TDS is.
The tool is `execute_query`, as the live server lists it; the name on the Learn
page, `executeSQL`, works too. Its result is CSV embedded as a `text/csv`
resource, then a row count. A read-only Warehouse session's
refusal now says that, instead of naming the lakehouse endpoint.
[docs/07](../07-control-plane-api.md#fabric-data-warehouse-mcp)

## e2e harnesses no longer leave entra-emulator running

On a machine where a version manager such as goenv puts `entra-emulator` on
PATH, every e2e harness that starts it left it running after the run, holding
its port, and the next run refused to start. PATH resolved to a shell-script
shim, which runs the real binary as a child process, so the harness's
shutdown stopped the shim and not the emulator. `e2e/entra_install.py` now
uses PATH only when it finds the binary itself. For a wrapper script it takes
the binary `go install` put in GOBIN, and if there is none it installs its own
copy. The fifteen copies of the busy-port check are now one, `e2e/port_guard.py`,
and when a port is taken its message names the process holding it and says
whether that process outlived the run that started it.

## Test cases can be kept as data

A suite whose cases are a table can now keep them in `cases/<suite>.json`, read
by every runner that executes them: a pytest test marked `cases("<suite>")` is
parametrized over the file, and an e2e runner reads it through the
standard-library `scripts/casefiles.py`. `make check` refuses a case without a
unique kebab-case `id` or a `why`. The spark agent's consumer contract is the
first suite: the unit test and `e2e/agent-contract` now read the same twelve
cases, where the e2e runner used to retype the statements it executed. Fabric
IQ MCP is the second: its Go test and its e2e driver, which calls the tools
through the unmodified `mcp` SDK as entra-emulator's seeded users, now run the
same nineteen tool calls from `cases/fabric-iq-tool-calls.json`. Each case's
expected answer is a short list of paths into the tool's JSON reply, read the
same way in Go and Python.
[docs/10](../10-testing.md#test-cases-as-data)

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

## Copy activity: `fileListPath` (list-of-files copy)

A Copy activity source naming `fileListPath` now copies exactly the files that
text file lists — one relative path per line, each "the relative path to the
path configured in the dataset" (ADF's own wording) — instead of failing
loudly as every other unimplemented Copy option still does. A file present
under the source folder but left off the list is not copied; a listed file
that does not exist fails the activity rather than silently copying fewer
files than the list promised. The list file itself is addressed the same way
`folderPath`/`fileName` are. `fileListPath` on a **sink** is still refused by
name, since Fabric's schema carries it only on `*ReadSettings`. A list entry
that climbs out of the source path (`../x`, `/../x`) fails the activity and writes
nothing.
[docs/parity.md](../parity.md)

## Strict mode refuses two more of Fabric's unsupported T-SQL

`-tsql-strict` (`FABRIC_TSQL_STRICT`, off by default) now also refuses `FOR JSON` inside a subquery, derived table, CTE or call — Fabric allows it only as the last operator — and a `/` or `\` in the name of a schema or table being created. Of the T-SQL Microsoft lists as unsupported, strict mode now refuses 12 of 16, and together with the endpoint's write guard 14; the vector type is the one neither refuses, because the SQL Server 2022 sidecar has no such type. Nothing changes without the flag. [docs/29](../29-tsql-parity.md)

## A OneLake security role naming a missing column fails the sync loudly

A row filter or column allow-list that no longer matches its table's schema used to silently narrow that role's grant to nothing — a Viewer in the role simply read less than intended. Fabric documents a louder reaction: "Row-level security policy references a column that no longer exists. Database enters error state until policy is fixed." (the same sentence for column-level security). The sync now fails with that message when this happens, for a role's own constraint and for a OneLake shortcut source's role alike, so every read through the endpoint fails until the role is fixed at its source — not only the affected role's. An RLS filter that is merely invalid syntax (Fabric's separate, documented "no rows being shown" case) still narrows to nothing rather than failing the sync; the two are now kept distinct. [docs/60](../60-sql-endpoint-access-modes.md)
