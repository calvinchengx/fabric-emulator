# 61 — Shortcuts on the SQL analytics endpoint

**Status: four of Fabric's documented shortcut behaviours are built — a OneLake
shortcut under `Tables/` reads as a table on the endpoint; in user identity mode a
caller needs access at the source as well as at the consumer, and what the
source's roles narrow narrows the read too; and in delegated mode a shortcut whose
source has row or column security is blocked. The rest are listed below, not
implied.**

Built to Microsoft's documentation and enforced by a real SQL engine. Not
verified against a real Fabric tenant (see the verification note in
[docs/60](60-sql-endpoint-access-modes.md)).

Grounded against Microsoft Learn as read on 2026-09-21: *OneLake security for SQL
analytics endpoints*, the sections *Shortcuts behavior with security sync* and
*Shortcuts behavior in delegated mode*.

## What Fabric says, and what this does

| Fabric's statement | Here | Witness |
|---|---|---|
| "Shortcuts function as tables in the SQL analytics endpoint" | **Built** for OneLake targets. A shortcut under `Tables/` is reflected under its own name, read from its target's Delta log, and follows the source when it changes | `TestAShortcutUnderTablesReadsAsATableOnTheEndpoint`, `TestTableSourcesListShortcuts` |
| "Users must have valid access on **both** the shortcut source … **and** the destination … If the user lacks permission on either side, queries fail with an access error" | **Built, user identity mode.** The destination is the lakehouse's own roles and grant; the source is asked of the same decision every OneLake read asks (`store.OneLakeReadAccess`), on the source item. A caller refused there gets `DENY SELECT` on the table, which wins over any role the synced security gives them. A source that no longer exists refuses | `TestAShortcutTableNeedsAccessOnTheSourceToo` |
| In delegated mode, a shortcut whose source table has row or column security "is blocked", "even if the end user has SQL permissions on the shortcut object" | **Built.** Any role at the source narrowing the path, whoever its members, blocks the shortcut for every reader — an Admin or Member included, which a `DENY` cannot do. It is reflected as a **view** that refuses every read with the reason, its columns those of the table, so a query naming one is refused for the reason and not for a missing column. Switching the endpoint to user identity mode lifts it, and back restores it | `TestDelegatedModeBlocksAShortcutWhoseSourceIsSecured`, `TestDelegatedShortcutBlock` |
| In user identity mode, the caller is evaluated against the **source's** OneLake security, including its row and column rules; "when enforcement cannot clearly validate access, the system applies the most restrictive outcome" | **Built.** The source's column allow-list is a per-principal `DENY SELECT` on each column it withholds, and its row filters are synced as roles of their own and ANDed into the table's row policy with the consumer's. A read is narrowed by both sides, and the intersection wins. A Contributor is not column-narrowed, as OneLake security does not narrow one; a Contributor in a source role that filters is filtered, as row-level security "is enforced for all users" | `TestAShortcutTableIsNarrowedByTheSourcesColumnsAndRows` |
| "Ownership chaining is disabled for tables and views involving shortcuts"; derived objects "do not inherit permissions from the object owner" | **Not modelled.** SQL Server's ownership chaining is the engine's | — |
| Producer and consumer identities must map "exactly 1:1", with no nested group resolution | The source is asked with the caller's own object id and the evaluator resolves no groups, which agrees; not separately witnessed | — |

## Where it differs, stated

- **OneLake targets only.** An ADLS Gen2, S3 or Dataverse shortcut is not reflected
  as a table: its bytes are behind an HTTP read the reflector does not make.
  That was true before this stage and stays true.
- **A blocked shortcut is a view, with our message.** In the catalog it is a view
  (`sys.views`), not a table; Fabric documents that access is blocked but not what
  a client sees, so the error — "This shortcut is blocked: its source table has
  row-level security, and a SQL analytics endpoint in delegated identity mode reads
  OneLake as the item owner…" — is ours, and reaches a client as the engine's
  conversion error carrying that text.
- **Owners are not narrowed or refused** in user identity mode. A workspace Admin or Member is `db_owner`, and
  `DENY` does not bind an owner, so they read a shortcut table whose source they
  cannot. Fabric says shortcut enforcement "can still deny access to Admins,
  Members, or Contributors" in specific cases; which cases is not documented, so
  none is modelled. A Contributor is refused (a `DENY` binds the fixed
  `db_datareader`), whereas in OneLake a Contributor's Write "overrides any
  OneLake security Read permissions" — so a Contributor with no access at the
  source is refused here and, on the documented text, may not be there.
- **Schema shortcuts** (a shortcut to a folder of tables) are not tables and are
  skipped, as a folder with no `_delta_log` always was.
- **Timing.** The refusal is set on each connection, after reflection and the
  role sync; Fabric syncs within "up to 5 minutes".
- **A column the source withholds, and a filter that reads it, refuse the reader.**
  SQL Server requires SELECT on every column a row policy takes as an argument, so
  where any filter on the table reads a column the source withholds from a reader,
  that reader's every read of the table is refused, even one that names only
  permitted columns. It fails closed and is stricter than OneLake, which narrows
  independently. It is not specific to shortcuts: the same happens with no
  shortcut when one role narrows a reader's columns and another role's filter
  reads a column outside them ([docs/60](60-sql-endpoint-access-modes.md)).
  Pinned by `TestAColumnTheSourceWithholdsAndAFilterReadsFailsClosed` and
  `TestAColumnNarrowedReaderAndAFilterOnThatColumnFailsClosed`.

## Design

Whether a shortcut is blocked is `store.DelegatedShortcutBlock`: delegated mode, a
OneLake source, and any source role narrowing the path. Reflection asks it while
listing tables (`tableSource.blocked`), and folds the answer into the table's
fingerprint, so a table whose block changed is reflected again rather than skipped
as unchanged. Every reflection first removes what an earlier one left under the
name, table or view.

Reflection lists `Tables/` and the store's shortcuts together
(`warehouse.tableSources`); a folder and a shortcut of one name cannot coexist in
OneLake, and if the store held both the folder would win. The fingerprint that
lets an unchanged reflection be skipped names the item and folder it read from, so
a shortcut re-pointed at a table whose commits are named alike is reloaded.

The per-caller refusal is the grant's `ShortcutTables`, `DeniedTables` and
`ShortcutColumns` (`oneLakeGrant`), applied by `tds.SyncShortcutAccess` after the
caller's role memberships: `DENY SELECT` for a refused table or a withheld column,
`REVOKE` (which removes a `DENY`) for everything else on a shortcut table. It runs
after reflection, which drops and recreates a table and its permissions with it.

The source's roles that cover a shortcut table are synced into the consumer's
database as `OLS_src_<hash>` roles — named from the source item and the role, so
they cannot be taken for the consumer's, and a collision refuses the sync — and
carry no permission: only who is in them, for the row predicate. The predicate
function is the AND of two layers, the consumer's roles and the source's, each
admitting a row through a role that filters it and whose filter holds, a role that
grants the table whole, or being in no role of the layer. The sync's hash covers
the source's roles, so a change at the source alone re-syncs.
