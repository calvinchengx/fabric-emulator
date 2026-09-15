# 60 — SQL analytics endpoint access modes: user identity and delegated identity

**Status: stage 1 built — the mode is switchable and its side effects applied;
user identity mode is refused by name until OneLake security is synced in.**
Stages 2–4 follow.

**Decision: model both modes the way Fabric's own security sync does — by
translating a lakehouse's OneLake security roles into real SQL Server objects on
its analytics endpoint, so the engine enforces them — and refuse what is not yet
synced rather than serve SQL permissions under the user identity name.**

Grounded against Microsoft Learn as read on 2026-09-15: *OneLake security for
SQL analytics endpoints* and *Get started with OneLake security*.

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
| **1 ✅** | The mode per SQL analytics endpoint, delegated by default, switched through an authenticated emulator-native API by Admin or Member; the switch closes the workspace's live sessions, turns off and remembers SQL security policies and drops custom roles on the way in, re-enables the remembered policies on the way out, and drops unbound functions either way. Until stage 2, an endpoint in user identity mode refuses connections, and a neighbour's three-part name cannot reach it | **The mode is a real, switchable state with its documented effects** |
| 2 | Security sync on connect: each OneLake role becomes an `OLS_<role>` database role whose members are the principals it names holding Read on the lakehouse, granted SELECT on its tables or column list; Viewer-level table access only through those roles; a DDL trigger refuses table GRANT/DENY/REVOKE and `CREATE SECURITY POLICY` | Tables and columns follow OneLake security |
| 3 | Row filters: each role's filter SQL wrapped in an inline function exposing the row's columns under their own names; one `OLS_` security policy per table ORs the roles' filters; members only — a Contributor+ in no filtering role reads unfiltered (inferred) | Rows follow OneLake security |
| 4 | Knock-on effects and grading: under Direct Lake on SQL (docs/59), `directLakeOnly` sees the synced policy as a fallback cause | End-to-end witnesses |

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

**Refused until stage 2.** `sqlAccess`, the one decision behind a relayed
connection and a Direct Lake on SQL read, refuses a lakehouse in user identity
mode by name; `workspaceGrants` gives it `RoleNone` as a sibling, so its CONNECT
is revoked and a three-part name from a warehouse cannot reach it.

## Boundaries

- **Sync timing**: Fabric syncs within "up to 5 minutes"; the emulator will sync
  on connect.
- **Shortcuts**, ownership chaining, and the security-sync error states are not
  modelled.
- **The owner's OneLake access** in delegated mode — "the item owner must have
  valid OneLake access, or all queries may fail" — is not modelled: the
  reflection reads as the service.
- **Mirrored items' endpoints** have no SQLEndpoint item here, so no mode.

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
