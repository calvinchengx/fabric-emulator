package tds

// Per-caller database principals, so SQL Server's own security has somebody to
// restrict (docs/55-tsql-security.md).
//
// THE PROBLEM THIS SOLVES. The splice logs every client's backend connection in
// with the relay's DSN account. That account is `sa` in the default compose, so
// every caller's T-SQL runs as a sysadmin — and a sysadmin bypasses row-level
// security outright, is not restricted by a column GRANT, and holds UNMASK.
// Write a perfect security policy against that and nothing happens, which reads
// as "the feature does not work" rather than "there is no user".
//
// So the emulator gives each caller a database principal and connects as it.
// After that the features are SQL Server's, not ours: `CREATE SECURITY POLICY`,
// `GRANT SELECT ON t(col)` and `MASKED WITH` all flow through the relay
// untouched and the engine applies them.
//
// WHY A LOGIN AND NOT `EXECUTE AS`. Impersonation would have to be re-applied
// per statement on a pooled connection, and a statement that errors can leave
// the connection impersonating — the next borrower inherits someone else's
// identity. The splice gives each client its own backend connection, so the
// identity is set once, at login, and cannot drift.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
)

// principalPasswordSalt makes the derived password non-guessable from the
// object id alone. It is a local emulator credential, never a secret that
// leaves the process, and deriving beats storing: nothing to keep in sync when
// a database is recreated.
const principalPasswordSalt = "fabric-emulator/tds/principal/v1"

// principalPassword derives this caller's SQL password. Deterministic, so a
// reconnect finds the login it made last time.
func principalPassword(objectID string) string {
	sum := sha256.Sum256([]byte(principalPasswordSalt + "\x00" + objectID))
	// A leading letter and a symbol keep it valid even if a deployment turns
	// CHECK_POLICY back on.
	return "Fe1!" + base64.RawURLEncoding.EncodeToString(sum[:16])
}

// principalName is the SQL identifier for a caller.
//
// The Entra OBJECT ID, not a display name: names change and are not unique,
// and a policy that keyed on one would silently start filtering the wrong
// person. Bracket-quoted at every use, so the id needs no escaping beyond the
// one character that could close the quote.
func principalName(objectID string) string {
	return strings.ReplaceAll(objectID, "]", "]]")
}

// principalRights is what a workspace role implies inside the database.
//
// THREE RUNGS, and the middle one is the interesting part.
//
//   - A READER gets db_datareader and nothing else.
//   - A WRITER gets read, write and DDL — dbt builds a warehouse by issuing
//     CREATE TABLE — but deliberately NOT db_owner. Ownership carries CONTROL,
//     which implies UNMASK: a writer would silently see through every masked
//     column, and the emulator would look like it enforced masking while
//     enforcing nothing.
//   - An OWNER (workspace Admin or Member) gets db_owner, because somebody has
//     to be able to AUTHOR the policy. `CREATE SECURITY POLICY` needs
//     ALTER ANY SECURITY POLICY, `ADD MASKED WITH` needs ALTER ANY MASK, and
//     `GRANT`/`DENY` needs the right to grant — the first run of the e2e failed
//     on all three with "User does not have permission to perform this action".
//     That an owner also sees unmasked data is the product's shape too: they own
//     the warehouse and define what everyone else may see.
func principalRights(role Role) []string {
	switch role {
	case RoleOwner:
		return []string{"db_owner"}
	case RoleWriter:
		return []string{"db_datareader", "db_datawriter", "db_ddladmin"}
	case RoleConnect, RoleNone:
		return nil
	default:
		return []string{"db_datareader"}
	}
}

// fixedRoles are every fixed database role a rung can grant. Syncing a
// principal means checking each: the ones its rung includes are added, and the
// ones it does not are REMOVED — see EnsurePrincipal.
var fixedRoles = []string{"db_datareader", "db_datawriter", "db_ddladmin", "db_owner"}

// Role is the database-side rung a workspace role maps to.
type Role int

const (
	// RoleReader can select and nothing else.
	RoleReader Role = iota
	// RoleWriter can also insert, update and create tables.
	RoleWriter
	// RoleOwner can additionally author security policies, masks and grants.
	RoleOwner
	// RoleConnect is a database user with no role: Read on an item without
	// ReadData. "Connect to the Warehouse or SQL analytics endpoint" is what Read
	// grants, and nothing more — SQL Server refuses a SELECT unless a T-SQL GRANT
	// allows it.
	RoleConnect
	// RoleNone is no access to this database. The principal is never created
	// here, and where it already exists it loses CONNECT — which also takes away
	// any explicit GRANT an owner once authored for it, since a principal that
	// cannot enter a database cannot use what is granted inside it. Appended
	// after the existing rungs so their values do not move.
	RoleNone
)

// EnsurePrincipal makes the caller's login and database user match the rung it
// holds NOW. Idempotent: a reconnect is the normal case.
//
// SYNCED, NOT ONLY ADDED. This used to add role memberships and never remove
// one, so a Contributor demoted to Viewer kept db_datawriter, and a revoked
// ReadData grant would have kept db_datareader — access that outlived the
// permission granting it, in the direction that matters. Every fixed role the
// rung does not include is now dropped, and CONNECT is granted back explicitly
// in case a previous RoleNone revoked it.
func EnsurePrincipal(ctx context.Context, master, target *sql.DB, objectID string, role Role) error {
	if objectID == "" {
		return fmt.Errorf("no principal to provision")
	}
	name := principalName(objectID)
	quotedID := strings.ReplaceAll(objectID, "'", "''")
	if role == RoleNone {
		return revokePrincipal(ctx, target, quotedID, name)
	}
	pw := strings.ReplaceAll(principalPassword(objectID), "'", "''")

	// Server-level login, created once per engine.
	if _, err := master.ExecContext(ctx, fmt.Sprintf(`
IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'%s')
    CREATE LOGIN [%s] WITH PASSWORD = '%s', CHECK_POLICY = OFF;`,
		quotedID, name, pw)); err != nil {
		return fmt.Errorf("create login for %s: %w", objectID, err)
	}

	// Database user, created once per database; CONNECT restored; memberships
	// made exactly the rung's.
	want := map[string]bool{}
	for _, r := range principalRights(role) {
		want[r] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, `
IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'%s')
    CREATE USER [%s] FOR LOGIN [%s];
GRANT CONNECT TO [%s];`,
		quotedID, name, name, name)
	for _, r := range fixedRoles {
		if want[r] {
			fmt.Fprintf(&b, "\nALTER ROLE [%s] ADD MEMBER [%s];", r, name)
		} else {
			fmt.Fprintf(&b, "\nIF IS_ROLEMEMBER(N'%s', N'%s') = 1 ALTER ROLE [%s] DROP MEMBER [%s];", r, quotedID, r, name)
		}
	}
	if _, err := target.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("create user for %s: %w", objectID, err)
	}
	return nil
}

// revokePrincipal takes a principal's access to one database away, if it has
// any. Nothing is created: a principal that never had access needs no user in
// order not to have it. The user is kept rather than dropped, because DROP USER
// fails on a user that owns objects, and a revoke that can fail is a revoke that
// sometimes does not happen.
func revokePrincipal(ctx context.Context, target *sql.DB, quotedID, name string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "IF EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'%s')\nBEGIN", quotedID)
	for _, r := range fixedRoles {
		fmt.Fprintf(&b, "\n    IF IS_ROLEMEMBER(N'%s', N'%s') = 1 ALTER ROLE [%s] DROP MEMBER [%s];", r, quotedID, r, name)
	}
	fmt.Fprintf(&b, "\n    REVOKE CONNECT FROM [%s];\nEND", name)
	if _, err := target.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("revoke access for %s: %w", quotedID, err)
	}
	return nil
}

// SyncOneLakeMemberships makes a principal's OLS_ role memberships in one
// database exactly roles (docs/60): each listed role that exists is joined, and
// every other OLS_ role is left. A listed role the security sync has not
// created is skipped rather than failing the login — it grants nothing until it
// exists. The user must already exist, which EnsurePrincipal sees to.
func SyncOneLakeMemberships(ctx context.Context, target *sql.DB, objectID string, roles []string) error {
	if objectID == "" {
		return fmt.Errorf("no principal to sync")
	}
	quotedID := strings.ReplaceAll(objectID, "'", "''")
	name := principalName(objectID)
	var b strings.Builder
	keep := "N''"
	for _, r := range roles {
		qr := strings.ReplaceAll(r, "'", "''")
		fmt.Fprintf(&b, "IF DATABASE_PRINCIPAL_ID(N'%s') IS NOT NULL AND IS_ROLEMEMBER(N'%s', N'%s') = 0 ALTER ROLE [%s] ADD MEMBER [%s];\n",
			qr, qr, quotedID, strings.ReplaceAll(r, "]", "]]"), name)
		keep += ", N'" + qr + "'"
	}
	fmt.Fprintf(&b, `DECLARE @leave nvarchar(max) = N'';
SELECT @leave += N'ALTER ROLE ' + QUOTENAME(r.name) + N' DROP MEMBER ' + QUOTENAME(m.name) + N';'
FROM sys.database_role_members rm
JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE m.name = N'%s' AND r.name LIKE 'OLS[_]%%' AND r.name NOT IN (%s);
EXEC sp_executesql @leave;`, quotedID, keep)
	if _, err := target.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("sync OneLake role memberships for %s: %w", objectID, err)
	}
	return nil
}

// SyncShortcutAccess makes a principal's per-table refusals on an endpoint's
// shortcut tables exactly denied: each is DENY SELECT, and every other listed
// shortcut table is cleared (REVOKE removes a DENY, and a principal holds no
// table permission of its own in user identity mode). DENY wins over a GRANT
// through any role, so a OneLake role at the consumer cannot lift what the source
// refuses. A table the engine does not have yet is skipped: it is listed from the
// store, and reflection may not have run.
func SyncShortcutAccess(ctx context.Context, target *sql.DB, objectID string, tables, denied []string) error {
	if objectID == "" {
		return fmt.Errorf("no principal to sync")
	}
	if len(tables) == 0 {
		return nil
	}
	name := principalName(objectID)
	deny := map[string]bool{}
	for _, t := range denied {
		deny[t] = true
	}
	var b strings.Builder
	for _, t := range tables {
		lit := "N'[dbo].[" + strings.ReplaceAll(strings.ReplaceAll(t, "]", "]]"), "'", "''") + "]'"
		verb := "REVOKE SELECT ON [dbo].[%s] FROM [%s]"
		if deny[t] {
			verb = "DENY SELECT ON [dbo].[%s] TO [%s]"
		}
		fmt.Fprintf(&b, "IF OBJECT_ID(%s, N'U') IS NOT NULL "+verb+";\n", lit, strings.ReplaceAll(t, "]", "]]"), name)
	}
	if _, err := target.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("sync shortcut access for %s: %w", objectID, err)
	}
	return nil
}
