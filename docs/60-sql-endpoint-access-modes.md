# 60 — SQL analytics endpoint access modes: user identity and delegated identity

**Status: all four stages built — the mode is switchable with its effects; in
user identity mode a lakehouse's OneLake table, column and row security is synced
into its SQL analytics endpoint and enforced by SQL Server, including for Direct
Lake on SQL.**

**Decision: model both modes the way Fabric's own security sync does — by
translating a lakehouse's OneLake security roles into real SQL Server objects on
its analytics endpoint, so the engine enforces them — and refuse what is not yet
synced rather than serve SQL permissions under the user identity name.**

Grounded against Microsoft Learn as read on 2026-09-15: *OneLake security for
SQL analytics endpoints* and *Get started with OneLake security*.

**Verification, stated plainly: built to Microsoft's documentation and enforced
by a real SQL engine. Not verified against a real Fabric tenant.** No published
spec covers the mode (the vendored `sqlEndpoint` swagger has no access-mode
property), so the sources are Learn prose, a real SQL Server 2022 and, for the
row-filter translation and the endpoint's T-SQL surface, second readers that are
not Fabric either (sqlglot-go's parser, and Microsoft's own surface-area page,
which records what Microsoft says, not what the service does). Only a real
user-identity-mode endpoint's catalog could show what its sync builds, and
several readings below are choices where Learn is silent or says two things:
whether a table `GRANT` is refused or ignored, whether a Contributor in no
filtering role reads unfiltered, what a client sees when a sync fails on a
renamed column, and whether an Admin may add a Viewer to `db_datareader`.

## What Fabric does

| Rule | Source |
|---|---|
| **Delegated identity**: the endpoint "connects to OneLake by using the identity of the workspace or item owner, and security is governed exclusively by SQL permissions" — GRANT/REVOKE, custom roles, RLS, CLS, DDM | SQL endpoint OneLake security |
| **User identity**: "read access is governed entirely by the security rules defined within OneLake"; table GRANT/REVOKE "isn't allowed"; RLS and CLS come from OneLake roles; DDM is not supported; views, procedures and functions still take SQL grants | same |
| "Newly created SQL analytics endpoints start in delegated identity access mode by default. … an Admin or Member must switch it" — in the portal; **no API is documented** | same |
| A switch "makes SQL analytics endpoints temporarily unavailable across the entire workspace" and "cancels all running and queued queries" | same |
| To user identity: "SQL RLS, CLS, and table-level permissions are ignored"; "existing SQL roles are deleted and can't be recovered". To delegated: "SQL roles and security policies become active". Either way: "removes inline metadata objects, including table-valued functions (TVFs) and scalar-valued functions" | same |
| A security sync translates OneLake roles into SQL database roles prefixed `OLS_`; "RLS … configured in user identity mode … enforced for all users, including those in Admin, Member, and Contributor roles" | same |

## What the emulator did

A lakehouse's analytics endpoint reflected its Delta into SQL Server as the
service account and applied SQL permissions only — delegated identity, the
default, without the name. User identity mode did not exist, so OneLake security
never reached T-SQL.

## Staging, and what each stage may claim

| Stage | Build | May claim |
|---|---|---|
| 1 ✅ | The mode per SQL analytics endpoint, delegated by default, switched through an authenticated emulator-native API by Admin or Member; the switch closes the workspace's live sessions, turns off and remembers SQL security policies and drops custom roles on the way in, re-enables the remembered policies on the way out, and drops unbound functions either way. Until stage 2, an endpoint in user identity mode refuses connections, and a neighbour's three-part name cannot reach it | The mode is a real, switchable state with its documented effects |
| 2 ✅ | Security sync on connect: each OneLake role becomes an `OLS_<role>` database role granted SELECT on its tables or column list; memberships travel with each caller into provisioning; below Contributor, table access comes only through those roles; a DDL trigger refuses table GRANT/DENY/REVOKE and security policies; explicit table permissions are set aside on the switch in and restored on the way out. Prerequisite found on the way: the endpoint accepts the SQL objects and security authored on it, still refusing data writes — past comments and later statements in a batch | Tables and columns follow OneLake security |
| 3 ✅ | Row filters: each role's filter validated against OneLake's documented grammar and translated into an inline predicate function exposing the row's columns under their own names; one `OLS_` security policy per table ORs the roles' filters; members only — a Contributor+ in no filtering role reads unfiltered (inferred); rebuilt in one transaction, only when something changed; reflection drops a table's synced policy before recreating it | Rows follow OneLake security |
| **4 ✅** | Direct Lake on SQL (docs/59) over an endpoint in user identity mode reads through the synced endpoint as the caller, and `directLakeOnly` fails every table there | **Direct Lake on SQL agrees with the endpoint** |

### Stage 1 in detail

**The switch is emulator-native** (decided 2026-09-15):
`GET|PUT /v1/workspaces/{ws}/sqlEndpoints/{id}/_emulator/dataAccessMode` with
`{"dataAccessMode": "DelegatedIdentity" | "UserIdentity"}`. It is
bearer-authenticated and gated as the portal is — Viewer reads the mode, Admin or
Member switches it — and not under the unauthenticated `/_emulator/` control
prefix, where anyone could change a workspace's security model. A warehouse is
refused: it "is secured by T-SQL and nothing else". Setting the mode an endpoint
already has is no switch and changes nothing.

**The effects are applied, in Fabric's order.** Every live TDS session to a SQL
item in the workspace is closed first (a new session registry on the TDS front),
so running queries end. Then, in the lakehouse's database, one batch:

- **to user identity** — enabled SQL security policies are turned off and their
  names remembered on the endpoint; custom database roles lose their members and
  are dropped; functions no security policy depends on are dropped;
- **to delegated** — functions no policy depends on are dropped, and the
  remembered policies are turned back on.

The mode is written last, so a switch whose SQL fails leaves the endpoint in the
mode it was in. Without a SQL engine attached, the mode is recorded and there is
nothing else to do.

**A divergence, stated:** a security policy's own predicate function is kept.
Dropping it would destroy the policy that "becomes active" again on the way back,
and Fabric documents both effects without saying how they combine.

*Stage 1 refused connections to an endpoint in user identity mode until stage 2;
that refusal is gone now that the mode is served.*

### Stage 2 in detail

**The endpoint accepts what is authored on it.** The relay treated a lakehouse
endpoint as read-only for every statement that began with a write keyword, so
`GRANT`, `CREATE VIEW` and `CREATE SECURITY POLICY` never reached SQL Server —
delegated identity's "full control using SQL `GRANT`/`REVOKE`" could not be
exercised. A lakehouse connection now uses `isEndpointWrite`: GRANT/REVOKE/DENY
and CREATE/ALTER/DROP of views, functions, procedures, schemas, roles, users and
security policies, and `ALTER TABLE … ALTER COLUMN … ADD|DROP MASKED`, are
forwarded, and SQL Server's permissions decide who may run them; data changes,
other table DDL and EXEC are refused. The batch is tokenized and every statement
judged, since T-SQL needs no semicolons: `GRANT … INSERT …` is refused. Doing so
closed two holes the first-keyword check had on this surface — a write after a
leading `/* comment */`, and a write after a statement it forwards.

**The sync** (`syncOneLakeRoles`) runs on every connection and every Direct Lake
on SQL read of an endpoint in user identity mode, after reflection. For each
OneLake role it keeps an `OLS_<role>` database role holding SELECT on exactly the
tables the role grants — on the permitted columns when it narrows them — and no
other object permission; an `OLS_` role whose OneLake role is gone is emptied and
dropped. It reuses `pkg/onelakesec`, so the tables and columns are the ones every
other OneLake reader computes. Two cases grant nothing, so a restriction is never
served as none: a column list naming a column the table lacks ("denying all
access to the resource" until fixed), and a row filter, until stage 3.

**Memberships** are carried with each caller's grant — `Grant.OneLake` and
`OneLakeRoles` — into the one provisioning path, which creates the database user
first and then joins exactly those `OLS_` roles and leaves the rest. They follow
the caller into a sibling lakehouse reached by three-part name too.

**The rung** below Contributor is CONNECT: "only users with Viewer permissions or
shared read-only access are governed by OneLake security", so a Viewer reads
tables only through their roles. Contributor and above keep their rung.

**T-SQL cannot author table security** in this mode: a database DDL trigger,
`OLS_guard`, rolls back GRANT/DENY/REVOKE on a table, SELECT or CONTROL on a
schema or the database, and `CREATE`/`ALTER SECURITY POLICY`, naming why — for
anyone but the service account the sync runs as. A view's GRANT passes, as
Fabric allows. The switch in records and revokes users' explicit table
permissions ("table-level permissions are ignored"); the switch out drops the
trigger and every `OLS_` role and restores them.

A sync the engine cannot apply fails the connection: a OneLake role name that
cannot become a SQL role name ("role names cannot exceed 124 characters;
otherwise … synchronization fails"), or a hand-made `OLS_` role it cannot drop.

### Stage 3 in detail

**The grammar is Microsoft's.** A OneLake row filter is `SELECT * FROM
[dbo.]<table> WHERE …` with columns compared to static values by `=`, `<>`, `>`,
`>=`, `<`, `<=`, `[NOT] IN (…)`, `IS [NOT] NULL|BLANK`, joined by `AND`/`OR`/`NOT`
with `TRUE`/`FALSE`, up to 1000 characters. `translateRowFilter` tokenizes it with
the repo's T-SQL tokenizer, accepts exactly that, and emits a predicate over
`r.[column]`, comparing text in `Latin1_General_100_CI_AS_KS_WS_SC_UTF8` as
OneLake does. The table must match exactly; a column must exist. A function call,
another table, a subquery or `UNION`-ed SQL is refused. `IS BLANK` is read as NULL
or empty text — undocumented further, so inferred. Several rules filtering one
table in one role, which OneLake consolidates with ` UNION `, become an OR.

**One policy per filtered table.** `OLS_rls_<hash>` filters the table through
`OLS_rlsfn_<hash>(cols)`, which admits a row when the reader is in a role whose
filter holds, in a role granting the table whole, or in no role granting the
table — the last term being how a Contributor or above in no filtering role reads
everything, while one who is a member is filtered, as "RLS … in user identity
mode … enforced for all users" says of role members (decided 2026-09-15:
inferred). A filter outside the grammar reads as `1 = 0` for its role: a
Contributor member sees no rows, and a Viewer member is not granted the table, so
their query errors — "no rows being shown to users, or query errors in the SQL
analytics endpoint".

**Consistency.** The whole sync runs under `XACT_ABORT` in one transaction, so a
reader never sees a policy dropped and not yet recreated; and it runs only when
its content — including the tables' object ids — differs from the last sync,
recorded as the `OLS_sync` database extended property. A security policy holds
its table even without schema binding, so reflection now drops a table's synced
`OLS_rls_` policy before recreating it; the table's grants go with it, so a
reader restricted by the policy has no SELECT until the sync after reflection
restores both — Fabric's "until synchronization completes" window, closed rather
than opened.

### Stage 4 in detail

Direct Lake on SQL reads through `sqlDBAsFor`, which runs the same access
decision and sync as a relayed connection, so a model over an endpoint in user
identity mode returns each caller what OneLake security gives them. And "the SQL
analytics endpoint can be changed to SSO. When this happens, OneLake security
roles are added as SQL granular access control rules … At this point, Direct Lake
on SQL falls back to DirectQuery 100% of the time" — so under `directLakeOnly`
every table over such an endpoint fails, naming the mode, before the catalog is
asked.

## Boundaries

- **Sync timing**: Fabric syncs within "up to 5 minutes"; the emulator syncs a
  lakehouse on each connection to it and each read through it. A sibling reached
  by three-part name uses that lakehouse's last sync.
- **Fixed database roles**: an Admin or Member, who is `db_owner`, can still add
  a Viewer to `db_datareader`; the guard covers permission statements, not role
  membership.
- **RLS and CLS from different roles** for one user: OneLake refuses the
  combination with query errors; the emulator applies both.
- **Masks** stay in force in user identity mode; Fabric says DDM is "not
  supported in OneLake security" without saying what happens to existing ones.
- **EXEC** stays refused on the endpoint, as before.
- **Shortcuts** are partly modelled — see [docs/61](61-sql-endpoint-shortcuts.md).
  Ownership chaining and the security-sync error states are not modelled.
- **The owner's OneLake access** in delegated mode — "the item owner must have
  valid OneLake access, or all queries may fail" — is not modelled: the
  reflection reads as the service.
- **Mirrored items' endpoints** have no SQLEndpoint item here, so no mode.

## What is Fabric's, and what is ours

Only the `OLS_` role prefix is documented ("OneLake security roles are propagated
to the SQL analytics endpoint with the `OLS_` prefix"). Everything else the sync
creates is this repository's design, chosen so SQL Server enforces what OneLake
says, and not a copy of what Fabric's sync service builds: the `OLS_rls_` security
policies and `OLS_rlsfn_` predicate functions, the `OLS_guard` database DDL
trigger, and the `OLS_sync` extended property that skips an unchanged sync. Nothing
here claims a real endpoint's catalog looks like this; only a real endpoint's
catalog could say. (Fabric documents triggers as unsupported; `OLS_guard` is the
engine's own guard, not one a client can author.)

## Documented rules, and the witness for each

| Fabric's statement | Witness |
|---|---|
| "the maximum number of characters in a row-level security rule is 1000" | `TestTranslateRowFilterRefusals` |
| "RLS roles don't support dynamic and multitable queries" | `TestTranslateRowFilterRefusals`, and `TestATranslatedRowFilterIsAStaticPredicateOverOneTable`, which parses the *output* with sqlglot-go and fails on a subquery, a call or a second table |
| "role names cannot exceed 124 characters" | `TestARoleNameLongerThan124CharactersDoesNotSync` |
| "Manual changes to these roles are not supported" | `TestAOneLakeSyncThatFailsRefusesTheConnection` (a hand-made `OLS_` role that cannot be dropped) |
| "If there are no changes to sync, security sync does not override manual changes" | `TestAnUnchangedSyncKeepsAManualChangeAndAChangedOneOverwritesIt`, against a real SQL Server: a table permission authored on an `OLS_` role survives two syncs with nothing to sync, and is gone after one OneLake change. Mutation-checked both ways: with the `OLS_sync` skip removed it fails, and with the role's revoke removed it fails. The manual grant is authored with `OLS_guard` disabled, because this emulator refuses table `GRANT` in T-SQL in this mode where Fabric says only that it "isn't allowed" |
| "Queries with invalid RLS syntax … result in no rows being shown" | `TestUserIdentityModeAppliesOneLakeSecurityOnTheEndpoint` (a Contributor whose role's filter is invalid sees no rows) |
| Read on the item to connect; ReadData to read | `tds_itemaccess_test.go` |
| Which T-SQL an endpoint does not support | `internal/tds/tsqlsurface_test.go` against `third_party/fabric-tsql-surface/` |

The second reader is sqlglot-go's T-SQL parser (`v0.4.0`, whose dialects do not
include Fabric's). It says the predicate is T-SQL of the permitted shape; it does
not say Fabric emits that predicate.

Not modelled, and why: the security-sync error state for a renamed or deleted
column. Fabric documents the messages ("Row-level security policy references a
column that no longer exists") and that the database "enters error state", but not
what a client sees, so the emulator fails closed — a table with a column-level
allow-list naming a missing column grants nothing, and a row filter that no longer
matches grants no rows — without inventing the error.

## Witnesses

Stage 1: `internal/server/dataaccessmode_test.go` switches a real SQL Server
endpoint over HTTP with real Entra tokens: a Contributor is refused; the Admin's
switch ends a session to the lakehouse and one to the warehouse beside it, drops
a custom role and an unbound function, keeps the policy's predicate and turns the
policy off; connecting is then refused by name and a three-part name from the
warehouse fails; switching back re-enables the policy and the endpoint serves;
an endpoint with no policy switches both ways. Each of seven effects disabled on
its own fails it. `internal/api/dataaccessmode_test.go` covers the API's gate,
refusals, no-op switch, failure and no-engine paths;
`internal/server/dataaccessmode_internal_test.go` the switch's failure paths and
the access refusal without an engine; `internal/tds/sessions_test.go` the session
registry closing only the named databases; `internal/store/sqlendpoint_test.go`
the stored mode.

Stage 2: `internal/server/onelakesync_test.go` against a real SQL Server over the
relay: an owner authors a view and a grant on the endpoint while four disguised
writes are refused and the table is untouched; under delegated identity two
Viewers read every table; after the switch, a Viewer reads only her role's two
columns of one table, another only his table, a Viewer in no role and in a
row-filtered role nothing, a Viewer whose role names a missing column nothing on
that table, and a Contributor everything despite a DENY set aside; the same by
three-part name from the warehouse; a table GRANT is refused naming OneLake
security; a role moving from one Viewer to another moves access on the next
connection, and a removed role is gone; back under delegated identity the `OLS_`
roles are gone, Viewers read everything and the DENY is enforced again.
`TestAOneLakeSyncThatFailsRefusesTheConnection` covers an unsyncable role name
and a hand-made `OLS_` role. Each of eight mutations — no sync, Viewers keeping
their reader rung, memberships not synced, columns ignored, no guard, permissions
not set aside, permissions not restored, stale memberships kept — fails it.
`internal/tds/writeguard_test.go` covers the endpoint guard's statements,
comments and batches.

Stage 3: in the same witness, a Viewer and a Contributor in a role filtering hr to
`'ADA'` see the one `ada` row (case-insensitively), a Contributor in a role
whose filter calls a function sees none, a Contributor in no role and a Viewer
granted hr whole see both, and a Viewer whose role has two filtering rules on hr
and a `WHERE TRUE` on sales sees both rules' rows. `TestARowFilterSurvivesReflection`
reflects a real Delta table under a row filter, reconnects without the policy
being rebuilt, commits new Delta so reflection recreates the table, and still
sees only the filtered rows; switching back leaves no `OLS_` object.
`internal/server/rowfilter_test.go` covers the grammar and every refusal. Each of
seven mutations — no policy, no in-no-role term, no case-insensitive collation,
an invalid filter read as none, reflection keeping the policy, rebuilding every
sync, delegated keeping the policies — fails a witness.

Stage 4: `TestDirectLakeOnSQLOverAUserIdentityEndpoint` runs `executeQueries`
over a model bound to a user identity endpoint: a Viewer in a role filtered to
`west` gets only west, a Viewer in no role is refused by the endpoint, the owner
gets every row, and the same model under `directLakeOnly` fails naming the mode.
Without the sync on that read, or without the mode as a fallback cause, it fails.
