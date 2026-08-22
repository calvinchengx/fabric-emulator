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
// DELIBERATELY NOT db_owner for writers. db_owner carries CONTROL, which
// implies UNMASK and makes masking invisible — a writer would silently see
// through every masked column, and the emulator would look like it had
// implemented masking while enforcing nothing.
func principalRights(readOnly bool) []string {
	if readOnly {
		return []string{"db_datareader"}
	}
	return []string{"db_datareader", "db_datawriter", "db_ddladmin"}
}

// EnsurePrincipal makes the caller's login and database user exist, with the
// rights its workspace role implies. Idempotent: a reconnect is the normal case.
func EnsurePrincipal(ctx context.Context, master, target *sql.DB, objectID string, readOnly bool) error {
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
	for _, role := range principalRights(readOnly) {
		fmt.Fprintf(&b, "\nALTER ROLE [%s] ADD MEMBER [%s];", role, name)
	}
	if _, err := target.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("create user for %s: %w", objectID, err)
	}
	return nil
}
