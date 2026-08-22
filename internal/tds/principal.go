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
	default:
		return []string{"db_datareader"}
	}
}

// Role is the database-side rung a workspace role maps to.
type Role int

const (
	// RoleReader can select and nothing else.
	RoleReader Role = iota
	// RoleWriter can also insert, update and create tables.
	RoleWriter
	// RoleOwner can additionally author security policies, masks and grants.
	RoleOwner
)

// EnsurePrincipal makes the caller's login and database user exist, with the
// rights its workspace role implies. Idempotent: a reconnect is the normal case.
func EnsurePrincipal(ctx context.Context, master, target *sql.DB, objectID string, role Role) error {
	if objectID == "" {
		return fmt.Errorf("no principal to provision")
	}
	name := principalName(objectID)
	pw := strings.ReplaceAll(principalPassword(objectID), "'", "''")

	// Server-level login, created once per engine.
	if _, err := master.ExecContext(ctx, fmt.Sprintf(`
IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'%s')
    CREATE LOGIN [%s] WITH PASSWORD = '%s', CHECK_POLICY = OFF;`,
		strings.ReplaceAll(objectID, "'", "''"), name, pw)); err != nil {
		return fmt.Errorf("create login for %s: %w", objectID, err)
	}

	// Database user, created once per database, plus its role memberships.
	var b strings.Builder
	fmt.Fprintf(&b, `
IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'%s')
    CREATE USER [%s] FOR LOGIN [%s];`,
		strings.ReplaceAll(objectID, "'", "''"), name, name)
	for _, r := range principalRights(role) {
		fmt.Fprintf(&b, "\nALTER ROLE [%s] ADD MEMBER [%s];", r, name)
	}
	if _, err := target.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("create user for %s: %w", objectID, err)
	}
	return nil
}
